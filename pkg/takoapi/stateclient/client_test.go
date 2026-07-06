package stateclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

type fakeExecutor struct {
	calls  []fakeCall
	queue  []string
	ctxErr error
}

type fakeCall struct {
	cmd  string
	body string
}

func (f *fakeExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		f.ctxErr = err
		return "", err
	}
	f.calls = append(f.calls, fakeCall{cmd: cmd})
	return f.next(), nil
}

func (f *fakeExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		f.ctxErr = err
		return "", err
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	f.calls = append(f.calls, fakeCall{cmd: cmd, body: string(data)})
	return f.next(), nil
}

func (f *fakeExecutor) next() string {
	if len(f.queue) == 0 {
		return `{"found":true}`
	}
	out := f.queue[0]
	f.queue = f.queue[1:]
	return out
}

func TestWriteDesiredSendsStateRequestWithRevisionAndContentIdentity(t *testing.T) {
	exec := &fakeExecutor{}
	client := New(exec)
	doc := takoapi.NewDesiredStateDocument("demo", "production", "rev-123")
	doc.CreatedAt = time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	doc.Services["web"] = takoapi.DesiredServiceDocument{Name: "web", Replicas: 2}

	if err := client.WriteDesired(doc); err != nil {
		t.Fatalf("WriteDesired returned error: %v", err)
	}

	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'PUT'") || !strings.Contains(call.cmd, "http://takod/v1/state") {
		t.Fatalf("unexpected command: %s", call.cmd)
	}
	request := decodeRequest(t, call.body)
	if request.Project != "demo" || request.Environment != "production" || request.Document != takoapi.StateDocumentDesired || request.RevisionID != "rev-123" {
		t.Fatalf("request identity = %#v", request)
	}
	var content takoapi.DesiredStateDocument
	if err := json.Unmarshal([]byte(request.Content), &content); err != nil {
		t.Fatalf("content is not desired document JSON: %v", err)
	}
	if content.Project != doc.Project || content.Environment != doc.Environment || content.RevisionID != doc.RevisionID {
		t.Fatalf("content identity = project %q env %q revision %q", content.Project, content.Environment, content.RevisionID)
	}
}

func TestReadDesiredDecodesContent(t *testing.T) {
	doc := takoapi.NewDesiredStateDocument("demo", "production", "rev-123")
	doc.CreatedAt = time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"found": true, "content": string(content)})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{string(response)}}

	got, err := New(exec).ReadDesired("demo", "production")
	if err != nil {
		t.Fatalf("ReadDesired returned error: %v", err)
	}
	if got.Project != "demo" || got.Environment != "production" || got.RevisionID != "rev-123" {
		t.Fatalf("decoded desired = %#v", got)
	}
	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'GET'") || !strings.Contains(call.cmd, "document=desired") || !strings.Contains(call.cmd, "environment=production") || !strings.Contains(call.cmd, "project=demo") {
		t.Fatalf("unexpected command: %s", call.cmd)
	}
}

func TestReadNotFoundReturnsErrNotFound(t *testing.T) {
	exec := &fakeExecutor{queue: []string{`{"found":false}`}}
	_, err := New(exec).ReadDesired("demo", "production")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestWriteAndReadActualNodeUseNodeIdentity(t *testing.T) {
	nodeDoc := takoapi.NewActualNodeStateDocument("demo", "production", "node-a")
	nodeDoc.CapturedAt = time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	content, err := json.Marshal(nodeDoc)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"found": true, "content": string(content)})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{`{"found":true}`, string(response)}}
	client := New(exec)

	if err := client.WriteActualNode(nodeDoc); err != nil {
		t.Fatalf("WriteActualNode returned error: %v", err)
	}
	got, err := client.ReadActualNode("demo", "production", "node-a")
	if err != nil {
		t.Fatalf("ReadActualNode returned error: %v", err)
	}
	if got.Node != "node-a" || got.Project != "demo" || got.Environment != "production" {
		t.Fatalf("decoded node actual = %#v", got)
	}

	if len(exec.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(exec.calls))
	}
	request := decodeRequest(t, exec.calls[0].body)
	if request.Document != takoapi.StateDocumentActualNode || request.Node != "node-a" {
		t.Fatalf("write request = %#v", request)
	}
	var written takoapi.ActualNodeStateDocument
	if err := json.Unmarshal([]byte(request.Content), &written); err != nil {
		t.Fatalf("content is not actual-node document JSON: %v", err)
	}
	if written.Node != "node-a" {
		t.Fatalf("written content node = %q", written.Node)
	}
	if !strings.Contains(exec.calls[1].cmd, "document=actual-node") || !strings.Contains(exec.calls[1].cmd, "node=node-a") {
		t.Fatalf("read command missing node identity: %s", exec.calls[1].cmd)
	}
}

