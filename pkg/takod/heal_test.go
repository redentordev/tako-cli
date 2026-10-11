package takod

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/selfheal"
)

type healFixture struct {
	t       *testing.T
	dataDir string
	now     time.Time
	manager *healManager

	mu      sync.Mutex
	samples map[string]ContainerSample
	acts    []string
	actErr  error
	// stopTimeouts are the docker restart -t values passed to act.
	stopTimeouts []time.Duration
	onAct        func()
	probe        selfheal.ProbeResult
}

var (
	healWebID = strings.Repeat("a", 64)
	healAPIID = strings.Repeat("b", 64)
)

func newHealFixture(t *testing.T) *healFixture {
	t.Helper()
	f := &healFixture{t: t, dataDir: t.TempDir(), now: time.Now().UTC().Truncate(time.Second), samples: map[string]ContainerSample{}}
	m := newHealManager(f.dataDir, nil)
	m.node = func() string { return "app-1" }
	m.now = func() time.Time { return f.now }
	m.samples = func() (map[string]ContainerSample, time.Time) {
		f.mu.Lock()
		defer f.mu.Unlock()
		copied := map[string]ContainerSample{}
		for id, sample := range f.samples {
			copied[id] = sample
		}
		return copied, f.now
	}
	m.probe = func(context.Context, string, string, *HealthSpec) selfheal.ProbeResult {
		result := f.probe
		result.At = f.now
		return result
	}
	// Act as soon as the condition holds; the jitter has its own tests.
	m.jitter = func(string, string, selfheal.ServiceSpec) time.Duration { return 0 }
	m.executor.nodeLocks = func(context.Context) (func(), string) { return func() {}, "" }
	m.executor.inspect = func(_ context.Context, id string) (*healInspection, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sample, ok := f.samples[id]
		if !ok {
			return nil, errors.New("no such container")
		}
		return &healInspection{ID: sample.ID, Name: sample.Name, Status: sample.Status, Restarting: sample.Restarting,
			OOMKilled: sample.OOMKilled, ExitCode: sample.ExitCode, Health: sample.Health, RestartPolicy: sample.RestartPolicy,
			Revision: sample.Revision, Active: sample.Active, Image: sample.Image}, nil
	}
	m.executor.act = func(_ context.Context, action, id string, stopTimeout time.Duration) error {
		if f.onAct != nil {
			f.onAct()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.acts = append(f.acts, action+" "+id)
		f.stopTimeouts = append(f.stopTimeouts, stopTimeout)
		return f.actErr
	}
	m.executor.backupRunning = func(context.Context, string, string) (bool, error) { return false, nil }
	f.manager = m
	return f
}

func healTestPolicy() *selfheal.Policy {
	return &selfheal.Policy{Enabled: true, RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, WindowSeconds: 1800}
}

// writeDesired writes desired state for demo/production; each service maps
// to its desired document.
func (f *healFixture) writeDesired(services map[string]map[string]any) {
	f.t.Helper()
	path := filepath.Join(f.dataDir, "desired", "demo", "production", "revision.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"services": services})
	if err := os.WriteFile(path, data, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func healTestService(image string, extra map[string]any) map[string]any {
	service := map[string]any{"image": image, "replicas": 1, "restart": "on-failure", "selfHealing": healTestPolicy()}
	for key, value := range extra {
		service[key] = value
	}
	return service
}

func (f *healFixture) record(service, id, image string, recordedAgo time.Duration) {
	f.t.Helper()
	if err := selfheal.WriteRecord(f.dataDir, "demo", "production", service, selfheal.Record{
		Image: image, Revision: "r1", Restart: "on-failure", RecordedAt: f.now.Add(-recordedAgo),
		Containers: []selfheal.RecordContainer{{Name: "demo_production_" + service + "_1", ID: id}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *healFixture) sample(service, id, status string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples[id] = ContainerSample{
		ID: id, Name: "demo_production_" + service + "_1", Project: "demo", Environment: "production", Service: service,
		Status: status, ExitCode: exitCode, RestartPolicy: "on-failure", StartedAt: f.now.Add(-time.Hour),
		Image: "demo/" + service + ":1", Revision: "r1",
	}
}

func (f *healFixture) crashedWeb() {
	f.writeDesired(map[string]map[string]any{"web": healTestService("demo/web:1", nil)})
	f.record("web", healWebID, "demo/web:1", time.Hour)
	f.sample("web", healWebID, "exited", 1)
}

func (f *healFixture) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acts...)
}

func TestHealStartsCrashedContainerOnceAndJournals(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 1 || got[0] != "start "+healWebID {
		t.Fatalf("actions = %v, want one start", got)
	}
	journal, _ := selfheal.ReadJournal(f.dataDir, "demo", "production", 0)
	if len(journal) != 1 || journal[0].Decision != selfheal.DecisionStarted || journal[0].Reason != selfheal.ReasonExitedNonzero || journal[0].Code != 1 {
		t.Fatalf("journal = %+v", journal)
	}
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	if web := control.Lookup("web"); web == nil || len(web.Actions) != 1 || web.Containers[healWebID].Actions != 1 {
		t.Fatalf("control = %+v", control)
	}
	// Cooldown: the next tick does not act again.
	f.now = f.now.Add(healEvaluateInterval)
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 1 {
		t.Fatalf("acted inside cooldown: %v", got)
	}
	if !f.manager.active.Load() {
		t.Fatal("manager not active with a policy")
	}
}

func TestHealIdlesWithoutPolicy(t *testing.T) {
	f := newHealFixture(t)
	service := healTestService("demo/web:1", nil)
	delete(service, "selfHealing")
	f.writeDesired(map[string]map[string]any{"web": service})
	f.record("web", healWebID, "demo/web:1", time.Hour)
	f.sample("web", healWebID, "exited", 1)
	called := false
	f.manager.samples = func() (map[string]ContainerSample, time.Time) { called = true; return nil, time.Time{} }
	f.manager.Tick(context.Background())
	if f.manager.active.Load() || called || len(f.actions()) != 0 {
		t.Fatalf("idle manager worked: active=%t sampled=%t actions=%v", f.manager.active.Load(), called, f.actions())
	}
}

func TestHealNeverActsDuringDeployLease(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	response, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-1", Operation: "deploy", Who: "cli", TTLSeconds: 600})
	if err != nil || !response.Acquired {
		t.Fatalf("deploy lease: %+v %v", response, err)
	}
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("healed during a deploy lease: %v", got)
	}
	status := f.manager.Status("demo", "production", 0)
	if len(status.Services) != 1 || status.Services[0].State != selfheal.StateBlocked || status.Services[0].Skip != selfheal.SkipLeaseHeld {
		t.Fatalf("status = %+v", status.Services)
	}
	// A deploy lease taken between evaluation and action also stops it.
	if _, err := ReleaseLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-1"}); err != nil {
		t.Fatal(err)
	}
	candidate := selfheal.Candidate{Project: "demo", Environment: "production", Service: "web", Revision: "r1", Action: selfheal.ActionStart,
		Container: selfheal.RecordContainer{Name: "demo_production_web_1", ID: healWebID}, Reason: selfheal.ReasonExitedNonzero}
	if _, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-2", Operation: "deploy", Who: "cli", TTLSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	f.manager.executor.Execute(context.Background(), candidate)
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("executor acted under a foreign lease: %v", got)
	}
}

func TestHealHoldsSelfHealLeaseOnlyDuringAction(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	var during *LeaseResponse
	f.onAct = func() {
		during, _ = AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-1", Operation: "deploy", Who: "cli", TTLSeconds: 600})
	}
	f.manager.Tick(context.Background())
	if during == nil || during.Acquired || during.Lease == nil || during.Lease.Operation != healLeaseOperation {
		t.Fatalf("deploy during the action saw %+v, want a self-heal holder", during)
	}
	if !during.Lease.ExpiresAt.Before(time.Now().Add(healLeaseTTL + time.Minute)) {
		t.Fatalf("self-heal lease TTL too long: %v", during.Lease.ExpiresAt)
	}
	after, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-1", Operation: "deploy", Who: "cli", TTLSeconds: 600})
	if err != nil || !after.Acquired {
		t.Fatalf("self-heal lease not released after the action: %+v %v", after, err)
	}
}

// TestHealIntentIsDurableBeforeDockerRuns: the action counts toward the
// budget and cooldown before docker is called, so a takod crash mid-action
// cannot reset them.
func TestHealIntentIsDurableBeforeDockerRuns(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	var during *selfheal.ServiceControl
	var nodeActions int
	f.onAct = func() {
		control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
		during = control.Lookup("web")
		node, _ := selfheal.ReadNodeState(f.dataDir)
		nodeActions = len(node.Actions)
	}
	f.manager.Tick(context.Background())
	if during == nil || len(during.Actions) != 1 || during.LastAction != selfheal.DecisionPending || during.Containers[healWebID] == nil || !during.Containers[healWebID].CooldownUntil.After(f.now) || nodeActions != 1 {
		t.Fatalf("intent not persisted before docker ran: %+v (node actions %d)", during, nodeActions)
	}
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	if web := control.Lookup("web"); web == nil || len(web.Actions) != 1 || web.LastAction != selfheal.DecisionStarted {
		t.Fatalf("outcome = %+v, want one counted action recorded as started", web)
	}
}

// TestHealCrashMidActionKeepsCooldown: a takod that died after writing the
// intent comes back to a counted action: the restarted loop releases its
// stale lease and the container stays in cooldown.
func TestHealCrashMidActionKeepsCooldown(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	candidate := selfheal.Candidate{Project: "demo", Environment: "production", Service: "web", Revision: "r1", Action: selfheal.ActionStart,
		Container: selfheal.RecordContainer{Name: "demo_production_web_1", ID: healWebID}, Reason: selfheal.ReasonExitedNonzero, Window: 30 * time.Minute}
	if _, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "self-heal-crashed", Operation: healLeaseOperation, Who: "takod/app-1", TTLSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if !f.manager.executor.recordIntent(candidate) {
		t.Fatal("intent not recorded")
	}
	// takod restarts here.
	f.manager.releaseStaleLeases(context.Background())
	f.now = f.now.Add(healEvaluateInterval)
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("acted again right after a crashed action: %v", got)
	}
	if status := f.manager.Status("demo", "production", 0); status.Services[0].Skip == selfheal.SkipLeaseHeld {
		t.Fatalf("stale self-heal lease still blocks: %+v", status.Services[0])
	}
}

func TestHealReleasesOnlyOwnStaleSelfHealLeases(t *testing.T) {
	f := newHealFixture(t)
	acquire := func(environment, id, operation, who string) {
		if response, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: environment, ID: id, Operation: operation, Who: who, TTLSeconds: 300}); err != nil || !response.Acquired {
			t.Fatalf("acquire %s: %+v %v", id, response, err)
		}
	}
	acquire("production", "self-heal-own", healLeaseOperation, "takod/app-1")
	acquire("staging", "self-heal-other", healLeaseOperation, "takod/app-2")
	acquire("preview", "deploy-1", "deploy", "takod/app-1")
	f.manager.releaseStaleLeases(context.Background())
	for environment, wantHeld := range map[string]bool{"production": false, "staging": true, "preview": true} {
		path, _ := leasePath(f.dataDir, "demo", environment)
		lease, err := readLeaseFile(path)
		if err != nil || (lease != nil) != wantHeld {
			t.Fatalf("%s lease = %+v %v, want held=%t", environment, lease, err, wantHeld)
		}
	}
}

