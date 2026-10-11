package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestStreamLogNodesWithRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []LogNodeResult, 1)
	go func() {
		resultsDone <- StreamLogNodesWith(context.Background(), servers, func(serverName string, _ config.ServerConfig, prefix bool) error {
			if !prefix {
				return fmt.Errorf("expected multi-node stream to request prefixes")
			}
			started <- serverName
			<-release
			return nil
		})
	}()

	waitForEngineLogStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].ServerName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].ServerName, want)
		}
		if results[i].Err != nil {
			t.Fatalf("result %d err = %v", i, results[i].Err)
		}
	}
}

func TestStreamLogNodesWithLimitBoundsInFlightStreams(t *testing.T) {
	servers := map[string]config.ServerConfig{}
	for i := 0; i < 12; i++ {
		servers[fmt.Sprintf("node-%02d", i)] = config.ServerConfig{}
	}
	var inFlight, maxInFlight atomic.Int32
	results := StreamLogNodesWithLimit(context.Background(), servers, logsNonFollowNodeConcurrency, func(string, config.ServerConfig, bool) error {
		current := inFlight.Add(1)
		for {
			seen := maxInFlight.Load()
			if current <= seen || maxInFlight.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if got := maxInFlight.Load(); got != logsNonFollowNodeConcurrency {
		t.Fatalf("max in-flight = %d, want %d", got, logsNonFollowNodeConcurrency)
	}
	if len(results) != 12 || results[0].ServerName != "node-00" || results[11].ServerName != "node-11" {
		t.Fatalf("results not in sorted order: %#v", results)
	}
}

func TestSummarizeLogStreamResultsReportsSortedErrors(t *testing.T) {
	err := SummarizeLogStreamResults([]LogNodeResult{
		{ServerName: "node-b", Err: fmt.Errorf("second")},
		{ServerName: "node-a", Err: fmt.Errorf("first")},
	})
	if err == nil {
		t.Fatal("expected all-node failure")
	}
	message := err.Error()
	if !strings.Contains(message, "failed to stream logs from all target nodes") {
		t.Fatalf("unexpected error message: %s", message)
	}
	if strings.Index(message, "node-a") > strings.Index(message, "node-b") {
		t.Fatalf("expected node errors to be sorted: %s", message)
	}
}

func TestSummarizeLogStreamResultsClassifiesExitCodes(t *testing.T) {
	connectivity := &ConnectivityError{Err: errors.New("dial failed")}
	capability := &takodclient.CapabilityRequiredError{Server: "node-a", Capability: takodclient.CapabilityLogsStreamV1, Feature: logsStreamFeature}
	tests := []struct {
		name    string
		results []LogNodeResult
		want    Class
	}{
		{name: "success", results: []LogNodeResult{{ServerName: "a"}, {ServerName: "b"}}, want: ClassNone},
		{name: "partial", results: []LogNodeResult{{ServerName: "a"}, {ServerName: "b", Err: connectivity}}, want: ClassAttention},
		{name: "partial capability", results: []LogNodeResult{{ServerName: "a"}, {ServerName: "b", Err: capability}}, want: ClassAttention},
		{name: "all connectivity", results: []LogNodeResult{{ServerName: "a", Err: connectivity}, {ServerName: "b", Err: connectivity}}, want: ClassConnectivity},
		{name: "all capability", results: []LogNodeResult{{ServerName: "a", Err: capability}, {ServerName: "b", Err: capability}}, want: ClassInvalid},
		{name: "mixed", results: []LogNodeResult{{ServerName: "a", Err: connectivity}, {ServerName: "b", Err: errors.New("boom")}}, want: ClassFailed},
		{name: "mixed capability", results: []LogNodeResult{{ServerName: "a", Err: connectivity}, {ServerName: "b", Err: capability}}, want: ClassFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(SummarizeLogStreamResults(tt.results)); got != tt.want {
				t.Fatalf("class = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLogsResultJSONShape(t *testing.T) {
	result := LogsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindLogsResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Services:    []string{"web"},
		Tail:        25,
		Since:       "15m",
		Grep:        "error",
		Status:      logsStatusSuccess,
		Nodes:       []LogsNodeResult{{Name: "node-a", Host: "node-a.example.test", Status: logsStatusSuccess, Lines: 3, Sources: 2}},
		StartedAt:   time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:    1.25,
		Message:     "streamed logs from 1 node(s)",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{
		`"apiVersion":"tako.redentor.dev/v1alpha1"`, `"kind":"LogsResult"`, `"project":"demo"`, `"environment":"production"`,
		`"service":"web"`, `"tail":25`, `"follow":false`, `"status":"success"`, `"nodes":[{"name":"node-a","host":"node-a.example.test","status":"success","lines":3,"sources":2}]`,
		`"startedAt":"2026-07-06T12:00:00Z"`, `"durationSeconds":1.25`, `"message":"streamed logs from 1 node(s)"`,
		`"services":["web"]`, `"since":"15m"`, `"grep":"error"`, `"ignoreCase":false`, `"timestamps":false`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("result JSON missing %s: %s", want, jsonText)
		}
	}
}

// fakeLogsNode scripts one takod node for StreamLogs tests.
type fakeLogsNode struct {
	mu              sync.Mutex
	supports        bool
	probeErr        error
	frames          []takodclient.LogsStreamFrame
	streamErr       error
	follow          chan takodclient.LogsStreamFrame
	legacyLines     []string
	legacyErr       error
	requests        []takod.LogsStreamRequest
	legacyEndpoints []string
}

func (n *fakeLogsNode) SupportsLogsStream(context.Context) (bool, error) {
	return n.supports, n.probeErr
}

func (n *fakeLogsNode) StreamFrames(ctx context.Context, request takod.LogsStreamRequest, handle func(takodclient.LogsStreamFrame) error) error {
	n.mu.Lock()
	n.requests = append(n.requests, request)
	n.mu.Unlock()
	for _, frame := range n.frames {
		if err := handle(frame); err != nil {
			return err
		}
	}
	if n.follow != nil {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case frame := <-n.follow:
				if err := handle(frame); err != nil {
					return err
				}
			}
		}
	}
	return n.streamErr
}

func (n *fakeLogsNode) StreamLegacy(_ context.Context, endpoint string, handle func(string) error) error {
	n.mu.Lock()
	n.legacyEndpoints = append(n.legacyEndpoints, endpoint)
	n.mu.Unlock()
	for _, line := range n.legacyLines {
		if err := handle(line); err != nil {
			return err
		}
	}
	return n.legacyErr
}

func logsTestConfig(nodes ...string) *config.Config {
	servers := map[string]config.ServerConfig{}
	for _, node := range nodes {
		servers[node] = config.ServerConfig{Host: node + ".example.test"}
	}
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: servers,
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: nodes,
				Services: map[string]config.ServiceConfig{
					"web":     {Image: "web", Replicas: 2, Env: map[string]string{"API_TOKEN": "literal-env-secret"}},
					"worker":  {Image: "worker"},
					"api":     {Image: "api"},
					"nightly": {Image: "jobs", Kind: config.ServiceKindJob, Schedule: "@daily"},
					"migrate": {Image: "jobs", Kind: config.ServiceKindRun},
				},
			},
		},
	}
}

func newLogsTestEngine(t *testing.T, nodes map[string]*fakeLogsNode) (*Engine, *events.BufferSink) {
	t.Helper()
	t.Chdir(t.TempDir())
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.logsHooks = &logsHooks{
		connect: func(_ context.Context, serverName string) (logsNode, error) {
			node, ok := nodes[serverName]
			if !ok || node == nil {
				return nil, fmt.Errorf("dial tcp %s: connection refused", serverName)
			}
			return node, nil
		},
	}
	return eng, sink
}

func lineFrame(service, slot, data string) takodclient.LogsStreamFrame {
	return takodclient.LogsStreamFrame{Type: takodclient.LogsFrameLine, Service: service, Slot: slot, Container: "demo_production_" + service + "_" + slot, Revision: "rev1", Stream: "stdout", Time: "2026-10-11T12:00:00.123456789Z", Data: data}
}

func attachedFrame(service, slot string) takodclient.LogsStreamFrame {
	return takodclient.LogsStreamFrame{Type: takodclient.LogsFrameSource, Event: "attached", Service: service, Slot: slot}
}

func eventsOfType(sink *events.BufferSink, eventType string) []events.Event {
	var out []events.Event
	for _, event := range sink.Events() {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

func logLineEvents(sink *events.BufferSink) []events.Event {
	var out []events.Event
	for _, event := range eventsOfType(sink, events.TypeLogLine) {
		if event.Level != events.LevelDebug {
			out = append(out, event)
		}
	}
	return out
}

func TestStreamLogsRejectsInvalidRequestsBeforeContactingNodes(t *testing.T) {
	tests := []struct {
		name    string
		req     LogsRequest
		wantErr string
	}{
		{name: "invalid regex", req: LogsRequest{Grep: "("}, wantErr: "invalid grep pattern"},
		{name: "regex too long", req: LogsRequest{Grep: strings.Repeat("x", 1025)}, wantErr: "exceeds 1024 bytes"},
		{name: "bad since", req: LogsRequest{Since: "yesterday"}, wantErr: "invalid --since value"},
		{name: "bad until", req: LogsRequest{Until: "tomorrow"}, wantErr: "invalid --until value"},
		{name: "until with follow", req: LogsRequest{Until: "2026-10-01T00:00:00Z", Follow: true}, wantErr: "--until cannot be combined with --follow"},
		{name: "until before since", req: LogsRequest{Since: "2026-10-02T00:00:00Z", Until: "2026-10-01T00:00:00Z"}, wantErr: "--until must not be earlier"},
		{name: "unknown service", req: LogsRequest{Services: []string{"nope"}}, wantErr: "service nope not found in environment production"},
		{name: "run service", req: LogsRequest{Service: "migrate"}, wantErr: "deploy-time run"},
		{name: "negative tail", req: LogsRequest{Tail: -1, TailExplicit: true}, wantErr: "tail cannot be negative"},
		{name: "tail too large", req: LogsRequest{Tail: 10001, TailExplicit: true}, wantErr: "tail cannot exceed 10000"},
		{name: "unknown server", req: LogsRequest{Server: "ghost"}, wantErr: "server ghost not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contacted := false
			eng, _ := newLogsTestEngine(t, nil)
			eng.logsHooks.connect = func(context.Context, string) (logsNode, error) {
				contacted = true
				return nil, errors.New("unexpected")
			}
			req := tt.req
			req.Config = logsTestConfig("node-a")
			req.Environment = "production"
			result, err := eng.StreamLogs(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if Classify(err) != ClassInvalid {
				t.Fatalf("class = %d, want invalid (2): %v", Classify(err), err)
			}
			if result != nil || contacted {
				t.Fatalf("invalid request contacted nodes or returned a result: %#v", result)
			}
		})
	}
}

func TestPlanLogsRequestResolvesServices(t *testing.T) {
	services, _ := logsTestConfig("node-a").GetServices("production")
	tests := []struct {
		name         string
		req          LogsRequest
		wantSelected []string
		wantServices []string
		wantJobs     []string
	}{
		{name: "empty selects long-running", wantSelected: []string{"api", "web", "worker"}, wantServices: []string{"api", "web", "worker"}},
		{name: "flags args and commas dedupe", req: LogsRequest{Service: "api", Services: []string{"web,worker", "web", " "}}, wantSelected: []string{"api", "web", "worker"}, wantServices: []string{"api", "web", "worker"}},
		{name: "named job", req: LogsRequest{Services: []string{"nightly", "web"}}, wantSelected: []string{"nightly", "web"}, wantServices: []string{"web"}, wantJobs: []string{"nightly"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planLogsRequest(tt.req, services, "production")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(plan.selected, tt.wantSelected) || !slices.Equal(plan.services, tt.wantServices) || !slices.Equal(plan.jobs, tt.wantJobs) {
				t.Fatalf("plan selected=%v services=%v jobs=%v", plan.selected, plan.services, plan.jobs)
			}
		})
	}

	plan, _ := planLogsRequest(LogsRequest{Tail: 100, Since: "15m"}, services, "production")
	if plan.tail != logsMaxTail {
		t.Fatalf("since without explicit tail = %d, want %d", plan.tail, logsMaxTail)
	}
	plan, _ = planLogsRequest(LogsRequest{Tail: 20, TailExplicit: true, Since: "15m"}, services, "production")
	if plan.tail != 20 {
		t.Fatalf("explicit tail = %d, want 20", plan.tail)
	}
	plan, _ = planLogsRequest(LogsRequest{}, services, "production")
	if plan.tail != logsDefaultTail {
		t.Fatalf("default tail = %d", plan.tail)
	}
	plan, _ = planLogsRequest(LogsRequest{Since: "1790000000"}, services, "production")
	if plan.since != "2026-09-21T14:13:20Z" {
		t.Fatalf("unix since normalized to %q", plan.since)
	}
}

func TestStreamLogsFramedEventsCarryAttributionAndRedactSecrets(t *testing.T) {
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{
		{Type: takodclient.LogsFrameReady},
		attachedFrame("web", "1"),
		attachedFrame("web", "2"),
		lineFrame("web", "1", "starting with literal-env-secret"),
		lineFrame("web", "2", "ready"),
	}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a"), Environment: "production", Service: "web", Tail: 100})
	if err != nil {
		t.Fatal(err)
	}
	lines := logLineEvents(sink)
	if len(lines) != 2 {
		t.Fatalf("log lines = %#v", lines)
	}
	first := lines[0]
	for key, want := range map[string]string{"service": "web", "node": "node-a", "replica": "1", "container": "demo_production_web_1", "revision": "rev1", "stream": "stdout", "time": "2026-10-11T12:00:00.123456789Z"} {
		if first.Data[key] != want {
			t.Fatalf("data.%s = %v, want %q (%#v)", key, first.Data[key], want, first.Data)
		}
	}
	if strings.Contains(fmt.Sprint(first.Data["data"]), "literal-env-secret") || strings.Contains(first.Message, "literal-env-secret") {
		t.Fatalf("env secret leaked: data=%q message=%q", first.Data["data"], first.Message)
	}
	// web has two replicas, so lines carry padded source prefixes.
	if first.Message != "web.1@node-a | starting with [REDACTED:env.API_TOKEN]\n" || first.Data["data"] != "starting with [REDACTED:env.API_TOKEN]" {
		t.Fatalf("message = %q data = %q", first.Message, first.Data["data"])
	}
	if strings.Contains(first.Message, "\x1b[") {
		t.Fatalf("engine message must not contain ANSI escapes: %q", first.Message)
	}
	if result.Status != logsStatusSuccess || result.Service != "web" || !slices.Equal(result.Services, []string{"web"}) {
		t.Fatalf("result = %#v", result)
	}
	if got := result.Nodes[0]; got.Lines != 2 || got.Sources != 2 {
		t.Fatalf("node counts = %#v", got)
	}
	request := node.requests[0]
	if !slices.Equal(request.Services, []string{"web"}) || request.Tail == nil || *request.Tail != 100 || request.Follow {
		t.Fatalf("stream request = %#v", request)
	}
}

func TestStreamLogsSingleSourceIsUnprefixedAndTimestampsPrepend(t *testing.T) {
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("worker", "1"), lineFrame("worker", "1", "hello")}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a"), Environment: "production", Service: "worker"}); err != nil {
		t.Fatal(err)
	}
	if got := logLineEvents(sink)[0].Message; got != "hello\n" {
		t.Fatalf("single-source message = %q", got)
	}

	eng, sink = newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a"), Environment: "production", Service: "worker", Timestamps: true}); err != nil {
		t.Fatal(err)
	}
	if got := logLineEvents(sink)[0].Message; got != "2026-10-11T12:00:00.123456789Z hello\n" {
		t.Fatalf("timestamped message = %q", got)
	}
}

