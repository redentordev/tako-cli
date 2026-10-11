package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// fakePreviewNode is one takod node for preview tests: it advertises
// capabilities, serves preview records and records every request.
type fakePreviewNode struct {
	name         string
	capabilities []string
	records      []takod.PreviewRecord
	failCleanup  error
	failStatus   error
	// documents marks "<environment>/<document>" state documents as found.
	documents map[string]bool

	mu       sync.Mutex
	requests []fakePreviewRequest
}

type fakePreviewRequest struct {
	method   string
	endpoint string
	body     any
}

func (n *fakePreviewNode) RequestJSON(ctx context.Context, method string, endpoint string, value any) (string, error) {
	return n.RequestJSONWithTimeout(ctx, method, endpoint, value, 0)
}

func (n *fakePreviewNode) RequestJSONWithTimeout(_ context.Context, method string, endpoint string, value any, _ time.Duration) (string, error) {
	n.mu.Lock()
	n.requests = append(n.requests, fakePreviewRequest{method: method, endpoint: endpoint, body: value})
	n.mu.Unlock()
	switch {
	case endpoint == "/v1/status":
		if n.failStatus != nil {
			return "", n.failStatus
		}
		payload, _ := json.Marshal(takod.Status{Capabilities: n.capabilities})
		return string(payload), nil
	case strings.HasPrefix(endpoint, "/v1/state/previews"):
		payload, _ := json.Marshal(takod.PreviewListResponse{Project: "demo", Previews: n.records})
		return string(payload), nil
	case endpoint == "/v1/cleanup":
		if n.failCleanup != nil {
			return "", n.failCleanup
		}
		request := value.(takod.CleanupRequest)
		kept := n.records[:0:0]
		for _, record := range n.records {
			if record.Environment != request.Environment {
				kept = append(kept, record)
			}
		}
		n.records = kept
		payload, _ := json.Marshal(takod.CleanupResponse{Project: request.Project, Environment: request.Environment, VolumesRemoved: 1, PreviewRecordRemoved: true})
		return string(payload), nil
	case endpoint == "/v1/state" && method == "PUT":
		request := value.(takod.StateDocumentRequest)
		var record takod.PreviewRecord
		if err := json.Unmarshal([]byte(request.Content), &record); err != nil {
			return "", err
		}
		n.records = []takod.PreviewRecord{record}
		return `{"found":true}`, nil
	case strings.HasPrefix(endpoint, "/v1/state"):
		if parsed, err := url.Parse(endpoint); err == nil {
			query := parsed.Query()
			if n.documents[query.Get("environment")+"/"+query.Get("document")] {
				return `{"found":true,"content":"{}"}`, nil
			}
		}
		return `{"found":false}`, nil
	}
	return "", fmt.Errorf("unexpected request %s %s", method, endpoint)
}

func (n *fakePreviewNode) cleanupRequests() []takod.CleanupRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []takod.CleanupRequest
	for _, request := range n.requests {
		if request.endpoint == "/v1/cleanup" {
			out = append(out, request.body.(takod.CleanupRequest))
		}
	}
	return out
}

func (n *fakePreviewNode) recordWrites() []takod.PreviewRecord {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []takod.PreviewRecord
	for _, request := range n.requests {
		if request.endpoint == "/v1/state" && request.method == "PUT" {
			var record takod.PreviewRecord
			_ = json.Unmarshal([]byte(request.body.(takod.StateDocumentRequest).Content), &record)
			out = append(out, record)
		}
	}
	return out
}

var previewCapabilities = []string{takod.CapabilityPreviewLifecycleV1}

type fakePreviewFleet struct {
	nodes       map[string]*fakePreviewNode
	unreachable map[string]bool
	heldLeases  map[string]bool
	leases      []string
	// onAcquire runs after a lease is granted, before the operation reads
	// anything under it.
	onAcquire func(env string)
}

func (f *fakePreviewFleet) install(e *Engine) {
	e.previewRuntimeOverride = func(cfg *config.Config) (*previewRuntime, error) {
		return &previewRuntime{
			clientFor: f.clientFor,
			acquire: func(ctx context.Context, env string, servers []string, operation string) (context.Context, func(), error) {
				if f.heldLeases[env] {
					return nil, nil, &LockedError{Operation: operation, Err: fmt.Errorf("lease for %s is held", env)}
				}
				f.leases = append(f.leases, env+":"+strings.Join(servers, ","))
				if f.onAcquire != nil {
					f.onAcquire(env)
				}
				return ctx, func() {}, nil
			},
			close: func() {},
		}, nil
	}
}