// TestHealPrunesStateOfReplacedContainersAndGoneScopes: per-container
// backoff of containers a deploy replaced, cached evaluations of destroyed
// environments and their scope locks are dropped.
func TestHealPrunesStateOfReplacedContainersAndGoneScopes(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	f.manager.Tick(context.Background())
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	if web := control.Lookup("web"); web == nil || web.Containers[healWebID] == nil {
		t.Fatalf("no backoff state after an action: %+v", control)
	}
	// A deploy replaces the container.
	replacement := strings.Repeat("d", 64)
	f.record("web", replacement, "demo/web:1", time.Hour)
	f.mu.Lock()
	delete(f.samples, healWebID)
	f.mu.Unlock()
	f.sample("web", replacement, "running", 0)
	f.now = f.now.Add(healEvaluateInterval)
	f.manager.Tick(context.Background())
	control, _ = selfheal.ReadControl(f.dataDir, "demo", "production")
	web := control.Lookup("web")
	if web == nil || web.Containers[healWebID] != nil || len(web.Actions) != 1 {
		t.Fatalf("control after replacement = %+v, want the old container pruned and the budget kept", web)
	}

	// The environment is destroyed.
	if _, ok := f.manager.lastEvals[alertScopeKey("demo", "production")]; !ok {
		t.Fatal("no cached evaluation before teardown")
	}
	if err := healClearScope(f.dataDir, "demo", "production"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.dataDir, "desired", "demo", "production")); err != nil {
		t.Fatal(err)
	}
	f.manager.Tick(context.Background())
	f.manager.statusMu.Lock()
	cached := len(f.manager.lastEvals)
	f.manager.statusMu.Unlock()
	if cached != 0 {
		t.Fatalf("cached evaluations of a destroyed environment kept: %d", cached)
	}
	if got := healScopeLockCount(); got != 0 {
		t.Fatalf("scope locks kept after use: %d", got)
	}
}