func TestStreamLogsAllServicesOneStreamPerNode(t *testing.T) {
	nodeA := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("api", "1"), lineFrame("api", "1", "a")}}
	nodeB := &fakeLogsNode{supports: true}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": nodeA, "node-b": nodeB})
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a", "node-b"), Environment: "production"})
	if err != nil {
		t.Fatalf("service absent on node-b must not fail: %v", err)
	}
	if len(nodeA.requests) != 1 || !slices.Equal(nodeA.requests[0].Services, []string{"api", "web", "worker"}) || len(nodeA.requests[0].IncludeJobs) != 0 {
		t.Fatalf("node-a requests = %#v", nodeA.requests)
	}
	if result.Service != "" || !slices.Equal(result.Services, []string{"api", "web", "worker"}) || len(result.Nodes) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Nodes[1].Status != logsStatusSuccess || result.Nodes[1].Lines != 0 {
		t.Fatalf("node-b = %#v", result.Nodes[1])
	}
	if got := logLineEvents(sink)[0].Message; !strings.HasPrefix(got, "api.1@node-a") {
		t.Fatalf("multi-source message = %q", got)
	}
}

func TestStreamLogsNoContainersAnywhereFails(t *testing.T) {
	eng, _ := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": {supports: true}, "node-b": {legacyErr: errLogsNoContainers}})
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a", "node-b"), Environment: "production", Service: "worker"})
	if err == nil || !strings.Contains(err.Error(), "no containers found for worker") || Classify(err) != ClassFailed {
		t.Fatalf("err = %v", err)
	}
	if result.Status != logsStatusFailed {
		t.Fatalf("status = %q", result.Status)
	}
}