func (f *fakePreviewFleet) clientFor(_ context.Context, server string) (any, error) {
	if f.unreachable[server] {
		return nil, &ConnectivityError{Server: server, Err: errors.New("connection refused")}
	}
	node, ok := f.nodes[server]
	if !ok {
		return nil, &ConnectivityError{Server: server, Err: errors.New("unknown server")}
	}
	return node, nil
}

func previewTestConfig(servers ...string) *config.Config {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}, Servers: map[string]config.ServerConfig{}}
	for _, server := range servers {
		cfg.Servers[server] = config.ServerConfig{Host: "192.0.2.1", User: "deploy"}
	}
	return cfg
}

func previewRecord(name string, base string, servers []string, updated time.Time, expires *time.Time) takod.PreviewRecord {
	return takod.PreviewRecord{
		APIVersion:  takod.PreviewRecordAPIVersion,
		Kind:        takod.PreviewRecordKind,
		Project:     "demo",
		Environment: config.PreviewIdentity(name),
		Name:        name,
		Base:        base,
		Servers:     servers,
		CreatedAt:   updated,
		UpdatedAt:   updated,
		ExpiresAt:   expires,
		TTL:         "168h",
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func newPreviewTestEngine(t *testing.T) (*Engine, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	e := New(Options{Sink: sink})
	return e, sink
}

type recordingSink struct {
	mu     sync.Mutex
	events []events.Event
}

func (s *recordingSink) Emit(event events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *recordingSink) ofType(eventType string) []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []events.Event
	for _, event := range s.events {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

func TestPreviewTeardownRequestNeverRemovesSharedResources(t *testing.T) {
	request := PreviewTeardownRequest("demo", "preview-pr-42", map[string]config.ServiceConfig{"web": {Proxy: &config.ProxyConfig{}}})
	if !request.RemovePreviewData || !request.RemoveContainers || !request.RemoveNetworks || !request.RemoveTakodState {
		t.Fatalf("teardown request misses a removal step: %#v", request)
	}
	if request.RemoveImages || len(request.ImageRepositories) > 0 || request.RemoveDeployFiles || request.PruneDocker || request.CleanOldImages {
		t.Fatalf("teardown request touches shared images or /opt/<project>: %#v", request)
	}
	if request.Environment != "preview-pr-42" {
		t.Fatalf("environment = %q", request.Environment)
	}
	if len(request.ProxyFiles) != 2 {
		t.Fatalf("proxy files = %v, want the environment file and the web maintenance file", request.ProxyFiles)
	}
}

func TestListPreviewsMergesNewestSkipsOldNodesAndReportsUnreachable(t *testing.T) {
	now := time.Now()
	older := previewRecord("pr-1", "preview", []string{"a"}, now.Add(-2*time.Hour), timePointer(now.Add(time.Hour)))
	newer := previewRecord("pr-1", "preview", []string{"a", "b"}, now.Add(-time.Hour), timePointer(now.Add(2*time.Hour)))
	expired := previewRecord("pr-2", "preview", []string{"b"}, now.Add(-10*time.Hour), timePointer(now.Add(-time.Minute)))
	fleet := &fakePreviewFleet{
		nodes: map[string]*fakePreviewNode{
			"a":   {name: "a", capabilities: previewCapabilities, records: []takod.PreviewRecord{older}},
			"b":   {name: "b", capabilities: previewCapabilities, records: []takod.PreviewRecord{newer, expired}},
			"old": {name: "old", capabilities: []string{"container.runtime-controls-v1"}},
		},
		unreachable: map[string]bool{"down": true},
	}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	result, err := e.ListPreviews(context.Background(), previewTestConfig("a", "b", "old", "down"))
	if Classify(err) != ClassAttention {
		t.Fatalf("unreachable server: class = %d (%v), want attention", Classify(err), err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items = %#v", result.Items)
	}
	first := result.Items[0]
	if first.Name != "pr-1" || !first.UpdatedAt.Equal(newer.UpdatedAt) || strings.Join(first.Nodes, ",") != "a,b" || first.Status != PreviewStatusActive {
		t.Fatalf("merged record = %#v", first)
	}
	if result.Items[1].Status != PreviewStatusExpired {
		t.Fatalf("pr-2 status = %s, want expired", result.Items[1].Status)
	}
	statuses := map[string]string{}
	for _, node := range result.Nodes {
		statuses[node.Name] = node.Status
	}
	if statuses["old"] != previewNodeStatusSkipped || statuses["down"] != previewNodeStatusFailed || statuses["a"] != previewNodeStatusOK {
		t.Fatalf("node statuses = %v", statuses)
	}
}

func TestListPreviewsQueriesServersOnlyAnAbstractTemplateUses(t *testing.T) {
	// Every top-level server is queried, including one that no resolved
	// environment references (an abstract template's dedicated server).
	record := previewRecord("pr-7", "preview", []string{"preview-box"}, time.Now(), nil)
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"prod":        {capabilities: previewCapabilities},
		"preview-box": {capabilities: previewCapabilities, records: []takod.PreviewRecord{record}},
	}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	cfg := previewTestConfig("prod", "preview-box")
	cfg.Environments = map[string]config.EnvironmentConfig{"production": {Servers: []string{"prod"}}}
	result, err := e.ListPreviews(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ListPreviews: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "pr-7" || result.Items[0].ExpiresAt != nil {
		t.Fatalf("items = %#v", result.Items)
	}
}

func TestDestroyPreviewSendsOneGuardedTeardownPerServer(t *testing.T) {
	record := previewRecord("pr-42", "preview", []string{"a", "b"}, time.Now(), nil)
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"a":    {capabilities: previewCapabilities, records: []takod.PreviewRecord{record}},
		"b":    {capabilities: previewCapabilities},
		"base": {capabilities: previewCapabilities},
	}}
	e, sink := newPreviewTestEngine(t)
	fleet.install(e)
	result, err := e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: previewTestConfig("a", "b", "base"), Name: "PR 42"})
	if err != nil {
		t.Fatalf("DestroyPreview: %v", err)
	}
	if result.Status != PreviewDestroyStatusDestroyed || result.Name != "pr-42" || result.Base != "preview" || result.VolumesRemoved != 2 {
		t.Fatalf("result = %#v", result)
	}
	for _, server := range []string{"a", "b"} {
		requests := fleet.nodes[server].cleanupRequests()
		if len(requests) != 1 {
			t.Fatalf("%s got %d teardown requests, want exactly 1", server, len(requests))
		}
		request := requests[0]
		if request.Environment != "preview-pr-42" || !request.RemovePreviewData || request.RemoveImages || len(request.ImageRepositories) > 0 || request.RemoveDeployFiles {
			t.Fatalf("%s teardown = %#v", server, request)
		}
	}
	// Each server's request carries the updatedAt of its own record, read
	// under the lease; a server without a record sends none.
	if got := fleet.nodes["a"].cleanupRequests()[0].PreviewUpdatedAt; got == nil || !got.Equal(record.UpdatedAt) {
		t.Fatalf("a: previewUpdatedAt = %v, want %v", got, record.UpdatedAt)
	}
	if got := fleet.nodes["b"].cleanupRequests()[0].PreviewUpdatedAt; got != nil {
		t.Fatalf("b: previewUpdatedAt = %v, want none", got)
	}
	if got := fleet.nodes["base"].cleanupRequests(); len(got) != 0 {
		t.Fatalf("a server outside the preview got a teardown: %#v", got)
	}
	if len(fleet.leases) != 1 || fleet.leases[0] != "preview-pr-42:a,b" {
		t.Fatalf("leases = %v, want the preview's own scope on a,b", fleet.leases)
	}
	if len(sink.ofType(events.TypePreviewDestroyed)) != 1 {
		t.Fatal("missing preview.destroyed event")
	}

	again, err := e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: previewTestConfig("a", "b", "base"), Name: "pr-42"})
	if err != nil || again.Status != PreviewDestroyStatusNotFound {
		t.Fatalf("second destroy = %#v, %v; want not_found without error", again, err)
	}
	if got := len(fleet.nodes["a"].cleanupRequests()); got != 1 {
		t.Fatalf("not_found destroy sent a teardown (%d requests)", got)
	}
}