func TestWriteAndReadHistoryUseProjectNameIdentity(t *testing.T) {
	history := takoapi.DeploymentHistoryDocument{
		ProjectName: "demo",
		Environment: "production",
		Server:      "node-a",
		Deployments: []*takoapi.DeploymentStateDocument{},
		LastUpdated: time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC),
	}
	content, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"found": true, "content": string(content)})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{`{"found":true}`, string(response)}}
	client := New(exec)

	if err := client.WriteHistory(history); err != nil {
		t.Fatalf("WriteHistory returned error: %v", err)
	}
	got, err := client.ReadHistory("demo", "production")
	if err != nil {
		t.Fatalf("ReadHistory returned error: %v", err)
	}
	if got.ProjectName != "demo" || got.Environment != "production" || got.Server != "node-a" {
		t.Fatalf("decoded history = %#v", got)
	}

	if len(exec.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(exec.calls))
	}
	request := decodeRequest(t, exec.calls[0].body)
	if request.Project != "demo" || request.Environment != "production" || request.Document != takoapi.StateDocumentHistory || request.RevisionID != "" {
		t.Fatalf("history write request = %#v", request)
	}
	var written takoapi.DeploymentHistoryDocument
	if err := json.Unmarshal([]byte(request.Content), &written); err != nil {
		t.Fatalf("content is not history document JSON: %v", err)
	}
	if written.ProjectName != "demo" || written.Environment != "production" {
		t.Fatalf("history content identity = %#v", written)
	}
	if !strings.Contains(exec.calls[1].cmd, "document=history") || !strings.Contains(exec.calls[1].cmd, "environment=production") || !strings.Contains(exec.calls[1].cmd, "project=demo") {
		t.Fatalf("read command missing history identity: %s", exec.calls[1].cmd)
	}
}

func TestWriteAndReadDeploymentUseIDAsRevision(t *testing.T) {
	deployment := takoapi.DeploymentStateDocument{
		ID:          "deploy_123",
		Timestamp:   time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC),
		ProjectName: "demo",
		Environment: "production",
		Version:     "rev-1",
		Status:      takoapi.StatusSuccess,
		Services:    map[string]takoapi.ServiceStateDocument{"web": {Name: "web", Replicas: 1}},
		User:        "alice",
		Host:        "node-a",
		Duration:    time.Second,
		Message:     "deployed",
	}
	content, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"found": true, "content": string(content)})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{`{"found":true}`, string(response)}}
	client := New(exec)

	if err := client.WriteDeployment(deployment); err != nil {
		t.Fatalf("WriteDeployment returned error: %v", err)
	}
	got, err := client.ReadDeployment("demo", "production", "deploy_123")
	if err != nil {
		t.Fatalf("ReadDeployment returned error: %v", err)
	}
	if got.ID != "deploy_123" || got.ProjectName != "demo" || got.Environment != "production" || got.Status != takoapi.StatusSuccess {
		t.Fatalf("decoded deployment = %#v", got)
	}

	if len(exec.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(exec.calls))
	}
	request := decodeRequest(t, exec.calls[0].body)
	if request.Project != "demo" || request.Environment != "production" || request.Document != takoapi.StateDocumentDeployment || request.RevisionID != "deploy_123" {
		t.Fatalf("deployment write request = %#v", request)
	}
	var written takoapi.DeploymentStateDocument
	if err := json.Unmarshal([]byte(request.Content), &written); err != nil {
		t.Fatalf("content is not deployment document JSON: %v", err)
	}
	if written.ID != "deploy_123" || written.ProjectName != "demo" || written.Environment != "production" {
		t.Fatalf("deployment content identity = %#v", written)
	}
	if !strings.Contains(exec.calls[1].cmd, "document=deployment") || !strings.Contains(exec.calls[1].cmd, "revisionId=deploy_123") {
		t.Fatalf("read command missing deployment identity: %s", exec.calls[1].cmd)
	}
}

func TestReplicateDeploymentWritesDeploymentThenHistory(t *testing.T) {
	exec := &fakeExecutor{}
	client := New(exec)
	deployment := takoapi.DeploymentStateDocument{
		ID:          "deploy_123",
		ProjectName: "demo",
		Environment: "production",
		Status:      takoapi.StatusSuccess,
		Services:    map[string]takoapi.ServiceStateDocument{"web": {Name: "web", Replicas: 1}},
	}
	history := &takoapi.DeploymentHistoryDocument{
		ProjectName: "demo",
		Environment: "production",
		Deployments: []*takoapi.DeploymentStateDocument{&deployment},
	}

	if err := client.ReplicateDeploymentContext(context.Background(), deployment, history); err != nil {
		t.Fatalf("ReplicateDeploymentContext returned error: %v", err)
	}

	if len(exec.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(exec.calls))
	}
	deploymentRequest := decodeRequest(t, exec.calls[0].body)
	if deploymentRequest.Document != takoapi.StateDocumentDeployment || deploymentRequest.RevisionID != "deploy_123" {
		t.Fatalf("first request = %#v, want deployment write", deploymentRequest)
	}
	historyRequest := decodeRequest(t, exec.calls[1].body)
	if historyRequest.Document != takoapi.StateDocumentHistory || historyRequest.RevisionID != "" {
		t.Fatalf("second request = %#v, want history write", historyRequest)
	}
}

