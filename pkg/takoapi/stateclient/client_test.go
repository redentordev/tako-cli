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
	calls []fakeCall
	queue []string
}

type fakeCall struct {
	cmd  string
	body string
}

func (f *fakeExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	f.calls = append(f.calls, fakeCall{cmd: cmd})
	return f.next(), nil
}

func (f *fakeExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
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