func TestDestroyPreviewFailsClosedOnNodeWithoutCapability(t *testing.T) {
	record := previewRecord("pr-42", "preview", []string{"a", "old"}, time.Now(), nil)
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"a":   {capabilities: previewCapabilities, records: []takod.PreviewRecord{record}},
		"old": {capabilities: []string{"container.runtime-controls-v1"}},
	}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	result, err := e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: previewTestConfig("a", "old"), Name: "pr-42"})
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), takod.CapabilityPreviewLifecycleV1) {
		t.Fatalf("err = %v (class %d), want invalid naming the capability", err, Classify(err))
	}
	if result.Status != PreviewDestroyStatusFailed {
		t.Fatalf("status = %s", result.Status)
	}
	for name, node := range fleet.nodes {
		if got := node.cleanupRequests(); len(got) != 0 {
			t.Fatalf("%s was mutated before the capability check passed everywhere: %#v", name, got)
		}
	}
}

func TestDestroyPreviewRetriesAfterVolumeRemovalFailure(t *testing.T) {
	record := previewRecord("pr-42", "preview", []string{"a"}, time.Now(), nil)
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{record}, failCleanup: errors.New("takod request POST /v1/cleanup returned HTTP 500: remove volume demo_preview-pr-42_data: in use")}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"a": node}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	cfg := previewTestConfig("a")
	result, err := e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: cfg, Name: "pr-42"})
	if err == nil || result.Status != PreviewDestroyStatusFailed {
		t.Fatalf("first destroy = %#v, %v; want failed", result, err)
	}
	if len(node.records) != 1 {
		t.Fatal("the record must survive a failed teardown so the destroy can be retried")
	}
	node.failCleanup = nil
	result, err = e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: cfg, Name: "pr-42"})
	if err != nil || result.Status != PreviewDestroyStatusDestroyed {
		t.Fatalf("retry = %#v, %v; want destroyed", result, err)
	}
}