func TestReplicateDeploymentSkipsNilHistory(t *testing.T) {
	exec := &fakeExecutor{}
	deployment := takoapi.DeploymentStateDocument{
		ID:          "deploy_123",
		ProjectName: "demo",
		Environment: "production",
	}

	if err := New(exec).ReplicateDeployment(deployment, nil); err != nil {
		t.Fatalf("ReplicateDeployment returned error: %v", err)
	}

	call := onlyCall(t, exec)
	request := decodeRequest(t, call.body)
	if request.Document != takoapi.StateDocumentDeployment || request.RevisionID != "deploy_123" {
		t.Fatalf("request = %#v, want deployment write", request)
	}
}

func TestReplicateDeploymentStopsWhenDeploymentWriteFails(t *testing.T) {
	exec := &fakeExecutor{queue: []string{"not-json"}}
	deployment := takoapi.DeploymentStateDocument{
		ID:          "deploy_123",
		ProjectName: "demo",
		Environment: "production",
	}
	history := &takoapi.DeploymentHistoryDocument{ProjectName: "demo", Environment: "production"}

	err := New(exec).ReplicateDeploymentContext(context.Background(), deployment, history)
	if err == nil {
		t.Fatal("ReplicateDeploymentContext returned nil, want error")
	}
	if len(exec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 after deployment write failure", len(exec.calls))
	}
}

func TestReplicateDeploymentContextCancellationPropagatesToExecutor(t *testing.T) {
	exec := &fakeExecutor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deployment := takoapi.DeploymentStateDocument{
		ID:          "deploy_123",
		ProjectName: "demo",
		Environment: "production",
	}

	err := New(exec).ReplicateDeploymentContext(ctx, deployment, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReplicateDeploymentContext error = %v, want context.Canceled", err)
	}
	if !errors.Is(exec.ctxErr, context.Canceled) {
		t.Fatalf("executor ctx err = %v, want context.Canceled", exec.ctxErr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("recorded calls = %d, want 0 after canceled executor", len(exec.calls))
	}
}

func TestAppendEventSendsPostStateRequest(t *testing.T) {
	exec := &fakeExecutor{}
	event := takoapi.NewStateEventDocument("demo", "production", "deploy.started")
	event.RevisionID = "rev-123"
	event.Time = time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)

	if err := New(exec).AppendEvent(event); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'POST'") || !strings.Contains(call.cmd, "http://takod/v1/state") {
		t.Fatalf("unexpected command: %s", call.cmd)
	}
	request := decodeRequest(t, call.body)
	if request.Project != "demo" || request.Environment != "production" || request.Document != takoapi.StateDocumentEvent {
		t.Fatalf("request identity = %#v", request)
	}
	var content takoapi.StateEventDocument
	if err := json.Unmarshal([]byte(request.Content), &content); err != nil {
		t.Fatalf("content is not event document JSON: %v", err)
	}
	if content.Type != "deploy.started" || content.RevisionID != "rev-123" {
		t.Fatalf("event content = %#v", content)
	}
}

func TestContextCancellationPropagatesToExecutor(t *testing.T) {
	exec := &fakeExecutor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New(exec).ReadDesiredContext(ctx, "demo", "production")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadDesiredContext error = %v, want context.Canceled", err)
	}
	if !errors.Is(exec.ctxErr, context.Canceled) {
		t.Fatalf("executor ctx err = %v, want context.Canceled", exec.ctxErr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("recorded calls = %d, want 0 after canceled executor", len(exec.calls))
	}
}

