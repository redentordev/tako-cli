package selfheal

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testPolicy() *Policy {
	return &Policy{Enabled: true, RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, WindowSeconds: 1800}
}

// fixture is one environment with one service "web" of two replicas and a
// fake clock.
type fixture struct {
	engine  *Engine
	now     time.Time
	control *Control
	node    *NodeState
	scope   ScopeInput
	service ServiceInput
}

func newFixture(health HealthSource) *fixture {
	record := &Record{Image: "web:1", Revision: "rev1", RecordedAt: t0.Add(-time.Hour), Containers: []RecordContainer{
		{Name: "demo-web-1", ID: "id1"}, {Name: "demo-web-2", ID: "id2"},
	}}
	observations := map[string]Observation{}
	for _, container := range record.Containers {
		observations[container.ID] = Observation{
			ID: container.ID, Name: container.Name, Image: "web:1", Status: "running",
			StartedAt: t0.Add(-time.Hour), RestartPolicy: "unless-stopped", Revision: "rev1", Active: "true",
		}
	}
	f := &fixture{engine: NewEngine(), now: t0, control: &Control{}, node: &NodeState{}}
	f.service = ServiceInput{
		Spec:   ServiceSpec{Name: "web", Policy: testPolicy(), Image: "web:1", LocalReplicas: 2, Health: health, ProbeInterval: 30 * time.Second},
		Record: record, Observations: observations, ObservedAt: t0, Probes: map[string]ProbeResult{},
	}
	f.scope = ScopeInput{Project: "demo", Environment: "production", Control: f.control}
	return f
}

func (f *fixture) setObservation(id string, mutate func(*Observation)) {
	observation := f.service.Observations[id]
	mutate(&observation)
	f.service.Observations[id] = observation
}

// tick advances the clock, refreshes the sample time and evaluates.
func (f *fixture) tick(advance time.Duration) ServiceEval {
	f.now = f.now.Add(advance)
	f.service.ObservedAt = f.now
	return f.evaluate()
}

func (f *fixture) evaluate() ServiceEval {
	f.engine.Observe(f.scope.Project, f.scope.Environment, f.service, f.now)
	f.scope.Control = f.control
	f.scope.Services = []ServiceInput{f.service}
	evals := f.engine.EvaluateScope(f.scope, f.now)
	return evals[0]
}

func (f *fixture) probe(id string, healthy bool) {
	result := ProbeResult{At: f.now, Healthy: healthy}
	if !healthy {
		result.Reason, result.Code = ReasonHTTPStatus, 503
	}
	f.service.Probes[id] = result
}

func (f *fixture) failingTicks(id string, count int) ServiceEval {
	var eval ServiceEval
	for i := 0; i < count; i++ {
		if i > 0 {
			f.now = f.now.Add(30 * time.Second)
		}
		f.probe(id, false)
		f.probe(otherID(id), true)
		f.service.ObservedAt = f.now
		eval = f.evaluate()
	}
	return eval
}

func otherID(id string) string {
	if id == "id1" {
		return "id2"
	}
	return "id1"
}

func TestUnhealthyNeedsThresholdAndSpan(t *testing.T) {
	f := newFixture(HealthHTTP)
	eval := f.failingTicks("id1", 2)
	if eval.Candidate != nil || eval.State != StateWatching {
		t.Fatalf("two failures (30s) must not act: %+v", eval)
	}
	if eval.Containers[0].State != ContainerUnhealthy || eval.Containers[0].Failures != 2 || eval.Containers[0].Threshold != 3 {
		t.Fatalf("container status = %+v", eval.Containers[0])
	}
	f.now = f.now.Add(30 * time.Second)
	f.probe("id1", false)
	f.probe("id2", true)
	eval = f.tick(0)
	if eval.Candidate == nil || eval.Candidate.Action != ActionRestart || eval.Candidate.Container.ID != "id1" || eval.Candidate.Reason != ReasonHTTPStatus || eval.Candidate.Code != 503 {
		t.Fatalf("one hung replica of two must be restarted: %+v", eval.Candidate)
	}
	if eval.Containers[1].State != ContainerHealthy {
		t.Fatalf("sibling must stay healthy and untouched: %+v", eval.Containers[1])
	}
}