func TestDestroyPreviewHeldLeaseIsLocked(t *testing.T) {
	record := previewRecord("pr-42", "preview", []string{"a"}, time.Now(), nil)
	fleet := &fakePreviewFleet{
		nodes:      map[string]*fakePreviewNode{"a": {capabilities: previewCapabilities, records: []takod.PreviewRecord{record}}},
		heldLeases: map[string]bool{"preview-pr-42": true},
	}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	_, err := e.DestroyPreview(context.Background(), PreviewDestroyRequest{Config: previewTestConfig("a"), Name: "pr-42"})
	if Classify(err) != ClassLocked {
		t.Fatalf("class = %d (%v), want locked", Classify(err), err)
	}
	if got := fleet.nodes["a"].cleanupRequests(); len(got) != 0 {
		t.Fatalf("teardown sent without the lease: %#v", got)
	}
}

func TestPrunePreviewsDestroysOnlyExpiredAndSkipsHeldLeases(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	expired := previewRecord("old", "preview", []string{"a"}, now.Add(-200*time.Hour), timePointer(now.Add(-time.Hour)))
	busy := previewRecord("busy", "preview", []string{"a"}, now.Add(-200*time.Hour), timePointer(now.Add(-time.Hour)))
	active := previewRecord("fresh", "preview", []string{"a"}, now, timePointer(now.Add(time.Hour)))
	forever := previewRecord("pinned", "preview", []string{"a"}, now.Add(-1000*time.Hour), nil)
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{expired, busy, active, forever}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"a": node}, heldLeases: map[string]bool{"preview-busy": true}}
	e, sink := newPreviewTestEngine(t)
	fleet.install(e)

	dry, err := e.PrunePreviews(context.Background(), PreviewPruneRequest{Config: previewTestConfig("a"), DryRun: true, Now: now})
	if err != nil || len(dry.Destroyed) != 2 || len(node.cleanupRequests()) != 0 {
		t.Fatalf("dry run = %#v, %v (requests %d)", dry, err, len(node.cleanupRequests()))
	}

	result, err := e.PrunePreviews(context.Background(), PreviewPruneRequest{Config: previewTestConfig("a"), Now: now})
	if err != nil {
		t.Fatalf("PrunePreviews: %v", err)
	}
	if len(result.Destroyed) != 1 || result.Destroyed[0].Name != "old" {
		t.Fatalf("destroyed = %#v", result.Destroyed)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Name != "busy" {
		t.Fatalf("skipped = %#v", result.Skipped)
	}
	requests := node.cleanupRequests()
	if len(requests) != 1 || requests[0].Environment != "preview-old" {
		t.Fatalf("teardowns = %#v", requests)
	}
	if len(sink.ofType(events.TypePreviewPruned)) != 1 {
		t.Fatal("missing preview.pruned event")
	}
}