func TestReadLeaseContextDecodesLeaseResponse(t *testing.T) {
	createdAt := time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	expiresAt := createdAt.Add(30 * time.Minute)
	response, err := json.Marshal(map[string]any{
		"found": true,
		"lease": map[string]any{
			"id":          "lease-123",
			"project":     "demo",
			"environment": "production",
			"operation":   "deploy",
			"who":         "alice@host",
			"holder":      "alice@host",
			"user":        "alice",
			"host":        "host",
			"pid":         1234,
			"acquiredAt":  createdAt.Format(time.RFC3339),
			"expiresAt":   expiresAt.Format(time.RFC3339),
			"ttlSeconds":  int64(1800),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{string(response)}}

	got, err := New(exec).ReadLeaseContext(context.Background(), "demo", "production")
	if err != nil {
		t.Fatalf("ReadLeaseContext returned error: %v", err)
	}
	if !got.Found || got.Lease == nil {
		t.Fatalf("lease response = %#v", got)
	}
	if got.Lease.ID != "lease-123" || got.Lease.Project != "demo" || got.Lease.ProjectName != "demo" || got.Lease.Environment != "production" {
		t.Fatalf("lease identity = %#v", got.Lease)
	}
	if got.Lease.Who != "alice@host" || got.Lease.Holder != "alice@host" || got.Lease.User != "alice" || got.Lease.Host != "host" || got.Lease.PID != 1234 || got.Lease.TTLSeconds != 1800 {
		t.Fatalf("lease owner fields = %#v", got.Lease)
	}
	if !got.Lease.AcquiredAt.Equal(createdAt) || !got.Lease.CreatedAt.Equal(createdAt) || !got.Lease.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("lease times = %#v", got.Lease)
	}
	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'GET'") || !strings.Contains(call.cmd, "http://takod/v1/lease?") || !strings.Contains(call.cmd, "project=demo") || !strings.Contains(call.cmd, "environment=production") {
		t.Fatalf("unexpected lease read command: %s", call.cmd)
	}
}

func TestAcquireLeaseContextSendsRequestAndDecodesResponse(t *testing.T) {
	acquiredAt := time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	response, err := json.Marshal(LeaseResponse{
		Acquired: true,
		Found:    true,
		Lease: &LeaseInfo{
			ID:          "lease-123",
			ProjectName: "demo",
			Environment: "production",
			Operation:   "deploy",
			Who:         "alice@host",
			PID:         1234,
			CreatedAt:   acquiredAt,
			ExpiresAt:   acquiredAt.Add(30 * time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{string(response)}}
	request := LeaseRequest{
		Project:     "demo",
		Environment: "production",
		ID:          "lease-123",
		Operation:   "deploy",
		Who:         "alice@host",
		PID:         1234,
		TTLSeconds:  1800,
	}

	got, err := New(exec).AcquireLeaseContext(context.Background(), request)
	if err != nil {
		t.Fatalf("AcquireLeaseContext returned error: %v", err)
	}
	if !got.Acquired || got.Lease == nil || got.Lease.ID != "lease-123" || got.Lease.Project != "demo" {
		t.Fatalf("lease acquire response = %#v", got)
	}
	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'POST'") || !strings.Contains(call.cmd, "http://takod/v1/lease") {
		t.Fatalf("unexpected lease acquire command: %s", call.cmd)
	}
	var body LeaseRequest
	if err := json.Unmarshal([]byte(call.body), &body); err != nil {
		t.Fatalf("failed to decode lease acquire body: %v", err)
	}
	if body != request {
		t.Fatalf("lease acquire body = %#v, want %#v", body, request)
	}
}

func TestReleaseLeaseContextSendsRequestAndDecodesResponse(t *testing.T) {
	released := LeaseInfo{ID: "lease-123", ProjectName: "demo", Environment: "production", Operation: "deploy"}
	response, err := json.Marshal(LeaseResponse{Found: false, Lease: &released})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{queue: []string{string(response)}}
	request := LeaseRequest{Project: "demo", Environment: "production", ID: "lease-123"}

	got, err := New(exec).ReleaseLeaseContext(context.Background(), request)
	if err != nil {
		t.Fatalf("ReleaseLeaseContext returned error: %v", err)
	}
	if got.Found || got.Lease == nil || got.Lease.ID != "lease-123" || got.Lease.Project != "demo" {
		t.Fatalf("lease release response = %#v", got)
	}
	call := onlyCall(t, exec)
	if !strings.Contains(call.cmd, "-X 'DELETE'") || !strings.Contains(call.cmd, "http://takod/v1/lease") {
		t.Fatalf("unexpected lease release command: %s", call.cmd)
	}
	var body LeaseRequest
	if err := json.Unmarshal([]byte(call.body), &body); err != nil {
		t.Fatalf("failed to decode lease release body: %v", err)
	}
	if body.Project != "demo" || body.Environment != "production" || body.ID != "lease-123" || body.Operation != "" || body.TTLSeconds != 0 {
		t.Fatalf("lease release body = %#v", body)
	}
}

func onlyCall(t *testing.T, exec *fakeExecutor) fakeCall {
	t.Helper()
	if len(exec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(exec.calls))
	}
	return exec.calls[0]
}

func decodeRequest(t *testing.T, body string) stateDocumentRequest {
	t.Helper()
	var request stateDocumentRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("failed to decode request body %q: %v", body, err)
	}
	return request
}