func TestHealScopeLockStillExcludes(t *testing.T) {
	unlock := lockHealScope("demo", "production")
	acquired := make(chan struct{})
	go func() {
		release := lockHealScope("demo", "production")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("second holder acquired a held scope lock")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the released scope lock")
	}
	deadline := time.Now().Add(5 * time.Second)
	for healScopeLockCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := healScopeLockCount(); got != 0 {
		t.Fatalf("scope lock entries = %d after all holders released", got)
	}
}

func TestHealRestartStopTimeoutIsBounded(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 10 * time.Second, "15s": 15 * time.Second, "60s": 30 * time.Second, "10m": 30 * time.Second, "bogus": 10 * time.Second} {
		if got := healRestartStopTimeout(value); got != want {
			t.Fatalf("healRestartStopTimeout(%q) = %s, want %s", value, got, want)
		}
	}
	if healRestartStopMax >= healDockerTimeout || healDockerTimeout+healSettleTimeout >= healLeaseTTL {
		t.Fatalf("restart bound %s, docker timeout %s and settle %s must fit the %s lease", healRestartStopMax, healDockerTimeout, healSettleTimeout, healLeaseTTL)
	}
	args := dockerHealRestartArgs(healWebID, 15*time.Second)
	if strings.Join(args, " ") != "restart -t 15 "+healWebID {
		t.Fatalf("restart args = %v", args)
	}
	if args := dockerHealRestartArgs(healWebID, 0); strings.Join(args, " ") != "restart -t 30 "+healWebID {
		t.Fatalf("unbounded restart args = %v", args)
	}
}

