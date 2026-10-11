package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/recovery"
)

const (
	testSlackTarget   = "https://hooks.slack.com/services/example"
	testWebhookTarget = "https://alerts.example.invalid/hook"
	testServiceTarget = "https://service-alerts.example.invalid/web"
)

type capturedDelivery struct {
	Provider string
	Endpoint string
	Payload  map[string]any
}

type sourceTestHarness struct {
	t       *testing.T
	manager *SourceAlertManager
	now     time.Time
	mu      sync.Mutex
	sent    []capturedDelivery
	result  alertDeliveryResult
}

func newSourceTestHarness(t *testing.T, dataDir string) *sourceTestHarness {
	t.Helper()
	h := &sourceTestHarness{t: t, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), result: alertDeliveryResult{Delivered: true}}
	h.manager = NewSourceAlertManager(dataDir)
	h.manager.SetNodeName("app-1")
	h.manager.now = func() time.Time { return h.now }
	h.manager.deliver = func(_ context.Context, provider, endpoint string, payload map[string]any) alertDeliveryResult {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sent = append(h.sent, capturedDelivery{Provider: provider, Endpoint: endpoint, Payload: payload})
		return h.result
	}
	if err := registerBuiltinAlertSources(h.manager); err != nil {
		t.Fatalf("register: %v", err)
	}
	return h
}

func (h *sourceTestHarness) dispatch() []capturedDelivery {
	h.t.Helper()
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	h.manager.DispatchDue(context.Background())
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]capturedDelivery(nil), h.sent...)
}

func (h *sourceTestHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *sourceTestHarness) level(project, environment, kind, subject string, failing ...string) {
	h.t.Helper()
	if err := h.manager.ReportLevel(LevelObservation{
		Project: project, Environment: environment, Kind: kind, Subject: subject, Failing: failing,
		Damping: Damping{OpenAfter: 1, RecoverAfter: 1}, ObservedAt: h.now,
	}); err != nil {
		h.t.Fatalf("ReportLevel: %v", err)
	}
}

func testMonitorRequest(project, environment string) AlertConfigRequest {
	return AlertConfigRequest{
		Project: project, Environment: environment,
		Targets: AlertTargets{Slack: testSlackTarget},
		Monitors: []MonitorSpec{{
			Service: "web", CheckType: MonitorCheckHTTP, IntervalSeconds: 60, TimeoutSeconds: 10,
			AlertAfter: 3, RecoverAfter: 2, Domain: "shop.example.com", HealthPath: "/health", CheckerNode: "app-1",
		}},
		Host:         &HostAlertPolicy{DiskPercent: 90},
		Containers:   &ContainerAlertPolicy{OOM: true, CrashLoopRestarts: 3, CrashLoopWindowSeconds: 600},
		Certificates: &CertificateAlertPolicy{WarnBeforeSeconds: 14 * 86400, CriticalBeforeSeconds: 3 * 86400},
	}
}

func TestRegisterSourceEnforcesKindTable(t *testing.T) {
	manager := NewSourceAlertManager(t.TempDir())
	valid := AlertSourceSpec{
		Kind: AlertKindSelfHeal, Reasons: map[string]Severity{"gave_up": SeverityCritical},
		Events: map[string]string{"gave_up": "heal_gave_up"}, Recoveries: map[string]string{"gave_up": "heal_resumed"},
		DetailKeys: []string{"attempts"},
	}
	cases := map[string]func(AlertSourceSpec) AlertSourceSpec{
		"unknown kind":        func(s AlertSourceSpec) AlertSourceSpec { s.Kind = "disk"; return s },
		"node scope mismatch": func(s AlertSourceSpec) AlertSourceSpec { s.NodeScoped = true; return s },
		"info severity": func(s AlertSourceSpec) AlertSourceSpec {
			s.Reasons = map[string]Severity{"gave_up": "info"}
			return s
		},
		"missing event":   func(s AlertSourceSpec) AlertSourceSpec { s.Events = nil; return s },
		"reserved detail": func(s AlertSourceSpec) AlertSourceSpec { s.DetailKeys = []string{"repeats"}; return s },
		"dotted reason": func(s AlertSourceSpec) AlertSourceSpec {
			s.Reasons = map[string]Severity{"heal.gave": SeverityCritical}
			return s
		},
		"recovery w/o reason": func(s AlertSourceSpec) AlertSourceSpec {
			s.Recoveries = map[string]string{"other": "x"}
			return s
		},
	}
	for name, mutate := range cases {
		if err := manager.RegisterSource(mutate(valid)); err == nil {
			t.Errorf("%s: expected RegisterSource to fail", name)
		}
	}
	if err := manager.RegisterSource(valid); err != nil {
		t.Fatalf("valid sibling spec rejected: %v", err)
	}
	if err := manager.RegisterSource(valid); err == nil {
		t.Fatalf("duplicate kind accepted")
	}
	node := AlertSourceSpec{Kind: AlertKindNode, NodeScoped: true, Reasons: map[string]Severity{"reboot_required": SeverityWarning}, Events: map[string]string{"reboot_required": "node_reboot_required"}}
	if err := manager.RegisterSource(node); err != nil {
		t.Fatalf("node kind rejected: %v", err)
	}
}