func TestThresholdNeedsSixtySeconds(t *testing.T) {
	f := newFixture(HealthHTTP)
	f.service.Spec.Policy.UnhealthyThreshold = 3
	// Three failures within 20s are not enough.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(10 * time.Second)
		f.probe("id1", false)
		f.probe("id2", true)
		eval := f.tick(0)
		if eval.Candidate != nil {
			t.Fatalf("failure %d within 30s acted", i+1)
		}
	}
}

func TestStaleProbeIsUnknown(t *testing.T) {
	f := newFixture(HealthHTTP)
	f.probe("id1", false)
	f.probe("id2", true)
	f.tick(0)
	// The same old failing result seen again much later must not count.
	for i := 0; i < 5; i++ {
		eval := f.tick(time.Minute)
		if eval.Candidate != nil || eval.Containers[0].Failures > 1 {
			t.Fatalf("stale probe counted: %+v", eval.Containers[0])
		}
	}
	// An explicitly unknown probe resets nothing either.
	f.service.Probes["id1"] = ProbeResult{At: f.now, Unknown: true}
	if eval := f.tick(time.Second); eval.Containers[0].Failures != 1 {
		t.Fatalf("unknown probe changed failures: %+v", eval.Containers[0])
	}
}

func TestDockerHealthSource(t *testing.T) {
	f := newFixture(HealthDocker)
	f.setObservation("id1", func(o *Observation) { o.Health = "unhealthy" })
	f.setObservation("id2", func(o *Observation) { o.Health = "healthy" })
	var eval ServiceEval
	for i := 0; i < 3; i++ {
		eval = f.tick(30 * time.Second)
	}
	if eval.Candidate == nil || eval.Candidate.Reason != ReasonDockerUnhealthy {
		t.Fatalf("docker unhealthy not restarted: %+v", eval)
	}
	// restartUnhealthy: false keeps watching.
	f2 := newFixture(HealthDocker)
	f2.service.Spec.Policy.RestartUnhealthy = false
	f2.setObservation("id1", func(o *Observation) { o.Health = "unhealthy" })
	for i := 0; i < 4; i++ {
		eval = f2.tick(30 * time.Second)
	}
	if eval.Candidate != nil {
		t.Fatalf("restartUnhealthy=false acted: %+v", eval.Candidate)
	}
}

func TestNoHealthSourceNeverRestartsRunning(t *testing.T) {
	f := newFixture(HealthNone)
	for i := 0; i < 5; i++ {
		eval := f.tick(30 * time.Second)
		if eval.Candidate != nil || eval.Containers[0].State != ContainerRunning {
			t.Fatalf("no health source: %+v", eval)
		}
	}
}

// TestClassificationTable covers each row of the D3 table.
func TestClassificationTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*Observation)
		startOff   bool
		wantState  string
		wantAction string
		wantReason string
	}{
		{name: "exit 1 under no starts", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "no" }, wantState: ContainerExited, wantAction: ActionStart, wantReason: ReasonExitedNonzero},
		{name: "exit 1 under on-failure starts", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "on-failure" }, wantState: ContainerExited, wantAction: ActionStart, wantReason: ReasonExitedNonzero},
		{name: "oom under no starts", mutate: func(o *Observation) { o.Status, o.ExitCode, o.OOMKilled, o.RestartPolicy = "exited", 137, true, "no" }, wantState: ContainerExited, wantAction: ActionStart, wantReason: ReasonOOMKilled},
		{name: "exit 0 under no completed", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 0, "no" }, wantState: ContainerCompleted},
		{name: "exit 0 under on-failure completed", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 0, "on-failure" }, wantState: ContainerCompleted},
		{name: "exited under unless-stopped stopped", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "unless-stopped" }, wantState: ContainerStopped},
		{name: "exited under always stopped", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 137, "always" }, wantState: ContainerStopped},
		{name: "created stopped", mutate: func(o *Observation) { o.Status, o.RestartPolicy = "created", "no" }, wantState: ContainerStopped},
		{name: "paused stopped", mutate: func(o *Observation) { o.Status = "paused" }, wantState: ContainerStopped},
		{name: "dead stopped", mutate: func(o *Observation) { o.Status = "dead" }, wantState: ContainerStopped},
		{name: "docker restarting", mutate: func(o *Observation) { o.Status, o.Restarting = "restarting", true }, wantState: ContainerDockerRestarting},
		{name: "startExited false", mutate: func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 2, "no" }, startOff: true, wantState: ContainerExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(HealthNone)
			f.service.Spec.Policy.StartExited = !tc.startOff
			f.setObservation("id1", tc.mutate)
			eval := f.tick(30 * time.Second)
			if eval.Containers[0].State != tc.wantState {
				t.Fatalf("state = %q, want %q", eval.Containers[0].State, tc.wantState)
			}
			action := ""
			if eval.Candidate != nil {
				action = eval.Candidate.Action
				if eval.Candidate.Reason != tc.wantReason {
					t.Fatalf("reason = %q, want %q", eval.Candidate.Reason, tc.wantReason)
				}
			}
			if action != tc.wantAction {
				t.Fatalf("action = %q, want %q", action, tc.wantAction)
			}
		})
	}
}