func TestHealActionPassesCappedStopGracePeriod(t *testing.T) {
	f := newHealFixture(t)
	f.writeDesired(map[string]map[string]any{"web": healTestService("demo/web:1", map[string]any{"stopGracePeriod": "2m"})})
	f.record("web", healWebID, "demo/web:1", time.Hour)
	f.sample("web", healWebID, "exited", 1)
	f.manager.Tick(context.Background())
	if len(f.stopTimeouts) != 1 || f.stopTimeouts[0] != healRestartStopMax {
		t.Fatalf("stop timeouts = %v, want the 30s cap", f.stopTimeouts)
	}
}

// TestHealTimedOutActionKeepsLeaseUntilSettled: when the docker call times
// out, the daemon may still be working; the self-heal lease is released
// only once a fresh inspect shows the container running again, otherwise
// it is left to expire at its TTL.
func TestHealTimedOutActionKeepsLeaseUntilSettled(t *testing.T) {
	oldTimeout, oldPoll := healSettleTimeout, healSettlePoll
	healSettleTimeout, healSettlePoll = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { healSettleTimeout, healSettlePoll = oldTimeout, oldPoll })
	candidate := selfheal.Candidate{Project: "demo", Environment: "production", Service: "web", Revision: "r1", Action: selfheal.ActionStart,
		Container: selfheal.RecordContainer{Name: "demo_production_web_1", ID: healWebID}, Reason: selfheal.ReasonExitedNonzero}
	deployLease := func(f *healFixture) *LeaseResponse {
		response, err := AcquireLease(context.Background(), f.dataDir, LeaseRequest{Project: "demo", Environment: "production", ID: "deploy-1", Operation: "deploy", Who: "cli", TTLSeconds: 600})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	// Never settles: the lease stays with self-heal.
	f := newHealFixture(t)
	f.crashedWeb()
	f.actErr = context.DeadlineExceeded
	f.manager.executor.Execute(context.Background(), candidate)
	if response := deployLease(f); response.Acquired || response.Lease == nil || response.Lease.Operation != healLeaseOperation {
		t.Fatalf("unsettled action released its lease: %+v", response)
	}
	journal, _ := selfheal.ReadJournal(f.dataDir, "demo", "production", 0)
	if len(journal) != 1 || journal[0].Result != "timeout" {
		t.Fatalf("journal = %+v", journal)
	}

	// Settles (Docker finished starting it): the lease is released.
	f = newHealFixture(t)
	f.crashedWeb()
	f.actErr = context.DeadlineExceeded
	f.onAct = func() {
		f.mu.Lock()
		sample := f.samples[healWebID]
		sample.Status, sample.ExitCode = "running", 0
		f.samples[healWebID] = sample
		f.mu.Unlock()
	}
	f.manager.executor.Execute(context.Background(), candidate)
	if response := deployLease(f); !response.Acquired {
		t.Fatalf("settled action kept its lease: %+v", response)
	}
}

func TestHealWorkerFenceBlocksAndFenceActivationWaits(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	f.manager.executor.enrolled = func() bool { return true }
	f.manager.executor.fence = f.manager.executor.defaultFence
	fencePath := filepath.Join(f.dataDir, "control", "worker-fence.json")
	if err := os.MkdirAll(filepath.Dir(fencePath), 0700); err != nil {
		t.Fatal(err)
	}
	active, _ := json.Marshal(map[string]any{"schemaVersion": 1, "active": map[string]any{"expiresAt": time.Now().Add(time.Hour)}})
	if err := os.WriteFile(fencePath, active, 0600); err != nil {
		t.Fatal(err)
	}
	candidate := selfheal.Candidate{Project: "demo", Environment: "production", Service: "web", Revision: "r1", Action: selfheal.ActionStart,
		Container: selfheal.RecordContainer{Name: "demo_production_web_1", ID: healWebID}, Reason: selfheal.ReasonExitedNonzero}
	f.manager.executor.Execute(context.Background(), candidate)
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("acted under an active worker fence: %v", got)
	}
	if err := os.Remove(fencePath); err != nil {
		t.Fatal(err)
	}
	blocked := false
	f.onAct = func() {
		// Fence activation takes workerFenceAdmissionMu exclusively; it
		// cannot proceed while the action runs.
		if workerFenceAdmissionMu.TryLock() {
			workerFenceAdmissionMu.Unlock()
		} else {
			blocked = true
		}
	}
	f.manager.executor.Execute(context.Background(), candidate)
	if got := f.actions(); len(got) != 1 || !blocked {
		t.Fatalf("actions=%v fenceBlocked=%t", got, blocked)
	}
	if !workerFenceAdmissionMu.TryLock() {
		t.Fatal("fence admission still held after the action")
	}
	workerFenceAdmissionMu.Unlock()
}

