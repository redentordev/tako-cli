package takodclient

import (
	"context"
	"io"
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
	if !strings.Contains(got, "curl --fail --silent --show-error --unix-socket '/run/tako/takod.sock' -X 'GET' 'http://takod/v1/health'") {
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

func TestProxyFileEndpointEscapesName(t *testing.T) {
	got := ProxyFileEndpoint("demo production.yml")
	want := "/v1/proxy-file?name=demo+production.yml"
	if got != want {
		t.Fatalf("ProxyFileEndpoint() = %q, want %q", got, want)
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

func TestLogsEndpointEscapesQueryValues(t *testing.T) {
	got := LogsEndpoint("demo app", "prod/us", "web api", 250, true)
	want := "/v1/logs?environment=prod%2Fus&follow=true&project=demo+app&service=web+api&tail=250"
	if got != want {
		t.Fatalf("LogsEndpoint() = %q, want %q", got, want)
	}
}

func TestStatsEndpointEscapesQueryValues(t *testing.T) {
	got := StatsEndpoint("demo app", "prod/us", "web api", true)
	want := "/v1/stats?all=true&environment=prod%2Fus&project=demo+app&service=web+api"
	if got != want {
		t.Fatalf("StatsEndpoint() = %q, want %q", got, want)
	}
}

func TestMetricsEndpointWithCollect(t *testing.T) {
	got := MetricsEndpoint(true)
	want := "/v1/metrics?collect=true"
	if got != want {
		t.Fatalf("MetricsEndpoint() = %q, want %q", got, want)
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
}

func (f *fakeTakodExecutor) ExecuteWithContext(ctx context.Context, _ string) (string, error) {
	f.contextCalls++
	f.captureDeadline(ctx)
	return "ok", nil
}

func (f *fakeTakodExecutor) ExecuteWithInput(ctx context.Context, _ string, input io.Reader) (string, error) {
	f.inputCalls++
	f.captureDeadline(ctx)
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	f.body = string(data)
	return "ok", nil
}

func (f *fakeTakodExecutor) ExecuteStream(_ string, _ io.Writer, _ io.Writer) error {
	return nil
}

func (f *fakeTakodExecutor) captureDeadline(ctx context.Context) {
	f.startedAt = time.Now()
	f.deadline, _ = ctx.Deadline()
}

func (f *fakeTakodExecutor) deadlineWithin(timeout time.Duration) bool {
	if f.deadline.IsZero() {
		return false
	}
	got := f.deadline.Sub(f.startedAt)
	return got > timeout-5*time.Second && got <= timeout
}
