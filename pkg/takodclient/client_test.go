package takodclient

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildRequestCommandWithBodyStreamsStdin(t *testing.T) {
	got := buildRequestCommand("/run/tako/takod.sock", "POST", "/v1/state?document=desired", true)

	if !strings.Contains(got, "--data-binary @-") {
		t.Fatalf("request body should stream from stdin: %s", got)
	}
	for _, disallowed := range []string{"/tmp/tako-takod", "rm -f"} {
		if strings.Contains(got, disallowed) {
			t.Fatalf("request command should not stage remote temp files (%s): %s", disallowed, got)
		}
	}
}

func TestBuildRequestCommandWithoutBodyOmitsStdinBody(t *testing.T) {
	got := buildRequestCommand("/run/tako/takod.sock", "GET", "/v1/health", false)

	if strings.Contains(got, "--data-binary") {
		t.Fatalf("GET request should not include a request body: %s", got)
	}
	if !strings.Contains(got, "curl --silent --show-error --write-out '\\n__TAKO_HTTP_STATUS__:%{http_code}' --unix-socket '/run/tako/takod.sock' -X 'GET' 'http://takod/v1/health'") {
		t.Fatalf("unexpected request command: %s", got)
	}
}

func TestRequestJSONUsesDeadlineForGET(t *testing.T) {
	client := &fakeTakodExecutor{}

	output, err := RequestJSON(client, DefaultSocket, "GET", "/v1/health", nil)
	if err != nil {
		t.Fatalf("RequestJSON returned error: %v", err)
	}
	if output != "ok" {
		t.Fatalf("output = %q, want ok", output)
	}
	if client.contextCalls != 1 {
		t.Fatalf("ExecuteWithContext calls = %d, want 1", client.contextCalls)
	}
	if !client.deadlineWithin(JSONRequestTimeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), JSONRequestTimeout)
	}
}

func TestRequestJSONUsesDeadlineForBody(t *testing.T) {
	client := &fakeTakodExecutor{}

	_, err := RequestJSON(client, DefaultSocket, "POST", "/v1/state", map[string]string{"document": "desired"})
	if err != nil {
		t.Fatalf("RequestJSON returned error: %v", err)
	}
	if client.inputCalls != 1 {
		t.Fatalf("ExecuteWithInput calls = %d, want 1", client.inputCalls)
	}
	if !client.deadlineWithin(JSONRequestTimeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), JSONRequestTimeout)
	}
	if !strings.Contains(client.body, `"document": "desired"`) {
		t.Fatalf("body = %q, want JSON content", client.body)
	}
}

func TestSanitizeJSONOutputStripsLeadingNUL(t *testing.T) {
	got := sanitizeJSONOutput("\x00\x00{\"ok\":true}")
	if got != "{\"ok\":true}" {
		t.Fatalf("sanitized output = %q", got)
	}
}

func TestRequestJSONReturnsHTTPErrorBody(t *testing.T) {
	client := &fakeTakodExecutor{output: "container web health check failed\n__TAKO_HTTP_STATUS__:502"}

	output, err := RequestJSON(client, DefaultSocket, "POST", "/v1/reconcile-service", map[string]string{"service": "web"})
	if err == nil {
		t.Fatal("RequestJSON should return HTTP errors")
	}
	if output != "container web health check failed" {
		t.Fatalf("output = %q, want response body", output)
	}
	if !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "container web health check failed") {
		t.Fatalf("error = %q, want HTTP status and response body", err.Error())
	}
}

func TestSplitHTTPStatusSeparatesTrailingCurlStatus(t *testing.T) {
	body, status, ok := splitHTTPStatus("{\"ok\":true}\n__TAKO_HTTP_STATUS__:201")
	if !ok {
		t.Fatal("expected status marker to be detected")
	}
	if body != "{\"ok\":true}" || status != 201 {
		t.Fatalf("body=%q status=%d, want JSON body and 201", body, status)
	}
}

func TestStreamRequestUsesLongDeadline(t *testing.T) {
	client := &fakeTakodExecutor{}

	_, err := StreamRequest(client, DefaultSocket, "POST", "/v1/images/import", strings.NewReader("archive"))
	if err != nil {
		t.Fatalf("StreamRequest returned error: %v", err)
	}
	if client.inputCalls != 1 {
		t.Fatalf("ExecuteWithInput calls = %d, want 1", client.inputCalls)
	}
	if !client.deadlineWithin(StreamRequestTimeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), StreamRequestTimeout)
	}
	if client.body != "archive" {
		t.Fatalf("body = %q, want archive", client.body)
	}
}