func TestStreamLogsPartialFailureWarnsAndExitsAttention(t *testing.T) {
	nodeA := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("worker", "1"), lineFrame("worker", "1", "a")}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": nodeA, "node-c": nodeA})
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a", "node-b", "node-c"), Environment: "production", Service: "worker"})
	if Classify(err) != ClassAttention {
		t.Fatalf("class = %d (%v), want attention", Classify(err), err)
	}
	if result.Status != logsStatusPartial || result.Nodes[1].Status != logsStatusFailed || result.Nodes[1].Error == "" {
		t.Fatalf("result = %#v", result)
	}
	warnings := eventsOfType(sink, events.TypeWarning)
	if len(warnings) != 1 || warnings[0].Node != "node-b" {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(logLineEvents(sink)) != 2 {
		t.Fatalf("healthy nodes' lines missing")
	}
}

func TestStreamLogsAllNodesUnreachableIsConnectivity(t *testing.T) {
	eng, _ := newLogsTestEngine(t, nil)
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: logsTestConfig("node-a", "node-b"), Environment: "production", Service: "worker"})
	if Classify(err) != ClassConnectivity || result.Status != logsStatusFailed {
		t.Fatalf("class = %d status = %q err = %v", Classify(err), result.Status, err)
	}
}

func TestStreamLogsLegacyAgentFallbackAndCapabilityGate(t *testing.T) {
	legacy := &fakeLogsNode{legacyLines: []string{"plain line"}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": legacy})
	cfg := logsTestConfig("node-a")
	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: cfg, Environment: "production", Service: "worker", Tail: 50, Follow: true}); err != nil {
		t.Fatalf("plain request against legacy agent failed: %v", err)
	}
	if len(legacy.legacyEndpoints) != 1 || !strings.Contains(legacy.legacyEndpoints[0], "/v1/logs?") || !strings.Contains(legacy.legacyEndpoints[0], "tail=50") || !strings.Contains(legacy.legacyEndpoints[0], "follow=true") {
		t.Fatalf("legacy endpoints = %v", legacy.legacyEndpoints)
	}
	line := logLineEvents(sink)[0]
	if line.Data["data"] != "plain line" || line.Data["replica"] != nil || line.Data["time"] != nil {
		t.Fatalf("legacy event data = %#v", line.Data)
	}

	for name, req := range map[string]LogsRequest{
		"grep":         {Service: "worker", Grep: "x"},
		"two services": {Services: []string{"worker", "api"}},
		"timestamps":   {Service: "worker", Timestamps: true},
		"since":        {Service: "worker", Since: "5m"},
	} {
		t.Run(name, func(t *testing.T) {
			legacy := &fakeLogsNode{legacyLines: []string{"unfiltered line"}}
			eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": legacy})
			req.Config = cfg
			req.Environment = "production"
			_, err := eng.StreamLogs(context.Background(), req)
			var capability *takodclient.CapabilityRequiredError
			if !errors.As(err, &capability) || Classify(err) != ClassInvalid {
				t.Fatalf("err = %v, want capability error", err)
			}
			if !strings.Contains(err.Error(), "logs.stream-v1") || !strings.Contains(err.Error(), "tako upgrade servers") {
				t.Fatalf("capability message = %q", err.Error())
			}
			if len(legacy.legacyEndpoints) != 0 || len(logLineEvents(sink)) != 0 {
				t.Fatal("unfiltered lines were streamed from an old agent")
			}
		})
	}
}