func TestHealExecutorRechecksUnderLocks(t *testing.T) {
	cases := map[string]func(f *healFixture){
		"container replaced": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.Revision = "r2"
			f.samples[healWebID] = sample
		},
		"container healthy again": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.Status, sample.ExitCode = "running", 0
			f.samples[healWebID] = sample
		},
		"stale record": func(f *healFixture) {
			f.record("web", healWebID, "demo/web:0", time.Hour)
		},
		"record converging": func(f *healFixture) {
			_ = selfheal.MarkConverging(f.dataDir, "demo", "production", "web", "demo/web:1", "r1", f.now)
		},
		"paused": func(f *healFixture) {
			_, _ = f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionPause})
		},
		"backup running": func(f *healFixture) {
			f.manager.executor.backupRunning = func(context.Context, string, string) (bool, error) { return true, nil }
		},
		"restart policy always": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.RestartPolicy = "always"
			f.samples[healWebID] = sample
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHealFixture(t)
			f.crashedWeb()
			mutate(f)
			candidate := selfheal.Candidate{Project: "demo", Environment: "production", Service: "web", Revision: "r1", Action: selfheal.ActionStart,
				Container: selfheal.RecordContainer{Name: "demo_production_web_1", ID: healWebID}, Reason: selfheal.ReasonExitedNonzero}
			f.manager.executor.Execute(context.Background(), candidate)
			if got := f.actions(); len(got) != 0 {
				t.Fatalf("acted after %s: %v", name, got)
			}
		})
	}
}

func TestHealDockerErrorIsRecordedFailure(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	f.actErr = errors.New("Error response from daemon: secret log text")
	f.manager.Tick(context.Background())
	journal, _ := selfheal.ReadJournal(f.dataDir, "demo", "production", 0)
	if len(journal) != 1 || journal[0].Decision != selfheal.DecisionFailed || journal[0].Result != "docker_error" {
		t.Fatalf("journal = %+v", journal)
	}
	data, _ := os.ReadFile(filepath.Join(f.dataDir, "heal", "demo", "production", "journal.jsonl"))
	if strings.Contains(string(data), "secret") {
		t.Fatal("docker error text reached the journal")
	}
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	if web := control.Lookup("web"); web == nil || len(web.Actions) != 1 {
		t.Fatalf("failed action not counted: %+v", control)
	}
}

func TestHealServiceExclusions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *healFixture) map[string]any
		want  string
	}{
		{"job", func(*healFixture) map[string]any { return map[string]any{"workloadKind": "job"} }, selfheal.SkipWorkloadKind},
		{"run", func(*healFixture) map[string]any { return map[string]any{"workloadKind": "run"} }, selfheal.SkipWorkloadKind},
		{"removal pending", func(*healFixture) map[string]any { return map[string]any{"removalPending": true} }, selfheal.SkipRemovalPending},
		{"scaled to zero", func(*healFixture) map[string]any { return map[string]any{"replicas": 0} }, selfheal.SkipNoLocalReplicas},
		{"assigned elsewhere", func(*healFixture) map[string]any {
			return map[string]any{"assignments": []map[string]any{{"slot": 1, "node": "app-2"}}}
		}, selfheal.SkipNoLocalReplicas},
		{"retention", func(f *healFixture) map[string]any {
			path := retainedStatePath(f.dataDir, "demo", "production", "web")
			_ = os.MkdirAll(filepath.Dir(path), 0700)
			_ = os.WriteFile(path, []byte("{}"), 0600)
			return nil
		}, selfheal.SkipRetention},
		{"maintenance", func(f *healFixture) map[string]any {
			f.samples["m"+strings.Repeat("0", 63)] = ContainerSample{ID: "m", Project: "demo", Environment: "production", Service: "web-maintenance", Status: "running"}
			return nil
		}, selfheal.SkipMaintenance},
		{"backup", func(f *healFixture) map[string]any {
			f.samples["k"+strings.Repeat("0", 63)] = ContainerSample{ID: "k", Project: "demo", Environment: "production", Service: "db", Status: "running", Role: "scheduled-backup"}
			return nil
		}, selfheal.SkipBackupRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHealFixture(t)
			f.record("web", healWebID, "demo/web:1", time.Hour)
			f.sample("web", healWebID, "exited", 1)
			extra := tc.setup(f)
			f.writeDesired(map[string]map[string]any{"web": healTestService("demo/web:1", extra)})
			f.manager.Tick(context.Background())
			if got := f.actions(); len(got) != 0 {
				t.Fatalf("acted: %v", got)
			}
			status := f.manager.Status("demo", "production", 0)
			if len(status.Services) != 1 || status.Services[0].Skip != tc.want {
				t.Fatalf("status = %+v, want skip %s", status.Services, tc.want)
			}
		})
	}
}

func TestHealSkipsStoppedMissingAndInactive(t *testing.T) {
	cases := map[string]func(f *healFixture){
		"stopped under unless-stopped": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.RestartPolicy = "unless-stopped"
			f.samples[healWebID] = sample
		},
		"exit 0 completed": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.ExitCode = 0
			f.samples[healWebID] = sample
		},
		"missing": func(f *healFixture) { delete(f.samples, healWebID) },
		"inactive blue-green": func(f *healFixture) {
			sample := f.samples[healWebID]
			sample.Active = "false"
			f.samples[healWebID] = sample
		},
		"no record":     func(f *healFixture) { _ = selfheal.ClearRecord(f.dataDir, "demo", "production", "web") },
		"recent deploy": func(f *healFixture) { f.record("web", healWebID, "demo/web:1", time.Minute) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHealFixture(t)
			f.crashedWeb()
			mutate(f)
			f.manager.Tick(context.Background())
			if got := f.actions(); len(got) != 0 {
				t.Fatalf("acted: %v", got)
			}
		})
	}
}