func TestReportLevelValidatesVocabulary(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	bad := []LevelObservation{
		{Project: "shop", Environment: "production", Kind: "selfheal", Subject: "web"},
		{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Failing: []string{"exploded"}},
		{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Details: map[string]string{"error": "x"}},
		{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Details: map[string]string{"errorClass": "dial tcp 10.0.0.1:443: connection refused"}},
		{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Failing: []string{"unreachable"}, Severities: map[string]Severity{"unreachable": "info"}},
		{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web.example"},
		{Kind: AlertKindService, Subject: "web"},
	}
	for index, observation := range bad {
		if err := h.manager.ReportLevel(observation); err == nil {
			t.Errorf("observation %d accepted", index)
		}
	}
	// Unsubscribed scopes are dropped silently.
	if err := h.manager.ReportLevel(LevelObservation{Project: "other", Environment: "production", Kind: AlertKindService, Subject: "web", Failing: []string{"unreachable"}}); err != nil {
		t.Fatalf("unsubscribed report returned error: %v", err)
	}
	if status, _ := h.manager.Status("other", "production"); len(status.Subjects) != 0 {
		t.Fatalf("unsubscribed scope recorded subjects: %+v", status.Subjects)
	}
}

func TestServiceEpisodeDampingAndPayload(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	report := func(failing ...string) {
		if err := h.manager.ReportLevel(LevelObservation{
			Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Failing: failing,
			Details: map[string]string{"errorClass": "connect"}, Damping: Damping{OpenAfter: 3, RecoverAfter: 2}, ObservedAt: h.now,
		}); err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
	}
	report("unreachable")
	report("unreachable")
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("opened before alertAfter: %d", len(sent))
	}
	report("unreachable")
	sent := h.dispatch()
	if len(sent) != 1 {
		t.Fatalf("want one opening, got %d", len(sent))
	}
	payload := sent[0].Payload
	for key, want := range map[string]any{
		"schemaVersion": AlertPayloadSchemaVersion, "transition": "opening", "event": "service_down", "node": "app-1",
		"kind": "service", "subject": "web", "service": "web", "severity": "critical",
	} {
		if payload[key] != want {
			t.Errorf("payload[%s] = %v, want %v", key, payload[key], want)
		}
	}
	if sent[0].Provider != "slack" {
		t.Fatalf("provider = %s", sent[0].Provider)
	}
	status, _ := h.manager.Status("shop", "production")
	if len(status.Subjects) != 1 || status.Subjects[0].State != SubjectStateAlerting {
		t.Fatalf("status = %+v", status.Subjects)
	}
	report()
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("recovered before recoverAfter")
	}
	report()
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["transition"] != "recovery" || sent[0].Payload["event"] != "service_up" {
		t.Fatalf("recovery = %+v", sent)
	}
	if sent[0].Payload["episodeId"] != payload["episodeId"] {
		t.Fatalf("recovery episode mismatch")
	}
}

func TestPayloadsCarryNoTargetsOrErrorText(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.ServiceTargets = map[string]string{"web": testServiceTarget}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.result = alertDeliveryResult{Retryable: true, Error: "network"}
	h.dispatch()
	status, _ := h.manager.Status("shop", "production")
	data, _ := json.Marshal(status)
	for _, secret := range []string{testSlackTarget, testServiceTarget, "hooks.slack.com", "example.invalid"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("status leaks target %q: %s", secret, data)
		}
	}
	h.result = alertDeliveryResult{Delivered: true}
	h.advance(10 * time.Second)
	for _, delivery := range h.dispatch() {
		encoded, _ := json.Marshal(delivery.Payload)
		if strings.Contains(string(encoded), "example.invalid") || strings.Contains(string(encoded), "hooks.slack") {
			t.Fatalf("payload leaks target: %s", encoded)
		}
	}
}

