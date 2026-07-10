package takodclient

import (
	"context"
	"errors"
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

func TestRequestJSONWithTimeoutUsesCustomDeadline(t *testing.T) {
	client := &fakeTakodExecutor{}
	timeout := 7 * time.Minute

	_, err := RequestJSONWithTimeout(client, DefaultSocket, "POST", "/v1/reconcile-service", map[string]string{"service": "web"}, timeout)
	if err != nil {
		t.Fatalf("RequestJSONWithTimeout returned error: %v", err)
	}
	if !client.deadlineWithin(timeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), timeout)
	}
}

func TestRequestJSONWithContextPassesCanceledContext(t *testing.T) {
	client := &fakeTakodExecutor{returnContextErr: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RequestJSONWithContext(ctx, client, DefaultSocket, "GET", "/v1/health", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if client.contextCalls != 1 {
		t.Fatalf("ExecuteWithContext calls = %d, want 1", client.contextCalls)
	}
	if !errors.Is(client.contextErr, context.Canceled) {
		t.Fatalf("executor ctx err = %v, want context.Canceled", client.contextErr)
	}
}

func TestRequestJSONWithTimeoutLegacyWrapperStillWorks(t *testing.T) {
	client := &fakeTakodExecutor{}

	output, err := RequestJSONWithTimeout(client, DefaultSocket, "GET", "/v1/health", nil, time.Second)
	if err != nil {
		t.Fatalf("RequestJSONWithTimeout returned error: %v", err)
	}
	if output != "ok" {
		t.Fatalf("output = %q, want ok", output)
	}
	if client.contextErr != nil {
		t.Fatalf("legacy wrapper ctx err = %v, want nil", client.contextErr)
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
	if !strings.Contains(client.inputCmd, "--data-binary @-") || !strings.Contains(client.inputCmd, "http://takod/v1/images/import") {
		t.Fatalf("unexpected stream request command: %s", client.inputCmd)
	}
}

func TestStreamRequestWithContextPassesCancellation(t *testing.T) {
	client := &fakeTakodExecutor{returnContextErr: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := StreamRequestWithContext(ctx, client, DefaultSocket, "POST", "/v1/images/import", strings.NewReader("archive"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if client.inputCalls != 1 {
		t.Fatalf("ExecuteWithInput calls = %d, want 1", client.inputCalls)
	}
}

func TestStreamRequestStripsTrailingHTTPStatusOnSuccess(t *testing.T) {
	client := &fakeTakodExecutor{output: "{\"image\":\"demo/web:abc\"}\n__TAKO_HTTP_STATUS__:200"}

	output, err := StreamRequest(client, DefaultSocket, "POST", "/v1/images/build", strings.NewReader("archive"))
	if err != nil {
		t.Fatalf("StreamRequest returned error: %v", err)
	}
	if output != "{\"image\":\"demo/web:abc\"}" {
		t.Fatalf("output = %q, want response body without status marker", output)
	}
}

func TestStreamRequestReturnsHTTPErrorBody(t *testing.T) {
	body := "failed to build image demo/web:abc: the --chmod option requires BuildKit"
	client := &fakeTakodExecutor{output: body + "\n__TAKO_HTTP_STATUS__:502"}

	output, err := StreamRequest(client, DefaultSocket, "POST", "/v1/images/build", strings.NewReader("archive"))
	if err == nil {
		t.Fatal("StreamRequest should return HTTP errors")
	}
	if output != body {
		t.Fatalf("output = %q, want response body", output)
	}
	if !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), body) {
		t.Fatalf("error = %q, want HTTP status and response body", err.Error())
	}
}

func TestStreamOutputBuildsCurlCommand(t *testing.T) {
	client := &fakeTakodExecutor{}

	if err := StreamOutput(client, DefaultSocket, "/v1/logs?follow=true", io.Discard, io.Discard); err != nil {
		t.Fatalf("StreamOutput returned error: %v", err)
	}
	if client.streamCalls != 1 {
		t.Fatalf("ExecuteStream calls = %d, want 1", client.streamCalls)
	}
	if !strings.Contains(client.streamCmd, "curl --fail --silent --show-error --no-buffer") || !strings.Contains(client.streamCmd, "http://takod/v1/logs?follow=true") {
		t.Fatalf("unexpected stream output command: %s", client.streamCmd)
	}
}

func TestStreamOutputWithContextCancelsContextAwareExecutor(t *testing.T) {
	client := &blockingStreamExecutor{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-client.started
		cancel()
	}()

	start := time.Now()
	err := StreamOutputWithContext(ctx, client, DefaultSocket, "/v1/logs?follow=true", io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %s, want under 1s", elapsed)
	}
	if client.streamContextCalls != 1 {
		t.Fatalf("ExecuteStreamWithContext calls = %d, want 1", client.streamContextCalls)
	}
	if !strings.Contains(client.streamCmd, "http://takod/v1/logs?follow=true") {
		t.Fatalf("unexpected stream output command: %s", client.streamCmd)
	}
}

func TestStreamOutputWithContextFallbackReturnsAlreadyCanceledContext(t *testing.T) {
	client := &fakeTakodExecutor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := StreamOutputWithContext(ctx, client, DefaultSocket, "/v1/logs", io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if client.streamCalls != 0 {
		t.Fatalf("ExecuteStream calls = %d, want 0", client.streamCalls)
	}
}

func TestProxyFileEndpointEscapesName(t *testing.T) {
	got := ProxyFileEndpoint("demo production.json")
	want := "/v1/proxy-file?name=demo+production.json"
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

func TestImageBuildEndpointEscapesDockerfile(t *testing.T) {
	got := ImageBuildEndpoint("demo/web:abc123", "packages/web/Dockerfile")
	want := "/v1/images/build?dockerfile=packages%2Fweb%2FDockerfile&image=demo%2Fweb%3Aabc123"
	if got != want {
		t.Fatalf("ImageBuildEndpoint() = %q, want %q", got, want)
	}
}

func TestImageBuildEndpointWithOptionsKeepsValuesOutOfURL(t *testing.T) {
	got := ImageBuildEndpointWithOptions("demo/web:abc123", "packages/web/Dockerfile", true)
	want := "/v1/images/build?dockerfile=packages%2Fweb%2FDockerfile&image=demo%2Fweb%3Aabc123&options=preamble&auth=preamble"
	if got != want {
		t.Fatalf("ImageBuildEndpointWithOptions() = %q, want %q", got, want)
	}
	if strings.Contains(got, "SENTRY_IMAGE") {
		t.Fatalf("build arg value leaked into URL: %q", got)
	}
}

func TestImageExistsEndpointEscapesImage(t *testing.T) {
	got := ImageExistsEndpoint("demo/web:abc123")
	want := "/v1/images/exists?image=demo%2Fweb%3Aabc123"
	if got != want {
		t.Fatalf("ImageExistsEndpoint() = %q, want %q", got, want)
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
	contextCalls     int
	inputCalls       int
	streamCalls      int
	deadline         time.Time
	startedAt        time.Time
	body             string
	inputCmd         string
	streamCmd        string
	output           string
	err              error
	contextErr       error
	returnContextErr bool
}

func (f *fakeTakodExecutor) ExecuteWithContext(ctx context.Context, _ string) (string, error) {
	f.contextCalls++
	f.captureDeadline(ctx)
	return f.response()
}

func (f *fakeTakodExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	f.inputCalls++
	f.inputCmd = cmd
	f.captureDeadline(ctx)
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	f.body = string(data)
	return f.response()
}

func (f *fakeTakodExecutor) ExecuteStream(cmd string, _ io.Writer, _ io.Writer) error {
	f.streamCalls++
	f.streamCmd = cmd
	return f.err
}

func (f *fakeTakodExecutor) captureDeadline(ctx context.Context) {
	f.startedAt = time.Now()
	f.deadline, _ = ctx.Deadline()
	f.contextErr = ctx.Err()
}

func (f *fakeTakodExecutor) response() (string, error) {
	if f.returnContextErr && f.contextErr != nil {
		return "", f.contextErr
	}
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

type blockingStreamExecutor struct {
	fakeTakodExecutor
	started            chan struct{}
	streamContextCalls int
}

func (f *blockingStreamExecutor) ExecuteStreamWithContext(ctx context.Context, cmd string, _ io.Writer, _ io.Writer) error {
	f.streamContextCalls++
	f.streamCmd = cmd
	close(f.started)
	<-ctx.Done()
	return ctx.Err()
}