func TestHealPauseResumeResetAPI(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	response, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionPause})
	if err != nil || !response.Changed || response.PausedUntil == nil || !response.PausedUntil.Equal(f.now.Add(selfheal.DefaultPause)) {
		t.Fatalf("pause = %+v %v", response, err)
	}
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("acted while paused: %v", got)
	}
	if status := f.manager.Status("demo", "production", 10); status.Services[0].State != selfheal.StatePaused || len(status.Journal) == 0 {
		t.Fatalf("paused status = %+v", status)
	}
	if _, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionPause, ForSeconds: 8 * 24 * 3600}); err == nil {
		t.Fatal("pause beyond 7 days accepted")
	}
	// The node computes the end time from the duration on its own clock.
	if response, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionPause, ForSeconds: 4 * 3600}); err != nil || response.PausedUntil == nil || !response.PausedUntil.Equal(f.now.Add(4*time.Hour)) {
		t.Fatalf("pause for 4h = %+v %v", response, err)
	}
	if _, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionPause, ForSeconds: 60, Indefinite: true}); err == nil {
		t.Fatal("pause with a duration and indefinite accepted")
	}
	if response, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionResume}); err != nil || !response.Changed {
		t.Fatalf("resume = %+v %v", response, err)
	}
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 1 {
		t.Fatalf("not healed after resume: %v", got)
	}
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	control.Service("web").GaveUp = true
	_ = selfheal.WriteControl(f.dataDir, "demo", "production", control)
	if response, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionReset, Service: "web"}); err != nil || !response.Changed {
		t.Fatalf("reset = %+v %v", response, err)
	}
	if control, _ := selfheal.ReadControl(f.dataDir, "demo", "production"); control.Lookup("web") != nil {
		t.Fatalf("reset kept control: %+v", control)
	}
	if _, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: "explode"}); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestHealGivesUpAndAlerts(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.RegisterSource(healAlertSourceSpec()); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.ApplyConfig(testMonitorRequest("demo", "production")); err != nil {
		t.Fatal(err)
	}
	f := newHealFixture(t)
	f.manager.alerts = h.manager
	f.now = h.now
	f.manager.now = func() time.Time { return h.now }
	f.crashedWeb()
	// Three actions inside the window (cooldowns 2m, 4m) exhaust the budget.
	for i, wait := range []time.Duration{selfheal.Cooldown(1), selfheal.Cooldown(2), selfheal.Cooldown(3)} {
		f.manager.Tick(context.Background())
		if got := f.actions(); len(got) != i+1 {
			t.Fatalf("after tick %d actions = %v", i+1, got)
		}
		// Successful actions deliver nothing.
		if sent := h.dispatch(); len(sent) != 0 {
			t.Fatalf("successful start delivered alerts: %+v", sent)
		}
		h.advance(wait + time.Second)
		f.now = h.now
	}
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 3 {
		t.Fatalf("acted past the budget: %v", got)
	}
	control, _ := selfheal.ReadControl(f.dataDir, "demo", "production")
	if web := control.Lookup("web"); web == nil || !web.GaveUp || web.GaveUpReason != selfheal.ReasonExitedNonzero {
		t.Fatalf("not gave up: %+v", control)
	}
	found := false
	for _, delivery := range h.dispatch() {
		if delivery.Payload["event"] == healEventGaveUp {
			found = true
		}
	}
	if !found {
		t.Fatal("gave_up alert not delivered")
	}
	if status := f.manager.Status("demo", "production", 0); status.Services[0].State != selfheal.StateGaveUp {
		t.Fatalf("status = %+v", status.Services)
	}
	if _, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionReset, Service: "web"}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	f.now = h.now
	f.manager.Tick(context.Background())
	recovered := false
	for _, delivery := range h.dispatch() {
		if delivery.Payload["event"] == healEventResumed {
			recovered = true
		}
	}
	if !recovered {
		t.Fatal("reset did not recover the gave_up alert")
	}
}

func TestHealActionFailureAlerts(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.RegisterSource(healAlertSourceSpec()); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.ApplyConfig(testMonitorRequest("demo", "production")); err != nil {
		t.Fatal(err)
	}
	f := newHealFixture(t)
	f.manager.alerts = h.manager
	f.now = h.now
	f.crashedWeb()
	f.actErr = errors.New("daemon error")
	f.manager.Tick(context.Background())
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != healEventActionFailed {
		t.Fatalf("action failure deliveries = %+v", sent)
	}
}