func TestPerServiceWebhookRouting(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.ServiceTargets = map[string]string{"web": testServiceTarget}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.level("shop", "production", AlertKindHost, "root", "disk_high")
	endpoints := map[string][]string{}
	for _, delivery := range h.dispatch() {
		subject := delivery.Payload["subject"].(string)
		endpoints[subject] = append(endpoints[subject], delivery.Endpoint)
	}
	if len(endpoints["web"]) != 2 {
		t.Fatalf("service subject should reach shared and service targets: %v", endpoints["web"])
	}
	if len(endpoints["root"]) != 1 || endpoints["root"][0] != testSlackTarget {
		t.Fatalf("host subject must not reach the service webhook: %v", endpoints["root"])
	}
}

func TestSeverityEscalation(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	report := func(severity Severity) {
		if err := h.manager.ReportLevel(LevelObservation{
			Project: "shop", Environment: "production", Kind: AlertKindCertificate, Subject: "shop.example.com",
			Failing: []string{"cert_expiring"}, Severities: map[string]Severity{"cert_expiring": severity}, ObservedAt: h.now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	report(SeverityWarning)
	if sent := h.dispatch(); len(sent) != 1 || sent[0].Payload["severity"] != "warning" {
		t.Fatalf("opening = %+v", sent)
	}
	report(SeverityWarning)
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("repeat warning delivered")
	}
	report(SeverityCritical)
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["transition"] != "escalation" || sent[0].Payload["severity"] != "critical" {
		t.Fatalf("escalation = %+v", sent)
	}
}

func TestFlapSuppression(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			h.level("shop", "production", AlertKindService, "web", "unreachable")
		} else {
			h.level("shop", "production", AlertKindService, "web")
		}
		for _, delivery := range h.dispatch() {
			transitions = append(transitions, delivery.Payload["transition"].(string))
		}
		h.advance(time.Minute)
	}
	if len(transitions) != 7 || transitions[6] != AlertTransitionSuppressed {
		t.Fatalf("transitions = %v", transitions)
	}
	status, _ := h.manager.Status("shop", "production")
	if status.Subjects[0].State != SubjectStateFlapping {
		t.Fatalf("state = %s", status.Subjects[0].State)
	}
	// Stable (failing) for 30 minutes: the current state is sent once.
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.advance(31 * time.Minute)
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	sent := h.dispatch()
	if len(sent) != 1 {
		t.Fatalf("after stabilizing want one delivery, got %d", len(sent))
	}
}

func TestEventCooldownCountsRepeats(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	event := func() {
		if err := h.manager.ReportEvent(EventObservation{Project: "shop", Environment: "production", Kind: AlertKindContainer, Subject: "web", Reason: "oom", Details: map[string]string{"exitCode": "137"}, ObservedAt: h.now}); err != nil {
			t.Fatal(err)
		}
	}
	event()
	if sent := h.dispatch(); len(sent) != 1 || sent[0].Payload["event"] != "container_oom" || sent[0].Payload["transition"] != "event" {
		t.Fatalf("first event = %+v", sent)
	}
	h.advance(time.Minute)
	event()
	event()
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("event inside cooldown delivered")
	}
	h.advance(15 * time.Minute)
	event()
	sent := h.dispatch()
	if len(sent) != 1 {
		t.Fatalf("event after cooldown: %d", len(sent))
	}
	details := sent[0].Payload["details"].(map[string]string)
	if details["repeats"] != "2" {
		t.Fatalf("repeats = %q", details["repeats"])
	}
}