func TestPrunePreviewsFailureIsAttention(t *testing.T) {
	now := time.Now()
	expired := previewRecord("old", "preview", []string{"a"}, now.Add(-200*time.Hour), timePointer(now.Add(-time.Hour)))
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{expired}, failCleanup: errors.New("boom")}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"a": node}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	result, err := e.PrunePreviews(context.Background(), PreviewPruneRequest{Config: previewTestConfig("a"), Now: now})
	if Classify(err) != ClassAttention || len(result.Failed) != 1 {
		t.Fatalf("result = %#v, err = %v; want one failure and exit 6", result, err)
	}
}

const previewDeployConfigYAML = `project:
  name: demo
  version: 1.0.0
servers:
  app-1:
    host: 192.0.2.10
    user: deploy
    password: test-password
  preview-box:
    host: 192.0.2.11
    user: deploy
    password: test-password
environments:
  production:
    servers: [app-1]
    services:
      web:
        image: nginx:1
        port: 80
        proxy:
          domain: app.example.com
  preview:
    abstract: true
    extends: production
    servers: [app-1, preview-box]
    preview:
      domain: "{name}.preview.example.com"
      ttl: 72h
      max: 2
`

func loadPreviewDeployConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	cfg, err := config.ParseConfigDocumentWithOptions([]byte(previewDeployConfigYAML), false, config.LoadOptions{Preview: &config.PreviewInstance{Name: name}})
	if err != nil {
		t.Fatalf("load preview config: %v", err)
	}
	return cfg
}

func newPreviewDeploySession(t *testing.T, e *Engine, name string, opts *DeployPreviewOptions) *DeploySession {
	t.Helper()
	cfg := loadPreviewDeployConfig(t, name)
	identity := cfg.PreviewInstance().Identity
	return &DeploySession{engine: e, cfg: cfg, envName: identity, req: DeployRequest{Config: cfg, Environment: identity, Preview: opts}}
}

func TestPreparePreviewDeployRejectsOldNodeBeforeAnyMutation(t *testing.T) {
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"app-1":       {capabilities: previewCapabilities},
		"preview-box": {capabilities: []string{"container.runtime-controls-v1"}},
	}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := newPreviewDeploySession(t, e, "pr-1", &DeployPreviewOptions{NoPrune: true})
	err := e.preparePreviewDeploy(context.Background(), session)
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "preview-box") {
		t.Fatalf("err = %v, want invalid naming the old node", err)
	}
	for name, node := range fleet.nodes {
		if writes := node.recordWrites(); len(writes) != 0 {
			t.Fatalf("%s got a record write: %#v", name, writes)
		}
	}
}

func TestPreparePreviewDeployEnforcesMaxPerBase(t *testing.T) {
	now := time.Now()
	future := timePointer(now.Add(time.Hour))
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("pr-1", "preview", []string{"app-1"}, now, future),
		// A failed first deploy leaves a record that counts.
		previewRecord("pr-2", "preview", []string{"app-1"}, now, future),
		previewRecord("other", "staging", []string{"app-1"}, now, future),
	}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)

	err := e.preparePreviewDeploy(context.Background(), newPreviewDeploySession(t, e, "pr-3", &DeployPreviewOptions{NoPrune: true}))
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "preview limit reached") || !strings.Contains(err.Error(), "pr-1") {
		t.Fatalf("err = %v, want the limit error listing existing previews", err)
	}

	// Redeploying an existing preview does not count itself.
	session := newPreviewDeploySession(t, e, "pr-2", &DeployPreviewOptions{NoPrune: true})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("redeploy of pr-2: %v", err)
	}
	if session.preview == nil || session.preview.existing == nil {
		t.Fatal("redeploy did not pick up the existing record")
	}
}