// TestHealHoldsServiceWhenAllLocalReplicasFail: two local replicas crash
// together (a shared dependency); neither is started, the decision is
// journaled once and a replicas_unhealthy alert opens.
func TestHealHoldsServiceWhenAllLocalReplicasFail(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.RegisterSource(healAlertSourceSpec()); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.ApplyConfig(testMonitorRequest("demo", "production")); err != nil {
		t.Fatal(err)
	}
	f := newHealFixture(t)
	f.manager.alerts = h.manager
	f.now = h.now
	f.manager.now = func() time.Time { return h.now }
	f.writeDesired(map[string]map[string]any{"web": healTestService("demo/web:1", map[string]any{"replicas": 2})})
	second := strings.Repeat("c", 64)
	if err := selfheal.WriteRecord(f.dataDir, "demo", "production", "web", selfheal.Record{
		Image: "demo/web:1", Revision: "r1", RecordedAt: f.now.Add(-time.Hour),
		Containers: []selfheal.RecordContainer{{Name: "demo_production_web_1", ID: healWebID}, {Name: "demo_production_web_2", ID: second}},
	}); err != nil {
		t.Fatal(err)
	}
	f.sample("web", healWebID, "exited", 1)
	f.mu.Lock()
	sibling := f.samples[healWebID]
	sibling.ID, sibling.Name = second, "demo_production_web_2"
	f.samples[second] = sibling
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		f.manager.Tick(context.Background())
		h.advance(healEvaluateInterval)
		f.now = h.now
	}
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("started replicas while all were down: %v", got)
	}
	journal, _ := selfheal.ReadJournal(f.dataDir, "demo", "production", 0)
	if len(journal) != 1 || journal[0].Decision != selfheal.DecisionAllReplicasUnhealthy || journal[0].Service != "web" {
		t.Fatalf("journal = %+v", journal)
	}
	if status := f.manager.Status("demo", "production", 0); status.Services[0].Skip != selfheal.SkipAllReplicasUnhealthy {
		t.Fatalf("status = %+v", status.Services)
	}
	found := false
	for _, delivery := range h.dispatch() {
		if delivery.Payload["event"] == healEventReplicasUnhealthy {
			found = true
		}
	}
	if !found {
		t.Fatal("replicas_unhealthy alert not delivered")
	}
}

// TestHealDefaultJitterIsPerNode: the default jitter is derived from the
// node identity, so two nodes do not act in the same tick.
func TestHealDefaultJitterIsPerNode(t *testing.T) {
	spec := selfheal.ServiceSpec{Name: "web", Policy: healTestPolicy(), Health: selfheal.HealthHTTP, ProbeInterval: 30 * time.Second}
	seen := map[time.Duration]bool{}
	for _, node := range []string{"app-1", "app-2", "app-3", "app-4", "app-5"} {
		manager := newHealManager(t.TempDir(), nil)
		name := node
		manager.node = func() string { return name }
		seen[manager.jitter("demo", "production", spec)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every node got the same jitter: %v", seen)
	}
}

func TestHealCircuitBreakerOpensOnMassFailure(t *testing.T) {
	f := newHealFixture(t)
	f.writeDesired(map[string]map[string]any{
		"web": healTestService("demo/web:1", nil),
		"api": healTestService("demo/api:1", nil),
	})
	f.record("web", healWebID, "demo/web:1", time.Hour)
	f.record("api", healAPIID, "demo/api:1", time.Hour)
	f.sample("web", healWebID, "exited", 1)
	f.sample("api", healAPIID, "exited", 1)
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("acted during mass failure: %v", got)
	}
	node, _ := selfheal.ReadNodeState(f.dataDir)
	if !node.BreakerOpen(f.now) || node.BreakerReason != selfheal.BreakerMassFailure {
		t.Fatalf("breaker = %+v", node)
	}
	if status := f.manager.Status("demo", "production", 0); !status.Breaker.Open {
		t.Fatalf("status breaker = %+v", status.Breaker)
	}
	// Reset of the environment closes the breaker.
	if _, err := f.manager.Change(HealChangeRequest{Project: "demo", Environment: "production", Action: HealActionReset}); err != nil {
		t.Fatal(err)
	}
	if node, _ := selfheal.ReadNodeState(f.dataDir); node.BreakerOpen(f.now) {
		t.Fatal("reset left the breaker open")
	}
}

func TestHealClearedScopeOnCleanupStopsHealing(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	if err := healClearScope(f.dataDir, "demo", "production"); err != nil {
		t.Fatal(err)
	}
	f.manager.Tick(context.Background())
	if got := f.actions(); len(got) != 0 {
		t.Fatalf("healed a torn-down environment: %v", got)
	}
}