func TestMissingContainerIsReportedNotRecreated(t *testing.T) {
	f := newFixture(HealthNone)
	delete(f.service.Observations, "id1")
	eval := f.tick(30 * time.Second)
	if eval.Containers[0].State != ContainerMissing || eval.Candidate != nil {
		t.Fatalf("missing container = %+v candidate %+v", eval.Containers[0], eval.Candidate)
	}
	if len(eval.Journal) != 1 || eval.Journal[0].Decision != DecisionMissing {
		t.Fatalf("missing journal = %+v", eval.Journal)
	}
	if again := f.tick(30 * time.Second); len(again.Journal) != 0 {
		t.Fatalf("missing journaled twice: %+v", again.Journal)
	}
}

// TestMissingJournaledOnceAcrossForget runs the loop's Forget between ticks:
// a missing container has no observation to refresh its tracker, so the
// journaled-once state must survive Forget or the journal is rewritten with
// a `missing` line every tick.
func TestMissingJournaledOnceAcrossForget(t *testing.T) {
	f := newFixture(HealthNone)
	delete(f.service.Observations, "id1")
	journaled := 0
	for i := 0; i < 80; i++ { // 20 minutes of 15s ticks
		eval := f.tick(15 * time.Second)
		journaled += len(eval.Journal)
		f.engine.Forget(f.now.Add(-10 * time.Minute))
	}
	if journaled != 1 {
		t.Fatalf("missing container journaled %d times, want once", journaled)
	}
}

// TestOldFailuresWithoutFreshObservationNeverAct: failures counted earlier
// must not keep a container "unhealthy" (and restartable) once its health
// source stops producing observations.
func TestOldFailuresWithoutFreshObservationNeverAct(t *testing.T) {
	f := newFixture(HealthDocker)
	f.setObservation("id1", func(o *Observation) { o.Health = "unhealthy" })
	f.setObservation("id2", func(o *Observation) { o.Health = "healthy" })
	var eval ServiceEval
	for i := 0; i < 3; i++ {
		eval = f.tick(30 * time.Second)
	}
	if eval.Candidate == nil {
		t.Fatalf("fresh failures did not act: %+v", eval)
	}
	// Docker stops reporting health (e.g. status cleared); the sampler is
	// still fresh, so the service is evaluated, but the last unhealthy
	// observation grows old (more than 3 sampler intervals).
	f.setObservation("id1", func(o *Observation) { o.Health = "" })
	for i := 0; i < 4; i++ {
		eval = f.tick(30 * time.Second)
	}
	if eval.Candidate != nil || eval.NeedsAction {
		t.Fatalf("stale failures acted: %+v", eval.Candidate)
	}
	if eval.Containers[0].State != ContainerUnknown {
		t.Fatalf("container state = %q, want unknown", eval.Containers[0].State)
	}
}

func TestCompletedJournaledOnce(t *testing.T) {
	f := newFixture(HealthNone)
	f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 0, "no" })
	first := f.tick(30 * time.Second)
	second := f.tick(30 * time.Second)
	if len(first.Journal) != 1 || first.Journal[0].Decision != DecisionCompleted || len(second.Journal) != 0 {
		t.Fatalf("completed journal = %+v then %+v", first.Journal, second.Journal)
	}
}