func TestNodeScopedFanOutAndLedgerDeduplication(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	for _, environment := range []string{"production", "staging"} {
		if err := h.manager.ApplyConfig(testMonitorRequest("shop", environment)); err != nil {
			t.Fatal(err)
		}
	}
	// Empty scope fans out to every subscribing scope.
	if err := h.manager.ReportLevel(LevelObservation{Kind: AlertKindHost, Subject: "root", Failing: []string{"disk_high"}, ObservedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	sent := h.dispatch()
	if len(sent) != 1 {
		t.Fatalf("shared endpoint should receive one opening, got %d", len(sent))
	}
	for _, environment := range []string{"production", "staging"} {
		status, _ := h.manager.Status("shop", environment)
		if len(status.Subjects) != 1 || status.Subjects[0].State != SubjectStateAlerting {
			t.Fatalf("%s status = %+v", environment, status.Subjects)
		}
	}
	h.advance(time.Minute)
	if err := h.manager.ReportLevel(LevelObservation{Kind: AlertKindHost, Subject: "root", ObservedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["transition"] != "recovery" {
		t.Fatalf("one coherent recovery expected: %+v", sent)
	}
}

func TestRetireSendsRetiredRecovery(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.dispatch()
	if err := h.manager.Retire("shop", "production", AlertKindService, "web"); err != nil {
		t.Fatal(err)
	}
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["transition"] != "recovery" || sent[0].Payload["details"].(map[string]string)["retired"] != "true" {
		t.Fatalf("retire = %+v", sent)
	}
	if subjects := h.manager.Subjects("shop", "production", AlertKindService); len(subjects) != 0 {
		t.Fatalf("subject not forgotten: %v", subjects)
	}
}

func TestTargetChangeRecoversRemovedAndOpensAdded(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.dispatch()
	request.Targets = AlertTargets{Webhook: testWebhookTarget}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, delivery := range h.dispatch() {
		got[delivery.Provider] = delivery.Payload["transition"].(string)
	}
	if got["slack"] != "recovery" || got["webhook"] != "opening" {
		t.Fatalf("target change deliveries = %v", got)
	}
}

func TestHoldAndStaleStates(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.ReportLevel(LevelObservation{Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Hold: SubjectHoldPaused, HoldReason: "operation", ObservedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	status, _ := h.manager.Status("shop", "production")
	if status.Subjects[0].State != SubjectStatePaused || !status.Subjects[0].Paused || status.Subjects[0].Reasons[0] != "operation" {
		t.Fatalf("paused status = %+v", status.Subjects[0])
	}
	h.level("shop", "production", AlertKindService, "web")
	h.advance(4 * time.Minute)
	status, _ = h.manager.Status("shop", "production")
	if status.Subjects[0].State != SubjectStateStale {
		t.Fatalf("state = %s, want stale", status.Subjects[0].State)
	}
}

func TestSourceStatePersistsAndSurvivesUnknownKinds(t *testing.T) {
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, dataDir)
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.dispatch()
	h.manager.flush(context.Background())

	reloaded := newSourceTestHarness(t, dataDir)
	if err := reloaded.manager.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	status, _ := reloaded.manager.Status("shop", "production")
	if len(status.Subjects) != 1 || status.Subjects[0].State != SubjectStateAlerting {
		t.Fatalf("reloaded status = %+v", status.Subjects)
	}
	// Recovery after restart goes to the target that got the opening.
	reloaded.level("shop", "production", AlertKindService, "web")
	if sent := reloaded.dispatch(); len(sent) != 1 || sent[0].Payload["transition"] != "recovery" {
		t.Fatalf("recovery after reload = %+v", sent)
	}

	// A subject of a kind nobody registers is dropped, not fatal.
	statePath := filepath.Join(dataDir, "alerts", "state", "shop", "production.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	_ = json.Unmarshal(data, &state)
	state["subjects"].(map[string]any)["future/x"] = map[string]any{"kind": "future", "subject": "x"}
	data, _ = json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// A corrupt second scope is skipped without failing startup.
	corrupt := filepath.Join(dataDir, "alerts", "config", "shop", "broken.json")
	if err := os.WriteFile(corrupt, []byte(`{"schemaVersion":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	third := newSourceTestHarness(t, dataDir)
	if err := third.manager.Load(context.Background()); err != nil {
		t.Fatalf("Load with unknown kind/corrupt scope failed: %v", err)
	}
	if !third.manager.Configured("shop", "production") || third.manager.Configured("shop", "broken") {
		t.Fatalf("unexpected configured scopes")
	}
	if _, err := os.Stat(filepath.Join(dataDir, schedulerAlertsDirName)); !os.IsNotExist(err) {
		t.Fatalf("source manager must not touch scheduler-alerts storage")
	}
}

func TestSnapshotLockDoesNotBlockAlertReporters(t *testing.T) {
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, dataDir)
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	statePath := filepath.Join(dataDir, "alerts", "state", "shop", "production.json")
	_ = os.Remove(statePath)
	// A platform backup or upgrade holds the snapshot lock.
	unlockSnapshot, err := recovery.AcquireSnapshotLock(dataDir)
	if err != nil {
		t.Fatalf("snapshot lock: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlockSnapshot()
		}
	}
	defer release()
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushed := make(chan struct{})
	go func() {
		h.manager.flush(flushCtx)
		close(flushed)
	}()
	time.Sleep(100 * time.Millisecond)
	reported := make(chan error, 1)
	go func() {
		err := h.manager.ReportLevel(LevelObservation{
			Project: "shop", Environment: "production", Kind: AlertKindService, Subject: "web", Failing: []string{"unreachable"},
			Damping: Damping{OpenAfter: 1, RecoverAfter: 1}, ObservedAt: h.now,
		})
		if err == nil {
			_, err = h.manager.Status("shop", "production")
		}
		reported <- err
	}()
	select {
	case err := <-reported:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReportLevel/Status blocked behind a flush waiting on the snapshot lock")
	}
	<-flushed
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("flush wrote state while the snapshot lock was held: %v", err)
	}
	// Once the snapshot lock is released the deferred state is persisted.
	release()
	h.manager.flush(context.Background())
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state not persisted after the snapshot lock was released: %v", err)
	}
}

func TestRemoveScopeClearsTree(t *testing.T) {
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, dataDir)
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	h.level("shop", "production", AlertKindService, "web", "unreachable")
	h.dispatch()
	if err := h.manager.RemoveScope("shop", "production"); err != nil {
		t.Fatal(err)
	}
	if sent := h.dispatch(); len(sent) != 1 || sent[0].Payload["transition"] != "recovery" {
		t.Fatalf("removal recovery = %+v", sent)
	}
	if h.manager.Configured("shop", "production") {
		t.Fatalf("scope still configured")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "alerts", "config", "shop", "production.json")); !os.IsNotExist(err) {
		t.Fatalf("config not removed: %v", err)
	}
}

func TestValidateSourceAlertRequest(t *testing.T) {
	valid := testMonitorRequest("shop", "production")
	if err := validateAlertConfigRequest(normalizeAlertConfigRequest(valid)); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	mutations := map[string]func(*AlertConfigRequest){
		"no target":           func(r *AlertConfigRequest) { r.Targets = AlertTargets{} },
		"timeout >= interval": func(r *AlertConfigRequest) { r.Monitors[0].TimeoutSeconds = 60 },
		"short interval":      func(r *AlertConfigRequest) { r.Monitors[0].IntervalSeconds = 5 },
		"bad check type":      func(r *AlertConfigRequest) { r.Monitors[0].CheckType = "tcp" },
		"bad path":            func(r *AlertConfigRequest) { r.Monitors[0].HealthPath = "health" },
		"private service target": func(r *AlertConfigRequest) {
			r.ServiceTargets = map[string]string{"web": "http://10.0.0.5/hook"}
		},
		"target for unmonitored": func(r *AlertConfigRequest) {
			r.ServiceTargets = map[string]string{"api": testServiceTarget}
		},
		"disk percent": func(r *AlertConfigRequest) { r.Host.DiskPercent = 100 },
		"cert order": func(r *AlertConfigRequest) {
			r.Certificates.CriticalBeforeSeconds = r.Certificates.WarnBeforeSeconds + 1
		},
	}
	for name, mutate := range mutations {
		request := testMonitorRequest("shop", "production")
		mutate(&request)
		if err := validateAlertConfigRequest(normalizeAlertConfigRequest(request)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	serviceOnly := testMonitorRequest("shop", "production")
	serviceOnly.Targets = AlertTargets{}
	serviceOnly.Host, serviceOnly.Certificates = nil, nil
	serviceOnly.ServiceTargets = map[string]string{"web": testServiceTarget}
	if err := validateAlertConfigRequest(normalizeAlertConfigRequest(serviceOnly)); err != nil {
		t.Fatalf("service-webhook-only request rejected: %v", err)
	}
	if !alertConfigEnabled(serviceOnly) || alertPolicyEnabled(serviceOnly.Policy) {
		t.Fatalf("monitoring-only policy must count as enabled")
	}
}