func TestPreparePreviewDeployRejectsBaseMismatch(t *testing.T) {
	now := time.Now()
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("pr-1", "staging", []string{"app-1"}, now, nil),
	}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	err := e.preparePreviewDeploy(context.Background(), newPreviewDeploySession(t, e, "pr-1", &DeployPreviewOptions{NoPrune: true}))
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "was created from staging, not preview") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreparePreviewDeployPrunesExpiredBeforeCounting(t *testing.T) {
	now := time.Now()
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("pr-1", "preview", []string{"app-1"}, now, timePointer(now.Add(time.Hour))),
		previewRecord("stale", "preview", []string{"app-1"}, now.Add(-100*time.Hour), timePointer(now.Add(-time.Hour))),
	}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	// max is 2: pr-1 plus the expired stale preview would block pr-3 unless
	// the prune runs first. Planning only discounts the expired preview;
	// Apply prunes it before the record write.
	session := newPreviewDeploySession(t, e, "pr-3", &DeployPreviewOptions{})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("prepare with prune: %v", err)
	}
	if requests := node.cleanupRequests(); len(requests) != 0 {
		t.Fatalf("planning pruned: %#v", requests)
	}
	session.preview.clientFor = fleet.clientFor
	session.mutationServerNames = []string{"app-1"}
	if err := session.applyPreviewBeforeMutation(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	requests := node.cleanupRequests()
	if len(requests) != 1 || requests[0].Environment != "preview-stale" {
		t.Fatalf("prune teardowns = %#v", requests)
	}
	if writes := node.recordWrites(); len(writes) != 1 || writes[0].Name != "pr-3" {
		t.Fatalf("record writes = %#v", writes)
	}
}

func TestPreparePreviewDeployIsReadOnly(t *testing.T) {
	// A plan-only, cancelled or refused deploy must neither prune expired
	// previews (deleting their volumes) nor leave a record behind.
	now := time.Now()
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("stale", "preview", []string{"app-1"}, now.Add(-100*time.Hour), timePointer(now.Add(-time.Hour))),
	}}
	box := &fakePreviewNode{capabilities: previewCapabilities}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": box}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := newPreviewDeploySession(t, e, "pr-3", &DeployPreviewOptions{})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for name, n := range fleet.nodes {
		if requests := n.cleanupRequests(); len(requests) != 0 {
			t.Fatalf("%s: planning sent teardowns %#v", name, requests)
		}
		if writes := n.recordWrites(); len(writes) != 0 {
			t.Fatalf("%s: planning wrote records %#v", name, writes)
		}
	}
	if len(fleet.leases) != 0 {
		t.Fatalf("planning took preview leases %v", fleet.leases)
	}
}

func TestApplyPreviewRechecksLimitWhenPruneCannotFreeASlot(t *testing.T) {
	now := time.Now()
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("pr-1", "preview", []string{"app-1"}, now, timePointer(now.Add(time.Hour))),
		previewRecord("busy", "preview", []string{"app-1"}, now.Add(-100*time.Hour), timePointer(now.Add(-time.Hour))),
	}}
	fleet := &fakePreviewFleet{
		nodes:      map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}},
		heldLeases: map[string]bool{"preview-busy": true},
	}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := newPreviewDeploySession(t, e, "pr-3", &DeployPreviewOptions{})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	session.preview.clientFor = fleet.clientFor
	session.mutationServerNames = []string{"app-1"}
	err := session.applyPreviewBeforeMutation(context.Background())
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "preview limit reached") {
		t.Fatalf("err = %v, want the limit error after a skipped prune", err)
	}
	if writes := node.recordWrites(); len(writes) != 0 {
		t.Fatalf("record written past the limit: %#v", writes)
	}
}

func TestPreparePreviewDeployRefusesDeclaredEnvironmentState(t *testing.T) {
	for _, document := range []string{"desired", "history"} {
		t.Run(document, func(t *testing.T) {
			// A declared environment preview-demo was deployed from another
			// branch; this config lacks it and asks for preview demo.
			node := &fakePreviewNode{capabilities: previewCapabilities, documents: map[string]bool{"preview-demo/" + document: true}}
			fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
			e, _ := newPreviewTestEngine(t)
			fleet.install(e)
			session := newPreviewDeploySession(t, e, "demo", &DeployPreviewOptions{NoPrune: true})
			err := e.preparePreviewDeploy(context.Background(), session)
			if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "declared environment") {
				t.Fatalf("err = %v, want invalid refusing to adopt the declared environment", err)
			}
			if writes := node.recordWrites(); len(writes) != 0 {
				t.Fatalf("record written: %#v", writes)
			}
		})
	}

	// An existing preview's own state never blocks its redeploy.
	now := time.Now()
	node := &fakePreviewNode{
		capabilities: previewCapabilities,
		records:      []takod.PreviewRecord{previewRecord("demo", "preview", []string{"app-1"}, now, nil)},
		documents:    map[string]bool{"preview-demo/desired": true, "preview-demo/history": true},
	}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	if err := e.preparePreviewDeploy(context.Background(), newPreviewDeploySession(t, e, "demo", &DeployPreviewOptions{NoPrune: true})); err != nil {
		t.Fatalf("redeploy of existing preview: %v", err)
	}
}