func TestHealProbeReasonCodes(t *testing.T) {
	old := containerNetworkIPFunc
	t.Cleanup(func() { containerNetworkIPFunc = old })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	healthy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	healthy.Listener = listener
	healthy.Start()
	defer healthy.Close()
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hung.Close()
	defer close(release)
	hungPort := hung.Listener.Addr().(*net.TCPAddr).Port
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer failing.Close()
	failingPort := failing.Listener.Addr().(*net.TCPAddr).Port

	ips := map[string]string{"healthy": "127.0.0.1", "hung": "127.0.0.1", "failing": "127.0.0.1", "refused": "127.0.0.1"}
	containerNetworkIPFunc = func(_ context.Context, _ string, container string) (string, error) {
		ip, ok := ips[container]
		if !ok {
			return "", errors.New("no IP")
		}
		return ip, nil
	}
	spec := &HealthSpec{Path: "/health", Port: port, Scheme: "http", Timeout: "300ms"}
	if result := probeContainerHealth(context.Background(), "net", "healthy", spec); !result.Healthy {
		t.Fatalf("healthy replica = %+v", result)
	}
	hungSpec := &HealthSpec{Path: "/health", Port: hungPort, Scheme: "http", Timeout: "300ms"}
	if result := probeContainerHealth(context.Background(), "net", "hung", hungSpec); result.Healthy || result.Reason != selfheal.ReasonHTTPTimeout {
		t.Fatalf("hung replica = %+v", result)
	}
	if result := probeContainerHealth(context.Background(), "net", "failing", &HealthSpec{Path: "/health", Port: failingPort, Scheme: "http", Timeout: "1s"}); result.Reason != selfheal.ReasonHTTPStatus || result.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing replica = %+v", result)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if result := probeContainerHealth(context.Background(), "net", "refused", &HealthSpec{Path: "/health", Port: closedPort, Scheme: "http", Timeout: "1s"}); result.Reason != selfheal.ReasonConnectFailed {
		t.Fatalf("refused replica = %+v", result)
	}
	if result := probeContainerHealth(context.Background(), "net", "refused", &HealthSpec{Port: closedPort, Timeout: "1s"}); result.Reason != selfheal.ReasonConnectFailed {
		t.Fatalf("refused tcp replica = %+v", result)
	}
	if result := probeContainerHealth(context.Background(), "net", "unknown", spec); !result.Unknown {
		t.Fatalf("unresolvable replica = %+v", result)
	}
}

func TestHealRestartsOnlyTheHungReplica(t *testing.T) {
	f := newHealFixture(t)
	f.writeDesired(map[string]map[string]any{"web": healTestService("demo/web:1", map[string]any{
		"replicas": 2, "port": 8080, "restart": "unless-stopped",
		"healthCheck": map[string]any{"path": "/health", "interval": "10s"},
	})})
	second := strings.Repeat("c", 64)
	if err := selfheal.WriteRecord(f.dataDir, "demo", "production", "web", selfheal.Record{
		Image: "demo/web:1", Revision: "r1", RecordedAt: f.now.Add(-time.Hour),
		Containers: []selfheal.RecordContainer{{Name: "demo_production_web_1", ID: healWebID}, {Name: "demo_production_web_2", ID: second}},
	}); err != nil {
		t.Fatal(err)
	}
	f.sample("web", healWebID, "running", 0)
	f.mu.Lock()
	sibling := f.samples[healWebID]
	sibling.ID, sibling.Name = second, "demo_production_web_2"
	f.samples[second] = sibling
	f.mu.Unlock()
	results := map[string]selfheal.ProbeResult{
		healWebID: {Reason: selfheal.ReasonHTTPTimeout},
		second:    {Healthy: true},
	}
	f.manager.probe = func(_ context.Context, _ string, id string, _ *HealthSpec) selfheal.ProbeResult {
		result := results[id]
		result.At = f.now
		return result
	}
	// First tick registers probe targets.
	f.manager.Tick(context.Background())
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(20 * time.Second)
		f.manager.scheduleProbes(context.Background())
		f.manager.probeWG.Wait()
		f.manager.Tick(context.Background())
	}
	got := f.actions()
	if len(got) != 1 || got[0] != "restart "+healWebID {
		t.Fatalf("actions = %v, want only the hung replica restarted", got)
	}
}

func TestHealHandlerStatusAndChange(t *testing.T) {
	f := newHealFixture(t)
	f.crashedWeb()
	server := &Server{dataDir: f.dataDir, heal: f.manager}
	put := httptest.NewRequest(http.MethodPut, "/v1/heal", strings.NewReader(`{"project":"demo","environment":"production","action":"pause","service":"web"}`))
	recorder := httptest.NewRecorder()
	server.handleHeal(recorder, put)
	var change HealChangeResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &change) != nil || !change.Changed || change.PausedUntil == nil {
		t.Fatalf("PUT = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.handleHeal(recorder, httptest.NewRequest(http.MethodGet, "/v1/heal?project=demo&environment=production&journal=5", nil))
	var status HealStatusResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &status) != nil {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if !status.Enabled || len(status.Services) != 1 || status.Services[0].State != selfheal.StatePaused || len(status.Journal) != 1 {
		t.Fatalf("status = %+v", status)
	}
	for _, bad := range []string{
		"/v1/heal?project=../x&environment=production",
		"/v1/heal?project=demo&environment=production&journal=-1",
	} {
		recorder = httptest.NewRecorder()
		server.handleHeal(recorder, httptest.NewRequest(http.MethodGet, bad, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d", bad, recorder.Code)
		}
	}
	recorder = httptest.NewRecorder()
	server.handleHeal(recorder, httptest.NewRequest(http.MethodPut, "/v1/heal", strings.NewReader(`{"project":"demo","environment":"production","action":"resume","indefinite":true}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid resume = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	(&Server{dataDir: f.dataDir}).handleHeal(recorder, httptest.NewRequest(http.MethodGet, "/v1/heal?project=demo&environment=production", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("no manager = %d", recorder.Code)
	}
}