func TestStreamLogsFollowJobRendersRunsAndWaitsWhenIdle(t *testing.T) {
	follow := make(chan takodclient.LogsStreamFrame, 8)
	zero := 0
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{{Type: takodclient.LogsFrameReady, Sources: &zero}}, follow: follow}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-b": node})
	eng.logsHooks.jobOwner = func(_ context.Context, _ *config.Config, _ string, servers []string, job string) (string, error) {
		if job != "nightly" || !slices.Equal(servers, []string{"node-a", "node-b"}) {
			return "", fmt.Errorf("unexpected owner lookup %s %v", job, servers)
		}
		return "node-b", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var result *LogsResult
	go func() {
		var err error
		result, err = eng.StreamLogs(ctx, LogsRequest{Config: logsTestConfig("node-a", "node-b"), Environment: "production", Services: []string{"nightly"}, Follow: true})
		done <- err
	}()
	exit := 0
	for _, run := range []string{"run-1", "run-2"} {
		follow <- takodclient.LogsStreamFrame{Type: takodclient.LogsFrameRun, Event: "started", Job: "nightly", Service: "nightly", Slot: "run", Container: run}
		follow <- takodclient.LogsStreamFrame{Type: takodclient.LogsFrameLine, Service: "nightly", Slot: "run", Container: run, Data: "output of " + run}
		follow <- takodclient.LogsStreamFrame{Type: takodclient.LogsFrameRun, Event: "finished", Job: "nightly", Service: "nightly", Slot: "run", Container: run, ExitCode: &exit}
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(logLineEvents(sink)) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	err := <-done
	if Classify(err) != ClassCancelled || result.Status != logsStatusFailed {
		t.Fatalf("interrupted follow: class=%d status=%v err=%v", Classify(err), result.Status, err)
	}
	if len(node.requests) != 1 || len(node.requests[0].Services) != 0 || !slices.Equal(node.requests[0].IncludeJobs, []string{"nightly"}) || !node.requests[0].Follow {
		t.Fatalf("job follow request = %#v", node.requests)
	}
	var headers []string
	for _, event := range eventsOfType(sink, events.TypePhaseStarted) {
		if event.Level == events.LevelInfo {
			headers = append(headers, strings.TrimSpace(event.Message))
		}
	}
	want := []string{
		"Streaming logs from nightly on 1 takod node(s)...",
		"Waiting for the next run of nightly on node-b...",
		"── Run of job nightly started on node-b ──",
		"── Run of job nightly finished on node-b (exit 0) ──",
		"── Run of job nightly started on node-b ──",
		"── Run of job nightly finished on node-b (exit 0) ──",
	}
	if !slices.Equal(headers, want) {
		t.Fatalf("headers = %#v", headers)
	}
	lines := logLineEvents(sink)
	if lines[0].Message != "output of run-1\n" || lines[1].Data["replica"] != "run" {
		t.Fatalf("job lines = %#v", lines)
	}
}

func TestStreamLogsRecordedJobRunAppliesGrepTailWindowAndTimestampWarning(t *testing.T) {
	now := time.Now().UTC()
	runs := &JobRunsResult{Runs: []JobRunInfo{
		{Job: "nightly", Server: "node-a", Status: "failed", StartedAt: now.Add(-1 * time.Hour), Output: "step 1 ok\nstep 2 failed: disk\nstep 3 failed: net\nstep 4 ok\n"},
		{Job: "nightly", Server: "node-a", Status: "succeeded", StartedAt: now.Add(-30 * time.Hour), Output: "old failed run\n"},
	}}
	eng, sink := newLogsTestEngine(t, nil)
	eng.logsHooks.jobRuns = func(_ context.Context, req JobRunsRequest) (*JobRunsResult, error) {
		if req.Job != "nightly" {
			return nil, fmt.Errorf("unexpected job %s", req.Job)
		}
		return runs, nil
	}
	cfg := logsTestConfig("node-a")
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: cfg, Environment: "production", Service: "nightly", Grep: "FAILED", IgnoreCase: true, Tail: 1, TailExplicit: true, Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	lines := logLineEvents(sink)
	if len(lines) != 1 || lines[0].Data["data"] != "step 3 failed: net" || lines[0].Data["replica"] != "run" {
		t.Fatalf("job lines = %#v", lines)
	}
	if warnings := eventsOfType(sink, events.TypeWarning); len(warnings) != 1 || !strings.Contains(warnings[0].Message, "--timestamps") {
		t.Fatalf("warnings = %#v", warnings)
	}
	if result.Status != logsStatusSuccess || result.Nodes[0].Lines != 1 {
		t.Fatalf("result = %#v", result)
	}

	eng2, sink2 := newLogsTestEngine(t, nil)
	eng2.logsHooks.jobRuns = eng.logsHooks.jobRuns
	if _, err := eng2.StreamLogs(context.Background(), LogsRequest{Config: cfg, Environment: "production", Service: "nightly", Since: "48h", Until: "24h"}); err != nil {
		t.Fatal(err)
	}
	if lines := logLineEvents(sink2); len(lines) != 1 || lines[0].Data["data"] != "old failed run" {
		t.Fatalf("window-selected lines = %#v", lines)
	}
}

func waitForEngineLogStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for log fanout; saw %v", seen)
		}
	}
}