const declaredPreviewNamedConfigYAML = `project:
  name: demo
  version: 1.0.0
servers:
  app-1:
    host: 192.0.2.10
    user: deploy
    password: test-password
environments:
  preview-demo:
    servers: [app-1]
    services:
      web:
        image: nginx:1
        port: 80
`

func TestDeclaredDeployRefusedWhilePreviewRecordExists(t *testing.T) {
	cfg, err := config.ParseConfigDocumentWithOptions([]byte(declaredPreviewNamedConfigYAML), false, config.LoadOptions{})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("demo", "preview", []string{"app-1"}, time.Now(), nil),
	}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := &DeploySession{engine: e, cfg: cfg, envName: "preview-demo", req: DeployRequest{Config: cfg, Environment: "preview-demo"}}
	err = e.preparePreviewDeploy(context.Background(), session)
	if Classify(err) != ClassInvalid || !strings.Contains(err.Error(), "tako preview destroy demo") {
		t.Fatalf("err = %v, want invalid naming the preview", err)
	}

	node.records = nil
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil || session.preview != nil {
		t.Fatalf("declared deploy without a preview record: err = %v, preview = %#v", err, session.preview)
	}
}

func TestPreparePreviewDeployAllowsRedeployAfterMaxLowered(t *testing.T) {
	now := time.Now()
	future := timePointer(now.Add(time.Hour))
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{
		previewRecord("pr-1", "preview", []string{"app-1"}, now, future),
		previewRecord("pr-2", "preview", []string{"app-1"}, now, future),
		previewRecord("pr-3", "preview", []string{"app-1"}, now, future),
	}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"app-1": node, "preview-box": {capabilities: previewCapabilities}}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	// max is 2 and three previews exist (max was lowered): existing ones
	// still redeploy; only a new one is refused.
	if err := e.preparePreviewDeploy(context.Background(), newPreviewDeploySession(t, e, "pr-1", &DeployPreviewOptions{NoPrune: true})); err != nil {
		t.Fatalf("redeploy of pr-1: %v", err)
	}
	if err := e.preparePreviewDeploy(context.Background(), newPreviewDeploySession(t, e, "pr-4", &DeployPreviewOptions{NoPrune: true})); Classify(err) != ClassInvalid {
		t.Fatalf("new preview over the limit: err = %v", err)
	}
}

func TestPrunePreviewsSkipsPreviewRedeployedAfterListing(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	expired := previewRecord("old", "preview", []string{"a"}, now.Add(-200*time.Hour), timePointer(now.Add(-time.Hour)))
	node := &fakePreviewNode{capabilities: previewCapabilities, records: []takod.PreviewRecord{expired}}
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{"a": node}}
	// A deploy of the preview finishes between the prune's listing and its
	// lease: the record now carries a new expiry.
	fleet.onAcquire = func(env string) {
		if env == "preview-old" {
			node.records = []takod.PreviewRecord{previewRecord("old", "preview", []string{"a"}, now, timePointer(now.Add(72*time.Hour)))}
		}
	}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	result, err := e.PrunePreviews(context.Background(), PreviewPruneRequest{Config: previewTestConfig("a"), Now: now})
	if err != nil {
		t.Fatalf("PrunePreviews: %v", err)
	}
	if len(result.Destroyed) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Name != "old" {
		t.Fatalf("result = %#v, want the redeployed preview skipped", result)
	}
	if requests := node.cleanupRequests(); len(requests) != 0 {
		t.Fatalf("redeployed preview was torn down: %#v", requests)
	}
}