// TestEligibilityExclusions has one case per skip reason (D4).
func TestEligibilityExclusions(t *testing.T) {
	failing := func(f *fixture) {
		f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "no" })
	}
	until := t0.Add(time.Hour)
	for _, tc := range []struct {
		name      string
		mutate    func(*fixture)
		wantState string
		wantSkip  string
	}{
		{name: "eligible baseline", mutate: func(*fixture) {}, wantState: StateWatching},
		{name: "policy disabled", mutate: func(f *fixture) { f.service.Spec.Policy = nil }, wantState: StateDisabled},
		{name: "deploy lease held", mutate: func(f *fixture) { f.scope.Skip = SkipLeaseHeld }, wantState: StateBlocked, wantSkip: SkipLeaseHeld},
		{name: "worker fence", mutate: func(f *fixture) { f.scope.Skip = SkipWorkerFence }, wantState: StateBlocked, wantSkip: SkipWorkerFence},
		{name: "node upgrade", mutate: func(f *fixture) { f.scope.Skip = SkipNodeUpgrade }, wantState: StateBlocked, wantSkip: SkipNodeUpgrade},
		{name: "pending restore", mutate: func(f *fixture) { f.scope.Skip = SkipRestorePending }, wantState: StateBlocked, wantSkip: SkipRestorePending},
		{name: "unschedulable", mutate: func(f *fixture) { f.scope.Skip = SkipNotSchedulable }, wantState: StateBlocked, wantSkip: SkipNotSchedulable},
		{name: "deny latch", mutate: func(f *fixture) { f.scope.Skip = SkipDenyLatch }, wantState: StateBlocked, wantSkip: SkipDenyLatch},
		{name: "environment paused", mutate: func(f *fixture) { f.control.AddPause("", &until, t0) }, wantState: StatePaused},
		{name: "service paused", mutate: func(f *fixture) { f.control.AddPause("web", nil, t0) }, wantState: StatePaused},
		{name: "job or run", mutate: func(f *fixture) { f.service.Skip = SkipWorkloadKind }, wantState: StateBlocked, wantSkip: SkipWorkloadKind},
		{name: "removal pending", mutate: func(f *fixture) { f.service.Skip = SkipRemovalPending }, wantState: StateBlocked, wantSkip: SkipRemovalPending},
		{name: "scaled to zero", mutate: func(f *fixture) { f.service.Skip = SkipNoLocalReplicas }, wantState: StateBlocked, wantSkip: SkipNoLocalReplicas},
		{name: "retention record", mutate: func(f *fixture) { f.service.Skip = SkipRetention }, wantState: StateBlocked, wantSkip: SkipRetention},
		{name: "backup running", mutate: func(f *fixture) { f.service.Skip = SkipBackupRunning }, wantState: StateBlocked, wantSkip: SkipBackupRunning},
		{name: "maintenance companion", mutate: func(f *fixture) { f.service.Skip = SkipMaintenance }, wantState: StateBlocked, wantSkip: SkipMaintenance},
		{name: "record missing", mutate: func(f *fixture) { f.service.Record = nil }, wantState: StateNotConverged},
		{name: "record unreadable", mutate: func(f *fixture) { f.service.RecordErr = true }, wantState: StateNotConverged, wantSkip: SkipStateUnreadable},
		{name: "record converging", mutate: func(f *fixture) { f.service.Record.Converging = true }, wantState: StateNotConverged},
		{name: "record wrong size", mutate: func(f *fixture) { f.service.Spec.LocalReplicas = 3 }, wantState: StateNotConverged},
		{name: "desired image changed", mutate: func(f *fixture) { f.service.Spec.Image = "web:2" }, wantState: StateNotConverged},
		{name: "gave up", mutate: func(f *fixture) {
			entry := f.control.Service("web")
			entry.GaveUp, entry.GaveUpRevision = true, "rev1"
		}, wantState: StateGaveUp},
		{name: "retention rename (name differs, same ID)", mutate: func(f *fixture) {
			f.setObservation("id1", func(o *Observation) { o.Name = "demo-web-1-retained" })
		}, wantState: StateNotConverged},
		{name: "revision label differs", mutate: func(f *fixture) {
			f.setObservation("id1", func(o *Observation) { o.Revision = "rev0" })
		}, wantState: StateNotConverged},
		{name: "replaced by older agent (new ID, same name)", mutate: func(f *fixture) {
			observation := f.service.Observations["id1"]
			delete(f.service.Observations, "id1")
			observation.ID = "id9"
			f.service.Observations["id9"] = observation
		}, wantState: StateNotConverged},
		{name: "stale sampler", mutate: func(f *fixture) { f.service.ObservedAt = f.now.Add(-5 * time.Minute) }, wantState: StateWatching, wantSkip: SkipStaleObservation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(HealthNone)
			failing(f)
			f.now = f.now.Add(30 * time.Second)
			f.service.ObservedAt = f.now
			tc.mutate(f)
			eval := f.evaluate()
			if eval.State != tc.wantState || eval.Skip != tc.wantSkip {
				t.Fatalf("state/skip = %q/%q, want %q/%q", eval.State, eval.Skip, tc.wantState, tc.wantSkip)
			}
			wantCandidate := tc.name == "eligible baseline"
			if (eval.Candidate != nil) != wantCandidate {
				t.Fatalf("candidate = %+v, want present=%t", eval.Candidate, wantCandidate)
			}
		})
	}
}