func TestStreamRequestReturnsHTTPErrorBody(t *testing.T) {
	client := &fakeTakodExecutor{output: "docker buildx is required\n__TAKO_HTTP_STATUS__:502"}

	output, err := StreamRequest(client, DefaultSocket, "POST", "/v1/images/build", strings.NewReader("archive"))
	if err == nil {
		t.Fatal("StreamRequest should return HTTP errors")
	}
	if output != "docker buildx is required" {
		t.Fatalf("output = %q, want response body", output)
	}
	if !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "docker buildx is required") {
		t.Fatalf("error = %q, want HTTP status and response body", err.Error())
	}
}

func TestProxyFileEndpointEscapesName(t *testing.T) {
	got := ProxyFileEndpoint("demo production.yml")
	want := "/v1/proxy-file?name=demo+production.yml"
	if got != want {
		t.Fatalf("ProxyFileEndpoint() = %q, want %q", got, want)
	}
}

func TestDiscoveryEndpointEscapesQueryValues(t *testing.T) {
	got := DiscoveryEndpoint("demo app", "prod/us", "web api", 3000, true)
	want := "/v1/discovery?environment=prod%2Fus&port=3000&project=demo+app&roundRobin=true&service=web+api"
	if got != want {
		t.Fatalf("DiscoveryEndpoint() = %q, want %q", got, want)
	}
}

func TestStateEndpointEscapesQueryValues(t *testing.T) {
	got := StateEndpoint("demo app", "prod/us", "desired")
	want := "/v1/state?document=desired&environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("StateEndpoint() = %q, want %q", got, want)
	}
}

func TestStateNodeEndpointEscapesQueryValues(t *testing.T) {
	got := StateNodeEndpoint("demo app", "prod/us", "actual-node", "node/a")
	want := "/v1/state?document=actual-node&environment=prod%2Fus&node=node%2Fa&project=demo+app"
	if got != want {
		t.Fatalf("StateNodeEndpoint() = %q, want %q", got, want)
	}
}

func TestActualStateEndpointEscapesQueryValues(t *testing.T) {
	got := ActualStateEndpoint("demo app", "prod/us")
	want := "/v1/actual?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("ActualStateEndpoint() = %q, want %q", got, want)
	}
}

func TestInspectEndpointEscapesQueryValues(t *testing.T) {
	got := InspectEndpoint("demo app", "prod/us", "web api")
	want := "/v1/inspect?environment=prod%2Fus&project=demo+app&service=web+api"
	if got != want {
		t.Fatalf("InspectEndpoint() = %q, want %q", got, want)
	}
}

func TestEnvBundleEndpointEscapesQueryValues(t *testing.T) {
	got := EnvBundleEndpoint("demo app", "prod/us")
	want := "/v1/env-bundle?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("EnvBundleEndpoint() = %q, want %q", got, want)
	}
}

func TestBackupsEndpointEscapesQueryValues(t *testing.T) {
	got := BackupsEndpoint("demo app", "prod/us", "db data", "20260613-120000")
	want := "/v1/backups?backupId=20260613-120000&environment=prod%2Fus&project=demo+app&volume=db+data"
	if got != want {
		t.Fatalf("BackupsEndpoint() = %q, want %q", got, want)
	}
}

func TestImageBuildEndpointEscapesImage(t *testing.T) {
	got := ImageBuildEndpoint("demo/web:abc123")
	want := "/v1/images/build?image=demo%2Fweb%3Aabc123"
	if got != want {
		t.Fatalf("ImageBuildEndpoint() = %q, want %q", got, want)
	}
}

func TestImageBuildEndpointEscapesPlatform(t *testing.T) {
	got := ImageBuildEndpoint("demo/web:abc123", "linux/amd64")
	want := "/v1/images/build?image=demo%2Fweb%3Aabc123&platform=linux%2Famd64"
	if got != want {
		t.Fatalf("ImageBuildEndpoint() = %q, want %q", got, want)
	}
}

func TestImageBuildEndpointWithOptions(t *testing.T) {
	got := ImageBuildEndpointWithOptions("registry.example.com/demo/web:abc123", ImageBuildEndpointOptions{
		Platform:   "linux/amd64",
		Dockerfile: "Dockerfile.renderer",
		CacheFrom:  []string{"type=registry,ref=registry.example.com/demo/web:cache"},
		CacheTo:    []string{"type=registry,ref=registry.example.com/demo/web:cache,mode=max"},
		Builder:    "mesh-builder",
		Buildx:     true,
	})
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("failed to parse endpoint: %v", err)
	}
	query := parsed.Query()
	if parsed.Path != "/v1/images/build" ||
		query.Get("image") != "registry.example.com/demo/web:abc123" ||
		query.Get("platform") != "linux/amd64" ||
		query.Get("dockerfile") != "Dockerfile.renderer" ||
		query.Get("builder") != "mesh-builder" ||
		query.Get("buildx") != "true" ||
		query.Get("cacheFrom") != "type=registry,ref=registry.example.com/demo/web:cache" ||
		query.Get("cacheTo") != "type=registry,ref=registry.example.com/demo/web:cache,mode=max" {
		t.Fatalf("unexpected endpoint/query: %s", got)
	}
}