func TestPreviewRecordWrittenBeforeMutationAndRefreshedAfterSuccess(t *testing.T) {
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"app-1":       {capabilities: previewCapabilities},
		"preview-box": {capabilities: previewCapabilities},
	}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := newPreviewDeploySession(t, e, "pr-9", &DeployPreviewOptions{NoPrune: true, TTL: "1h", CreatedBy: "ci"})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	session.preview.clientFor = fleet.clientFor
	session.mutationServerNames = []string{"app-1", "preview-box"}
	session.gitStrings = GitStrings{Branch: "feature/login", ShortHash: "abc1234"}

	if err := session.writePreviewRecordBeforeMutation(context.Background()); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	for _, server := range []string{"app-1", "preview-box"} {
		writes := fleet.nodes[server].recordWrites()
		if len(writes) != 1 {
			t.Fatalf("%s got %d record writes before mutation", server, len(writes))
		}
		record := writes[0]
		if record.Name != "pr-9" || record.Base != "preview" || record.TTL != "1h" || record.CreatedBy != "ci" || !record.CreatedAt.Equal(record.UpdatedAt) || record.ExpiresAt == nil {
			t.Fatalf("%s first record = %#v", server, record)
		}
		if record.Hosts["web"] != "pr-9.preview.example.com" {
			t.Fatalf("hosts = %v", record.Hosts)
		}
	}
	created := session.preview.record.CreatedAt
	if result := session.previewResult(); result == nil || !result.Created || result.TTL != "1h" {
		t.Fatalf("preview result = %#v", result)
	}

	time.Sleep(5 * time.Millisecond)
	session.refreshPreviewRecord(context.Background())
	writes := fleet.nodes["app-1"].recordWrites()
	refreshed := writes[len(writes)-1]
	if !refreshed.CreatedAt.Equal(created) || !refreshed.UpdatedAt.After(created) || refreshed.Source == nil || refreshed.Source.Branch != "feature/login" {
		t.Fatalf("refreshed record = %#v", refreshed)
	}

	// The next deploy without --ttl goes back to the template ttl, keeps
	// createdAt and unions servers; a failed redeploy (no refresh) keeps the
	// old expiry.
	next := newPreviewDeploySession(t, e, "pr-9", &DeployPreviewOptions{NoPrune: true})
	if err := e.preparePreviewDeploy(context.Background(), next); err != nil {
		t.Fatalf("prepare redeploy: %v", err)
	}
	if next.preview.existing == nil || next.preview.ttl != "72h" {
		t.Fatalf("redeploy state = %#v", next.preview)
	}
	next.preview.clientFor = fleet.clientFor
	next.mutationServerNames = []string{"app-1"}
	if err := next.writePreviewRecordBeforeMutation(context.Background()); err != nil {
		t.Fatalf("redeploy step 1: %v", err)
	}
	kept := next.preview.record
	if !kept.CreatedAt.Equal(created) || kept.TTL != "1h" || !kept.ExpiresAt.Equal(*refreshed.ExpiresAt) || strings.Join(kept.Servers, ",") != "app-1,preview-box" {
		t.Fatalf("redeploy step-1 record = %#v; want createdAt, ttl and expiry kept and servers unioned", kept)
	}
	if next.previewResult().Created {
		t.Fatal("redeploy reported created")
	}
	next.refreshPreviewRecord(context.Background())
	if next.preview.record.TTL != "72h" {
		t.Fatalf("refreshed ttl = %s, want the template ttl (the --ttl of the earlier deploy is not sticky)", next.preview.record.TTL)
	}
}

func TestPreviewDeploySkipsCleanOldImagesOnSharedServers(t *testing.T) {
	fleet := &fakePreviewFleet{nodes: map[string]*fakePreviewNode{
		"app-1":       {capabilities: previewCapabilities},
		"preview-box": {capabilities: previewCapabilities},
	}}
	e, _ := newPreviewTestEngine(t)
	fleet.install(e)
	session := newPreviewDeploySession(t, e, "pr-1", &DeployPreviewOptions{NoPrune: true})
	if err := e.preparePreviewDeploy(context.Background(), session); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if session.cleanOldImagesOn("app-1") {
		t.Fatal("preview deploy would prune old images on app-1, which production uses")
	}
	if !session.cleanOldImagesOn("preview-box") {
		t.Fatal("a server only previews use may prune old images")
	}
	normal := &DeploySession{}
	if !normal.cleanOldImagesOn("app-1") {
		t.Fatal("non-preview deploys keep image cleanup")
	}
}
