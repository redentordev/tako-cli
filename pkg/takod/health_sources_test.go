package takod

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func subjectStatus(t *testing.T, manager *SourceAlertManager, project, environment, kind, subject string) SourceSubjectStatus {
	t.Helper()
	status, err := manager.Status(project, environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range status.Subjects {
		if candidate.Kind == kind && candidate.Subject == subject {
			return candidate
		}
	}
	t.Fatalf("subject %s/%s not found in %+v", kind, subject, status.Subjects)
	return SourceSubjectStatus{}
}

type fakeHostSamples struct {
	sample HostSample
	at     time.Time
	err    error
}

func (f *fakeHostSamples) Latest() (HostSample, time.Time, error) { return f.sample, f.at, f.err }

func TestHostEvaluatorThresholdsAndStaleness(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.Host = &HostAlertPolicy{DiskPercent: 90, MemoryPercent: 90, CPUPercent: 80, CPUForSeconds: 600, LoadPerCore: 2}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	source := &fakeHostSamples{at: h.now, sample: HostSample{
		CPUPercent: 95, MemoryPercent: 50, Load5: 1, CPUCount: 2,
		Root: HostFilesystemSample{UsedPercent: 96, UsedMB: 96000, TotalMB: 100000},
	}}
	evaluator := newHostAlertEvaluator(h.manager, source)
	evaluator.now = func() time.Time { return h.now }
	evaluator.EvaluateOnce()
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != "high_disk" || sent[0].Payload["severity"] != "critical" {
		t.Fatalf("disk opening = %+v", sent)
	}
	details := sent[0].Payload["details"].(map[string]string)
	if details["percent"] != "96" || details["threshold"] != "90" || details["usedMb"] != "96000" {
		t.Fatalf("details = %v", details)
	}
	if root := subjectStatus(t, h.manager, "shop", "production", AlertKindHost, "root"); strings.Contains(strings.Join(root.Reasons, ","), "cpu_high") {
		t.Fatalf("cpu alerted before cpuFor elapsed: %v", root.Reasons)
	}
	h.advance(11 * time.Minute)
	source.at = h.now
	evaluator.EvaluateOnce()
	if root := subjectStatus(t, h.manager, "shop", "production", AlertKindHost, "root"); !strings.Contains(strings.Join(root.Reasons, ","), "cpu_high") {
		t.Fatalf("sustained cpu not reported: %v", root.Reasons)
	}
	h.advance(10 * time.Minute)
	evaluator.EvaluateOnce()
	if root := subjectStatus(t, h.manager, "shop", "production", AlertKindHost, "root"); strings.Join(root.Reasons, ",") != "metrics_stale" {
		t.Fatalf("stale sample reasons = %v", root.Reasons)
	}
}

func TestLegacyHostSampleSourceParsesStringPercents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.json")
	document := `{"timestamp":"2026-03-01T12:00:00Z","cpu_percent":"12.5","memory":{"total_mb":2000,"used_mb":1000,"available_mb":1000,"percent":"50.0"},"disk":{"total_mb":100000,"used_mb":91000,"available_mb":9000,"percent":"91%"},"uptime_seconds":10,"load_average":{"1min":"0.5","5min":"1.25","15min":"1.0"}}`
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	sample, at, err := legacyHostSampleSource{path: func() string { return path }}.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPUPercent != 12.5 || sample.MemoryPercent != 50 || sample.Root.UsedPercent != 91 || sample.Load5 != 1.25 || sample.Root.HasInodes {
		t.Fatalf("sample = %+v", sample)
	}
	if !at.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("sampledAt = %v", at)
	}
	if err := os.WriteFile(path, []byte(`{"cpu_percent":"n/a"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (legacyHostSampleSource{path: func() string { return path }}).Latest(); err == nil {
		t.Fatalf("unparsable sample must be an error (stale)")
	}
}

func TestContainerSamplerOOMAndCrashLoop(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	sampler := newContainerAlertSampler(h.manager)
	base := ContainerSample{ID: strings.Repeat("a", 64), Name: "shop-production-web-1", Project: "shop", Environment: "production", Service: "web", Status: "running", StartedAt: h.now}
	sampler.observe(map[string]ContainerSample{base.ID: base}, h.now)
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("healthy sample delivered %d", len(sent))
	}
	// A stale OOMKilled flag seen on first sample is not an event; a new
	// kill after a restart is.
	oom := base
	oom.OOMKilled, oom.ExitCode, oom.RestartCount, oom.StartedAt = true, 137, 1, h.now.Add(time.Second)
	h.advance(30 * time.Second)
	sampler.observe(map[string]ContainerSample{base.ID: oom}, h.now)
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != "container_oom" {
		t.Fatalf("oom event = %+v", sent)
	}
	h.advance(30 * time.Second)
	sampler.observe(map[string]ContainerSample{base.ID: oom}, h.now)
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("unchanged OOM sample re-reported")
	}
	for restarts := 2; restarts <= 4; restarts++ {
		h.advance(30 * time.Second)
		looping := oom
		looping.OOMKilled, looping.RestartCount, looping.StartedAt = false, restarts, h.now
		sampler.observe(map[string]ContainerSample{base.ID: looping}, h.now)
	}
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != "container_crash_loop" {
		t.Fatalf("crash loop = %+v", sent)
	}
	samples, at := sampler.ContainerObservations()
	if len(samples) != 1 || !at.Equal(h.now) {
		t.Fatalf("ContainerObservations = %d at %v", len(samples), at)
	}
}

type fakeCertificateStatus map[string]*ProxyCertificateStatusResponse

func writeTestRouteManifest(t *testing.T, dir, project, environment, service string, domains ...string) {
	t.Helper()
	quoted := make([]string, len(domains))
	for i, domain := range domains {
		quoted[i] = fmt.Sprintf("%q", domain)
	}
	content := fmt.Sprintf(`{"version":1,"project":%q,"environment":%q,"routes":[{"service":%q,"domains":[%s],"upstreams":["http://%s-%s:3000"]}]}`,
		project, environment, service, strings.Join(quoted, ","), project, service)
	if _, err := ParseProxyRouteManifest(content); err != nil {
		t.Fatalf("test manifest invalid: %v", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, project+"-"+environment+"-"+service+".json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateExpiryScanner(t *testing.T) {
	h := newSourceTestHarness(t, t.TempDir())
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	routes := t.TempDir()
	writeTestRouteManifest(t, routes, "shop", "production", "web", "shop.example.com", "api.example.com", "old.example.com")
	statuses := fakeCertificateStatus{
		"shop.example.com": {Present: true, Source: "pushed", NotAfter: h.now.Add(10 * 24 * time.Hour)},
		"api.example.com":  {Present: true, Source: "caddy", NotAfter: h.now.Add(48 * time.Hour)},
		"old.example.com":  {Present: true, Source: "caddy", NotAfter: h.now.Add(-time.Hour)},
	}
	scanner := newCertificateExpiryScanner(h.manager)
	scanner.now = func() time.Time { return h.now }
	scanner.routesDir = func() string { return routes }
	scanner.status = func(_ context.Context, domain string) (*ProxyCertificateStatusResponse, error) {
		if status, ok := statuses[domain]; ok {
			return status, nil
		}
		return &ProxyCertificateStatusResponse{Domain: domain}, nil
	}
	scanner.ScanOnce(context.Background())
	got := map[string]string{}
	for _, delivery := range h.dispatch() {
		got[delivery.Payload["subject"].(string)] = fmt.Sprintf("%s/%s", delivery.Payload["event"], delivery.Payload["severity"])
	}
	want := map[string]string{
		"shop.example.com": "ssl_expiring_soon/warning",
		"api.example.com":  "ssl_expiring_soon/critical",
		"old.example.com":  "ssl_expired/critical",
	}
	for domain, expected := range want {
		if got[domain] != expected {
			t.Errorf("%s = %q, want %q", domain, got[domain], expected)
		}
	}
	// Renewal recovers with ssl_renewed; an unrouted domain is retired.
	statuses["shop.example.com"].NotAfter = h.now.Add(80 * 24 * time.Hour)
	writeTestRouteManifest(t, routes, "shop", "production", "web", "shop.example.com", "api.example.com")
	h.advance(6 * time.Hour)
	scanner.ScanOnce(context.Background())
	events := map[string]string{}
	for _, delivery := range h.dispatch() {
		events[delivery.Payload["subject"].(string)] = delivery.Payload["event"].(string) + "/" + delivery.Payload["transition"].(string)
	}
	if events["shop.example.com"] != "ssl_renewed/recovery" || events["old.example.com"] != "ssl_renewed/recovery" {
		t.Fatalf("second scan deliveries = %v", events)
	}
}

func TestCertificateExpiryReportedByCheckerNodeOnly(t *testing.T) {
	routes := t.TempDir()
	writeTestRouteManifest(t, routes, "shop", "production", "web", "shop.example.com")
	sent := map[string]int{}
	for _, node := range []string{"app-1", "app-2"} {
		h := newSourceTestHarness(t, t.TempDir())
		h.manager.SetNodeName(node)
		// testMonitorRequest elects app-1 as the http checker node.
		if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
			t.Fatal(err)
		}
		scanner := newCertificateExpiryScanner(h.manager)
		scanner.now = func() time.Time { return h.now }
		scanner.routesDir = func() string { return routes }
		scanner.node = func() string { return node }
		scanner.status = func(_ context.Context, domain string) (*ProxyCertificateStatusResponse, error) {
			return &ProxyCertificateStatusResponse{Domain: domain, Present: true, Source: "caddy", NotAfter: h.now.Add(48 * time.Hour)}, nil
		}
		scanner.ScanOnce(context.Background())
		sent[node] = len(h.dispatch())
	}
	if sent["app-1"] != 1 || sent["app-2"] != 0 {
		t.Fatalf("certificate alerts per node = %v, want only the checker node app-1", sent)
	}
}

func writeTestDesiredRevision(t *testing.T, dataDir, project, environment, content string, modified time.Time) {
	t.Helper()
	path := filepath.Join(dataDir, "desired", project, environment, "revision.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func newTestMonitorRunner(t *testing.T, h *sourceTestHarness, dataDir, routes string, server *httptest.Server) *serviceMonitorRunner {
	t.Helper()
	runner := newServiceMonitorRunner(h.manager, dataDir, func() string { return "app-1" })
	runner.now = func() time.Time { return h.now }
	runner.routesDir = func() string { return routes }
	runner.leaseHeld = func(context.Context, string, string, string) (bool, error) { return false, nil }
	if server != nil {
		address := server.Listener.Addr().String()
		runner.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
			if target != localProxyAddress {
				t.Errorf("check dialed %s, want the local proxy", target)
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		}
	}
	return runner
}

func TestHTTPMonitorCheckThroughLocalProxy(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" || r.URL.Path != "/health" {
			t.Errorf("request host/path = %s %s", r.Host, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("internal error: database password rejected"))
	}))
	defer server.Close()

	dataDir, routes := t.TempDir(), t.TempDir()
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.Monitors[0].Domain = "example.com"
	request.Monitors[0].AlertAfter, request.Monitors[0].RecoverAfter = 1, 1
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	writeTestDesiredRevision(t, dataDir, "shop", "production", `{"services":{"web":{"replicas":1}}}`, h.now.Add(-time.Hour))
	runner := newTestMonitorRunner(t, h, dataDir, routes, server)
	monitor := request.Monitors[0]

	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); state.State != SubjectStateUnknown || state.Reasons[0] != "no_local_route" {
		t.Fatalf("missing route state = %+v", state)
	}
	writeTestRouteManifest(t, routes, "shop", "production", "web", "example.com")
	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); state.State != SubjectStateOK {
		t.Fatalf("healthy state = %+v", state)
	}
	status = http.StatusInternalServerError
	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	sent := h.dispatch()
	if len(sent) != 1 {
		t.Fatalf("want opening, got %d", len(sent))
	}
	encoded := fmt.Sprint(sent[0].Payload)
	if strings.Contains(encoded, "password") || strings.Contains(encoded, "127.0.0.1") {
		t.Fatalf("payload carries raw response/error text: %s", encoded)
	}
	if details := sent[0].Payload["details"].(map[string]string); details["statusCode"] != "500" || details["errorClass"] != "status" {
		t.Fatalf("details = %v", details)
	}
	// Maintenance holds the subject without a transition.
	maintenance := filepath.Join(routes, runtimeid.MaintenanceProxyConfigFileName("shop", "production", "web"))
	if err := os.WriteFile(maintenance, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); state.State != SubjectStateMaintenance {
		t.Fatalf("maintenance state = %+v", state)
	}
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("maintenance produced a transition")
	}
	_ = os.Remove(maintenance)

	// A certificate that does not cover the domain is tls_invalid.
	writeTestRouteManifest(t, routes, "shop", "production", "web", "shop.example.org")
	other := monitor
	other.Domain = "shop.example.org"
	runner.CheckOnce(context.Background(), "shop", "production", other)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); strings.Join(state.Reasons, ",") != "tls_invalid" {
		t.Fatalf("tls state = %+v", state)
	}
}

func TestMonitorPausesForLeaseGraceAndOwnership(t *testing.T) {
	dataDir, routes := t.TempDir(), t.TempDir()
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	writeTestDesiredRevision(t, dataDir, "shop", "production", `{"services":{"web":{"replicas":1,"healthCheck":{"startPeriod":"2m"}}}}`, h.now.Add(-time.Minute))
	runner := newTestMonitorRunner(t, h, dataDir, routes, nil)
	monitor := request.Monitors[0]

	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); state.State != SubjectStatePaused || state.Reasons[0] != "grace" {
		t.Fatalf("grace state = %+v", state)
	}
	h.advance(5 * time.Minute)
	runner.leaseHeld = func(context.Context, string, string, string) (bool, error) { return true, nil }
	runner.CheckOnce(context.Background(), "shop", "production", monitor)
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "web"); state.State != SubjectStatePaused || state.Reasons[0] != "operation" {
		t.Fatalf("lease state = %+v", state)
	}
	// The checker moved to another node: the subject is retired.
	moved := monitor
	moved.CheckerNode = "app-2"
	runner.CheckOnce(context.Background(), "shop", "production", moved)
	if subjects := h.manager.Subjects("shop", "production", AlertKindService); len(subjects) != 0 {
		t.Fatalf("non-checker node kept subject: %v", subjects)
	}
}

func TestContainerMonitorCountsLocalAssignments(t *testing.T) {
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.Monitors[0] = MonitorSpec{Service: "worker", CheckType: MonitorCheckContainer, IntervalSeconds: 60, TimeoutSeconds: 10, AlertAfter: 1, RecoverAfter: 1}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	writeTestDesiredRevision(t, dataDir, "shop", "production", `{"services":{"worker":{"replicas":3,"assignments":[{"slot":1,"node":"app-1"},{"slot":2,"node":"app-1"},{"slot":3,"node":"app-2"}]}}}`, h.now.Add(-time.Hour))
	runner := newTestMonitorRunner(t, h, dataDir, t.TempDir(), nil)
	runner.actual = func(context.Context, string, string) (*ActualStateResponse, error) {
		return &ActualStateResponse{Services: map[string]*ActualService{"worker": {ActiveContainers: []string{"one"}, Health: HealthStateUnhealthy}}}, nil
	}
	runner.CheckOnce(context.Background(), "shop", "production", request.Monitors[0])
	state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "worker")
	if strings.Join(state.Reasons, ",") != "replicas_missing,unhealthy" || state.Details["expected"] != "2" || state.Details["running"] != "1" {
		t.Fatalf("container state = %+v", state)
	}
}

func TestMonitorDockerCallsAreBounded(t *testing.T) {
	// A wedged dockerd blocks until the context ends: both the container
	// check and the container sampler must give up on their own deadline.
	wedged := func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return fmt.Errorf("docker call without a deadline")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, t.TempDir())
	request := testMonitorRequest("shop", "production")
	request.Monitors[0] = MonitorSpec{Service: "worker", CheckType: MonitorCheckContainer, IntervalSeconds: 60, TimeoutSeconds: 10, AlertAfter: 1, RecoverAfter: 1}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	writeTestDesiredRevision(t, dataDir, "shop", "production", `{"services":{"worker":{"replicas":1}}}`, h.now.Add(-time.Hour))
	runner := newTestMonitorRunner(t, h, dataDir, t.TempDir(), nil)
	runner.actualTimeout = 50 * time.Millisecond
	var actualErr error
	runner.actual = func(ctx context.Context, _, _ string) (*ActualStateResponse, error) {
		actualErr = wedged(ctx)
		return nil, actualErr
	}
	done := make(chan struct{})
	go func() {
		runner.CheckOnce(context.Background(), "shop", "production", request.Monitors[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("container check blocked on a wedged docker read")
	}
	if !errors.Is(actualErr, context.DeadlineExceeded) {
		t.Fatalf("actual read err = %v", actualErr)
	}
	if state := subjectStatus(t, h.manager, "shop", "production", AlertKindService, "worker"); strings.Join(state.Reasons, ",") != "docker_unavailable" {
		t.Fatalf("wedged docker state = %+v", state)
	}

	sampler := newContainerAlertSampler(h.manager)
	sampler.dockerTimeout = 50 * time.Millisecond
	var samplerErr error
	sampler.docker = func(ctx context.Context, _ ...string) ([]byte, error) {
		samplerErr = wedged(ctx)
		return nil, samplerErr
	}
	done = make(chan struct{})
	go func() {
		sampler.SampleOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("container sampler blocked on a wedged docker call")
	}
	if !errors.Is(samplerErr, context.DeadlineExceeded) {
		t.Fatalf("sampler docker err = %v", samplerErr)
	}
}

func TestExpectedLocalReplicasGlobalPlacement(t *testing.T) {
	global := localDesiredService{Replicas: 2}
	global.Placement = &struct {
		Strategy string `json:"strategy"`
	}{Strategy: "global"}
	if got := expectedLocalReplicas(global, "app-1", false); got != 1 {
		t.Fatalf("global without assignments = %d, want 1", got)
	}
	global.Assignments = []struct {
		Slot   int    `json:"slot"`
		NodeID string `json:"nodeId"`
		Node   string `json:"node"`
	}{{Slot: 1, Node: "app-1"}, {Slot: 2, Node: "app-2"}}
	if got := expectedLocalReplicas(global, "app-1", false); got != 1 {
		t.Fatalf("global assigned node = %d, want 1", got)
	}
	// A node excluded by placement, cordon or drain has no assignment and
	// must not expect a replica (it would alert replicas_missing forever).
	if got := expectedLocalReplicas(global, "app-3", false); got != 0 {
		t.Fatalf("global excluded node = %d, want 0", got)
	}
}