func TestInactiveContainerSkipped(t *testing.T) {
	f := newFixture(HealthNone)
	f.setObservation("id1", func(o *Observation) {
		o.Status, o.ExitCode, o.RestartPolicy, o.Active = "exited", 1, "no", "false"
	})
	eval := f.tick(30 * time.Second)
	if eval.Containers[0].State != ContainerInactive || eval.Candidate != nil {
		t.Fatalf("inactive container = %+v, candidate %+v", eval.Containers[0], eval.Candidate)
	}
}

func TestGracePeriods(t *testing.T) {
	crash := func(f *fixture) {
		f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "no" })
	}
	// Started one minute ago: within the 2m minimum.
	f := newFixture(HealthNone)
	crash(f)
	f.setObservation("id1", func(o *Observation) { o.StartedAt = t0.Add(-time.Minute) })
	eval := f.tick(0)
	if eval.Candidate != nil || !eval.Containers[0].Grace {
		t.Fatalf("2m start grace ignored: %+v", eval)
	}
	if eval = f.tick(61 * time.Second); eval.Candidate == nil {
		t.Fatalf("grace never ended: %+v", eval)
	}
	// startPeriod longer than 2m wins.
	f = newFixture(HealthNone)
	crash(f)
	f.service.Spec.StartPeriod = 5 * time.Minute
	f.setObservation("id1", func(o *Observation) { o.StartedAt = t0.Add(-3 * time.Minute) })
	if eval = f.tick(0); eval.Candidate != nil {
		t.Fatal("startPeriod grace ignored")
	}
	// 5m after the converged record was written (a deploy).
	f = newFixture(HealthNone)
	crash(f)
	f.service.Record.RecordedAt = t0.Add(-4 * time.Minute)
	if eval = f.tick(0); eval.Candidate != nil {
		t.Fatal("post-deploy grace ignored")
	}
	if eval = f.tick(time.Minute); eval.Candidate == nil {
		t.Fatal("post-deploy grace never ended")
	}
}

func TestCooldownDoublesAndCaps(t *testing.T) {
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := Cooldown(i + 1); got != expected {
			t.Fatalf("Cooldown(%d) = %v, want %v", i+1, got, expected)
		}
	}
}