func TestImagesEndpointEscapesQueryValues(t *testing.T) {
	got := ImagesEndpoint("demo app", "prod/us")
	want := "/v1/images?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("ImagesEndpoint() = %q, want %q", got, want)
	}
}

func TestVolumesEndpointEscapesQueryValues(t *testing.T) {
	got := VolumesEndpoint("demo app", "prod/us")
	want := "/v1/volumes?environment=prod%2Fus&project=demo+app"
	if got != want {
		t.Fatalf("VolumesEndpoint() = %q, want %q", got, want)
	}
}

func TestLogsEndpointEscapesQueryValues(t *testing.T) {
	got := LogsEndpoint("demo app", "prod/us", "web api", 250, true)
	want := "/v1/logs?environment=prod%2Fus&follow=true&project=demo+app&service=web+api&tail=250"
	if got != want {
		t.Fatalf("LogsEndpoint() = %q, want %q", got, want)
	}
}

func TestNodeLogsEndpointEscapesQueryValues(t *testing.T) {
	got := NodeLogsEndpoint("tako-monitor", 250, true)
	want := "/v1/node/logs?follow=true&tail=250&unit=tako-monitor"
	if got != want {
		t.Fatalf("NodeLogsEndpoint() = %q, want %q", got, want)
	}
}

func TestNodeInfoEndpoint(t *testing.T) {
	got := NodeInfoEndpoint()
	want := "/v1/node/info"
	if got != want {
		t.Fatalf("NodeInfoEndpoint() = %q, want %q", got, want)
	}
}

func TestStatsEndpointEscapesQueryValues(t *testing.T) {
	got := StatsEndpoint("demo app", "prod/us", "web api", true)
	want := "/v1/stats?all=true&environment=prod%2Fus&project=demo+app&service=web+api"
	if got != want {
		t.Fatalf("StatsEndpoint() = %q, want %q", got, want)
	}
}

func TestMeshRTTEndpointEscapesQueryValues(t *testing.T) {
	got := MeshRTTEndpoint("10.210.0.2", 3)
	want := "/v1/mesh/rtt?count=3&target=10.210.0.2"
	if got != want {
		t.Fatalf("MeshRTTEndpoint() = %q, want %q", got, want)
	}
}

func TestMetricsEndpointWithCollect(t *testing.T) {
	got := MetricsEndpoint(true)
	want := "/v1/metrics?collect=true"
	if got != want {
		t.Fatalf("MetricsEndpoint() = %q, want %q", got, want)
	}
}

func TestPrometheusMetricsEndpointEscapesQueryValues(t *testing.T) {
	got := PrometheusMetricsEndpoint("demo app", "prod/us", true)
	want := "/v1/metrics?collect=true&environment=prod%2Fus&format=prometheus&project=demo+app"
	if got != want {
		t.Fatalf("PrometheusMetricsEndpoint() = %q, want %q", got, want)
	}
}

func TestAccessLogsEndpointWithFollow(t *testing.T) {
	got := AccessLogsEndpoint(75, true)
	want := "/v1/access-logs?follow=true&tail=75"
	if got != want {
		t.Fatalf("AccessLogsEndpoint() = %q, want %q", got, want)
	}
}

type fakeTakodExecutor struct {
	contextCalls int
	inputCalls   int
	deadline     time.Time
	startedAt    time.Time
	body         string
	output       string
	err          error
}

func (f *fakeTakodExecutor) ExecuteWithContext(ctx context.Context, _ string) (string, error) {
	f.contextCalls++
	f.captureDeadline(ctx)
	return f.response()
}

func (f *fakeTakodExecutor) ExecuteWithInput(ctx context.Context, _ string, input io.Reader) (string, error) {
	f.inputCalls++
	f.captureDeadline(ctx)
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	f.body = string(data)
	return f.response()
}

func (f *fakeTakodExecutor) ExecuteStream(_ string, _ io.Writer, _ io.Writer) error {
	return nil
}

func (f *fakeTakodExecutor) captureDeadline(ctx context.Context) {
	f.startedAt = time.Now()
	f.deadline, _ = ctx.Deadline()
}

func (f *fakeTakodExecutor) response() (string, error) {
	if f.output != "" || f.err != nil {
		return f.output, f.err
	}
	return "ok", nil
}

func (f *fakeTakodExecutor) deadlineWithin(timeout time.Duration) bool {
	if f.deadline.IsZero() {
		return false
	}
	got := f.deadline.Sub(f.startedAt)
	return got > timeout-5*time.Second && got <= timeout
}