func TestBackoffGiveUpAndClear(t *testing.T) {
	f := newFixture(HealthNone)
	f.service.Spec.LocalReplicas = 1
	f.service.Record.Containers = f.service.Record.Containers[:1]
	crash := func() {
		f.setObservation("id1", func(o *Observation) {
			o.Status, o.ExitCode, o.RestartPolicy, o.StartedAt = "exited", 1, "no", f.now.Add(-10*time.Minute)
		})
	}
	crash()
	eval := f.tick(0)
	if eval.Candidate == nil {
		t.Fatal("no first action")
	}
	RecordAction(f.control, f.node, *eval.Candidate, DecisionStarted, f.now)
	cooldown := f.control.Lookup("web").Containers["id1"].CooldownUntil
	if !cooldown.Equal(f.now.Add(2 * time.Minute)) {
		t.Fatalf("first cooldown = %v", cooldown.Sub(f.now))
	}
	// Still crashed during cooldown: no action.
	crash()
	if eval = f.tick(time.Minute); eval.Candidate != nil || eval.Containers[0].CooldownUntil == nil {
		t.Fatalf("acted during cooldown: %+v", eval)
	}
	if eval = f.tick(61 * time.Second); eval.Candidate == nil {
		t.Fatal("no second action after cooldown")
	}
	RecordAction(f.control, f.node, *eval.Candidate, DecisionStarted, f.now)
	if got := f.control.Lookup("web").Containers["id1"].CooldownUntil.Sub(f.now); got != 4*time.Minute {
		t.Fatalf("second cooldown = %v", got)
	}
	crash()
	if eval = f.tick(4*time.Minute + time.Second); eval.Candidate == nil {
		t.Fatal("no third action")
	}
	RecordAction(f.control, f.node, *eval.Candidate, DecisionStarted, f.now)
	// The fourth needed action inside the window gives up instead.
	crash()
	eval = f.tick(8*time.Minute + time.Second)
	if !eval.GiveUp || eval.Candidate != nil || eval.State != StateGaveUp {
		t.Fatalf("expected give-up after maxRestarts: %+v", eval)
	}
	if gaveUp, _ := ApplyEval(f.control, eval, f.now); !gaveUp {
		t.Fatal("ApplyEval did not give up")
	}
	if eval = f.tick(time.Hour); eval.State != StateGaveUp || eval.Candidate != nil {
		t.Fatalf("gave_up service acted: %+v", eval)
	}
	// A deploy writes a new valid record with a different revision.
	f.service.Record = &Record{Image: "web:2", Revision: "rev2", RecordedAt: f.now.Add(-time.Hour), Containers: []RecordContainer{{Name: "demo-web-1", ID: "id5"}}}
	f.service.Spec.Image = "web:2"
	f.service.Observations = map[string]Observation{"id5": {ID: "id5", Name: "demo-web-1", Status: "running", Revision: "rev2", StartedAt: f.now.Add(-time.Hour)}}
	eval = f.tick(30 * time.Second)
	if !eval.ClearGaveUp {
		t.Fatalf("new revision did not clear gave_up: %+v", eval)
	}
	if _, cleared := ApplyEval(f.control, eval, f.now); !cleared || f.control.Lookup("web") != nil {
		t.Fatal("clear not applied")
	}
}

func TestPruneContainersKeepsOnlyRecordedIDs(t *testing.T) {
	f := newFixture(HealthNone)
	RecordAction(f.control, f.node, Candidate{Service: "web", Container: RecordContainer{ID: "old"}, Window: 30 * time.Minute}, DecisionStarted, f.now)
	RecordAction(f.control, f.node, Candidate{Service: "web", Container: RecordContainer{ID: "id1"}, Window: 30 * time.Minute}, DecisionStarted, f.now)
	eval := f.tick(30 * time.Second)
	if !PruneContainers(f.control, eval) {
		t.Fatal("nothing pruned")
	}
	web := f.control.Lookup("web")
	if web.Containers["old"] != nil || web.Containers["id1"] == nil || len(web.Actions) != 2 {
		t.Fatalf("pruned control = %+v", web)
	}
	// A converging record names no IDs: nothing is pruned meanwhile.
	f.service.Record.Converging = true
	if PruneContainers(f.control, f.tick(30*time.Second)) || web.Containers["id1"] == nil {
		t.Fatal("pruned against a converging record")
	}
}

func TestRecordOutcomeDoesNotCountAgain(t *testing.T) {
	control, node := &Control{}, &NodeState{}
	candidate := Candidate{Service: "web", Container: RecordContainer{ID: "id1"}, Window: 30 * time.Minute}
	RecordAction(control, node, candidate, DecisionPending, t0)
	if !RecordOutcome(control, candidate, DecisionRestarted) {
		t.Fatal("outcome not applied")
	}
	web := control.Lookup("web")
	if web.LastAction != DecisionRestarted || len(web.Actions) != 1 || web.Containers["id1"].Actions != 1 || len(node.Actions) != 1 {
		t.Fatalf("outcome counted again: %+v node %+v", web, node)
	}
	if RecordOutcome(control, candidate, DecisionFailed) {
		t.Fatal("outcome overwrote a final decision")
	}
}

func TestHealthyWindowResetsBackoff(t *testing.T) {
	f := newFixture(HealthNone)
	RecordAction(f.control, f.node, Candidate{Service: "web", Container: RecordContainer{ID: "id1"}, Window: 30 * time.Minute}, DecisionStarted, f.now)
	RecordAction(f.control, f.node, Candidate{Service: "web", Container: RecordContainer{ID: "id1"}, Window: 30 * time.Minute}, DecisionStarted, f.now)
	f.tick(0)
	eval := f.tick(29 * time.Minute)
	if len(eval.ResetContainers) != 0 {
		t.Fatal("reset before a full healthy window")
	}
	eval = f.tick(2 * time.Minute)
	if len(eval.ResetContainers) != 1 || eval.ResetContainers[0] != "id1" {
		t.Fatalf("reset after healthy window = %v", eval.ResetContainers)
	}
	ApplyEval(f.control, eval, f.now)
	if container := f.control.Lookup("web").Containers["id1"]; container.Actions != 0 || !container.CooldownUntil.IsZero() {
		t.Fatalf("backoff not reset: %+v", container)
	}
}

// addReplica adds a third running replica id3 to the fixture.
func (f *fixture) addReplica() {
	f.service.Record.Containers = append(f.service.Record.Containers, RecordContainer{Name: "demo-web-3", ID: "id3"})
	f.service.Spec.LocalReplicas = len(f.service.Record.Containers)
	f.service.Observations["id3"] = Observation{
		ID: "id3", Name: "demo-web-3", Image: "web:1", Status: "running",
		StartedAt: t0.Add(-time.Hour), RestartPolicy: "unless-stopped", Revision: "rev1", Active: "true",
	}
}

func TestSerialReplicaActions(t *testing.T) {
	f := newFixture(HealthHTTP)
	f.addReplica()
	f.probe("id3", true)
	f.failingTicks("id1", 3)
	// Two of three replicas hang.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(30 * time.Second)
		f.probe("id1", false)
		f.probe("id2", false)
		f.probe("id3", true)
		f.tick(0)
	}
	eval := f.tick(0)
	if eval.Candidate == nil || eval.Candidate.Container.ID != "id1" {
		t.Fatalf("first candidate = %+v", eval.Candidate)
	}
	RecordAction(f.control, f.node, *eval.Candidate, DecisionRestarted, f.now)
	// id1 has not proven healthy yet and is in cooldown: id2 waits.
	f.now = f.now.Add(30 * time.Second)
	f.probe("id2", false)
	eval = f.tick(0)
	if eval.Candidate != nil {
		t.Fatalf("second replica acted before the first recovered: %+v", eval.Candidate)
	}
	// id1 healthy again: id2 may proceed.
	f.now = f.now.Add(30 * time.Second)
	f.probe("id1", true)
	f.probe("id2", false)
	eval = f.tick(0)
	if eval.Candidate == nil || eval.Candidate.Container.ID != "id2" {
		t.Fatalf("second replica blocked after first recovered: %+v", eval.Candidate)
	}
}

// TestAllLocalReplicasUnhealthyHoldsService: when every local replica of a
// multi-replica service needs an action, the cause is shared, so nothing is
// restarted; the service is held, journaled once and reported.
func TestAllLocalReplicasUnhealthyHoldsService(t *testing.T) {
	f := newFixture(HealthHTTP)
	var eval ServiceEval
	journaled := 0
	for i := 0; i < 6; i++ {
		f.now = f.now.Add(30 * time.Second)
		f.probe("id1", false)
		f.probe("id2", false)
		eval = f.tick(0)
		journaled += len(eval.Journal)
		if eval.Candidate != nil {
			t.Fatalf("tick %d restarted a replica while all were unhealthy: %+v", i, eval.Candidate)
		}
	}
	if eval.State != StateBlocked || eval.Skip != SkipAllReplicasUnhealthy || !eval.NeedsAction || !eval.Eligible || eval.GiveUp {
		t.Fatalf("eval = %+v", eval)
	}
	if journaled != 1 {
		t.Fatalf("all_replicas_unhealthy journaled %d times, want once", journaled)
	}
	// One replica recovers: the other is healed normally.
	f.now = f.now.Add(30 * time.Second)
	f.probe("id1", false)
	f.probe("id2", true)
	if eval = f.tick(0); eval.Candidate == nil || eval.Candidate.Container.ID != "id1" {
		t.Fatalf("partial failure not healed: %+v", eval)
	}
}

// TestSingleReplicaStillHealed: with one local replica there is nothing to
// compare against, so the all-replicas rule does not apply.
func TestSingleReplicaStillHealed(t *testing.T) {
	f := newFixture(HealthHTTP)
	f.service.Spec.LocalReplicas = 1
	f.service.Record.Containers = f.service.Record.Containers[:1]
	var eval ServiceEval
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(30 * time.Second)
		f.probe("id1", false)
		eval = f.tick(0)
	}
	if eval.Candidate == nil || eval.Skip != "" {
		t.Fatalf("single replica not healed: %+v", eval)
	}
}

// TestJitterDelaysAction: an action waits for the service's jitter after
// its condition first holds past grace.
func TestJitterDelaysAction(t *testing.T) {
	f := newFixture(HealthNone)
	f.service.Spec.Jitter = 45 * time.Second
	f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode, o.RestartPolicy = "exited", 1, "no" })
	if eval := f.tick(15 * time.Second); eval.Candidate != nil || !eval.Containers[0].Grace || eval.NeedsAction {
		t.Fatalf("acted before jitter: %+v", eval)
	}
	if eval := f.tick(30 * time.Second); eval.Candidate != nil {
		t.Fatalf("acted 30s into a 45s jitter: %+v", eval.Candidate)
	}
	if eval := f.tick(15 * time.Second); eval.Candidate == nil {
		t.Fatalf("no action after jitter: %+v", eval)
	}
	// The condition clearing resets the clock.
	f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode = "running", 0 })
	f.tick(15 * time.Second)
	f.setObservation("id1", func(o *Observation) { o.Status, o.ExitCode = "exited", 1 })
	if eval := f.tick(15 * time.Second); eval.Candidate != nil {
		t.Fatalf("jitter clock not reset: %+v", eval.Candidate)
	}
}

func TestServiceJitterSpreadsNodes(t *testing.T) {
	spec := ServiceSpec{Name: "web", Policy: testPolicy(), Health: HealthHTTP, ProbeInterval: 30 * time.Second}
	seen := map[time.Duration]bool{}
	for _, node := range []string{"node-a", "node-b", "node-c", "node-d", "node-e", "node-f"} {
		jitter := ServiceJitter(node, "demo", "production", spec)
		if jitter < 0 || jitter >= 90*time.Second {
			t.Fatalf("jitter %v outside [0, 90s)", jitter)
		}
		if again := ServiceJitter(node, "demo", "production", spec); again != jitter {
			t.Fatalf("jitter not deterministic: %v vs %v", jitter, again)
		}
		seen[jitter] = true
	}
	if len(seen) < 3 {
		t.Fatalf("jitter does not spread nodes: %v", seen)
	}
	spec.ProbeInterval = time.Hour
	if jitter := ServiceJitter("node-a", "demo", "production", spec); jitter >= MaxJitter {
		t.Fatalf("jitter %v not capped", jitter)
	}
}

func TestCircuitBreaker(t *testing.T) {
	needing := ServiceEval{Eligible: true, NeedsAction: true, Candidate: &Candidate{Service: "a"}}
	healthy := ServiceEval{Eligible: true}
	node := &NodeState{}
	// More than half of the eligible services need an action.
	decision := DecideNode([]ServiceEval{needing, needing, healthy}, node, t0)
	if decision.OpenBreaker != BreakerMassFailure || decision.Pick != nil {
		t.Fatalf("mass failure decision = %+v", decision)
	}
	// Exactly half does not trip; one action is picked.
	decision = DecideNode([]ServiceEval{needing, healthy}, node, t0)
	if decision.OpenBreaker != "" || decision.Pick == nil {
		t.Fatalf("half failing decision = %+v", decision)
	}
	// A single eligible service never trips the mass-failure rule.
	if decision = DecideNode([]ServiceEval{needing}, node, t0); decision.Pick == nil {
		t.Fatalf("single service decision = %+v", decision)
	}
	// Six actions in ten minutes: the next one opens the breaker.
	for i := 0; i < BreakerMaxActions; i++ {
		RecordAction(&Control{}, node, Candidate{Service: "a", Container: RecordContainer{ID: "x"}}, DecisionRestarted, t0.Add(time.Duration(i)*time.Minute))
	}
	decision = DecideNode([]ServiceEval{needing, healthy}, node, t0.Add(6*time.Minute))
	if decision.OpenBreaker != BreakerActionRate || decision.Pick != nil {
		t.Fatalf("action rate decision = %+v", decision)
	}
	node.OpenBreaker(BreakerActionRate, t0.Add(6*time.Minute))
	if decision = DecideNode([]ServiceEval{needing, healthy}, node, t0.Add(20*time.Minute)); decision.Pick != nil {
		t.Fatalf("acted while breaker open: %+v", decision)
	}
	decision = DecideNode([]ServiceEval{needing, healthy}, node, t0.Add(36*time.Minute))
	if !decision.CloseBreaker || decision.Pick == nil {
		t.Fatalf("breaker did not close after 30m: %+v", decision)
	}
}
