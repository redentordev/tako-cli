package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestDecodeStateDocumentContentReturnsNotFoundSentinel(t *testing.T) {
	_, err := decodeStateDocumentContent(`{"found":false}`, "history")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentRejectsInvalidEnvelope(t *testing.T) {
	_, err := decodeStateDocumentContent(`{`, "history")
	if err == nil || !strings.Contains(err.Error(), "failed to parse takod state response") {
		t.Fatalf("error = %v, want parse error", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, should not match ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentRejectsEmptyContent(t *testing.T) {
	_, err := decodeStateDocumentContent(`{"found":true}`, "history")
	if err == nil || !strings.Contains(err.Error(), "empty takod state document history") {
		t.Fatalf("error = %v, want empty content error", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, should not match ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentReturnsContent(t *testing.T) {
	content, err := decodeStateDocumentContent(`{"found":true,"content":"{\"deployments\":[]}\n"}`, "history")
	if err != nil {
		t.Fatalf("decodeStateDocumentContent returned error: %v", err)
	}
	if content != "{\"deployments\":[]}\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestStateManagerWithRequestTimeoutUsesCustomDeadline(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"found":true,"content":"{\"deployments\":[]}\n"}`,
	}
	timeout := 7 * time.Second
	manager := (&StateManager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		projectName: "demo",
		environment: "production",
		server:      "node-a",
	}).WithRequestTimeout(timeout)

	if _, err := manager.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory returned error: %v", err)
	}
	if !client.deadlineWithin(timeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), timeout)
	}
}

func TestStateManagerLoadHistoryContextPassesCanceledContext(t *testing.T) {
	client := &fakeStateManagerExecutor{returnContextErr: true}
	manager := &StateManager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		projectName: "demo",
		environment: "production",
		server:      "node-a",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.LoadHistoryContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !errors.Is(client.contextErr, context.Canceled) {
		t.Fatalf("executor ctx err = %v, want context.Canceled", client.contextErr)
	}
}

func TestStateManagerReadLeaseContextPassesCanceledContext(t *testing.T) {
	client := &fakeStateManagerExecutor{returnContextErr: true}
	manager := &StateManager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		projectName: "demo",
		environment: "production",
		server:      "node-a",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.ReadLeaseContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !errors.Is(client.contextErr, context.Canceled) {
		t.Fatalf("executor ctx err = %v, want context.Canceled", client.contextErr)
	}
}

func TestStateManagerRenewLeaseReportsLostHolder(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"acquired":false,"found":true,"message":"lease is held","lease":{"id":"other","projectName":"demo","environment":"production"}}`,
	}
	manager := &StateManager{
		client: client, socket: "/run/tako/takod.sock",
		projectName: "demo", environment: "production", server: "node-a",
	}
	_, err := manager.RenewLeaseContext(context.Background(), &LeaseInfo{
		ID: "lease-1", Environment: "production", Operation: "deploy", Who: "tester",
	}, time.Minute)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("error = %v, want ErrLeaseLost", err)
	}
}

func TestStateManagerRenewLeaseIdentifiesLegacySameHolderResponse(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"acquired":false,"found":true,"message":"lease is held","lease":{"id":"lease-1","projectName":"demo","environment":"production"}}`,
	}
	manager := &StateManager{
		client: client, socket: "/run/tako/takod.sock",
		projectName: "demo", environment: "production", server: "node-a",
	}
	_, err := manager.RenewLeaseContext(context.Background(), &LeaseInfo{
		ID: "lease-1", Environment: "production", Operation: "deploy", Who: "tester",
	}, time.Minute)
	if !errors.Is(err, ErrLeaseRenewalUnsupported) {
		t.Fatalf("error = %v, want ErrLeaseRenewalUnsupported", err)
	}
	if errors.Is(err, ErrLeaseLost) {
		t.Fatalf("legacy same-holder response must not be reported as lease loss: %v", err)
	}
}

func TestStateManagerLeaseRequestsDistinguishAcquireFromRenew(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"acquired":true,"found":true,"lease":{"id":"lease-1","projectName":"demo","environment":"production","operation":"deploy","who":"tester","expiresAt":"2099-01-01T00:00:00Z"}}`,
	}
	manager := &StateManager{
		client: client, socket: "/run/tako/takod.sock",
		projectName: "demo", environment: "production", server: "node-a",
	}
	if _, err := manager.AcquireLeaseContext(context.Background(), "deploy", "production", time.Minute); err != nil {
		t.Fatalf("AcquireLeaseContext returned error: %v", err)
	}
	if strings.Contains(client.input, `"renew": true`) {
		t.Fatalf("acquire request unexpectedly enabled renewal: %s", client.input)
	}

	if _, err := manager.RenewLeaseContext(context.Background(), &LeaseInfo{
		ID: "lease-1", Environment: "production", Operation: "deploy", Who: "tester",
	}, time.Minute); err != nil {
		t.Fatalf("RenewLeaseContext returned error: %v", err)
	}
	if !strings.Contains(client.input, `"renew": true`) {
		t.Fatalf("renew request missing renewal marker: %s", client.input)
	}
}

func TestControllerLeaseAcquireRetriesUncertainResponseWithSameRequestID(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output:   `{"acquired":true,"found":true,"holderToken":"private","lease":{"id":"op-1","projectName":"demo","environment":"production","operation":"deploy","expiresAt":"2099-01-01T00:00:00Z"}}`,
		failures: []error{errors.New("connection reset after request commit")},
	}
	manager := &StateManager{client: client, socket: "/run/tako/takod.sock", projectName: "demo", environment: "production", server: "node-a"}
	lease, err := manager.AcquireControllerLeaseContext(context.Background(), "deploy", "production", time.Minute, []string{"node-id"})
	if err != nil || lease == nil || lease.HolderToken != "private" {
		t.Fatalf("acquire after uncertain response = %#v, %v", lease, err)
	}
	if len(client.inputs) != 2 {
		t.Fatalf("request attempts = %d, want 2", len(client.inputs))
	}
	var first, second struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(client.inputs[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(client.inputs[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.RequestID == "" || first.RequestID != second.RequestID {
		t.Fatalf("retry request IDs differ: %q vs %q", first.RequestID, second.RequestID)
	}
}

func TestControllerLeaseRenewRetriesUncertainCommittedResponse(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output:   `{"acquired":true,"found":true,"holderToken":"private","lease":{"id":"op-1","projectName":"demo","environment":"production","operation":"deploy","expiresAt":"2099-01-01T00:00:00Z"}}`,
		failures: []error{errors.New("connection reset after renewal commit")},
	}
	manager := &StateManager{client: client, socket: "/run/tako/takod.sock", projectName: "demo", environment: "production", server: "node-a"}
	original := &LeaseInfo{ID: "op-1", Environment: "production", Operation: "deploy", Who: "tester", HolderToken: "private", Fence: &nodeidentity.OperationFence{OperationID: "op-1"}}
	renewed, err := manager.RenewLeaseContext(context.Background(), original, time.Minute)
	if err != nil || renewed == nil || renewed.HolderToken != "private" {
		t.Fatalf("renew after uncertain response = %#v, %v", renewed, err)
	}
	if len(client.inputs) != 2 || client.inputs[0] != client.inputs[1] {
		t.Fatalf("renew retry did not reuse exact request: %#v", client.inputs)
	}
}

func TestReleaseLeaseUsesShortCleanupDeadline(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"released":true}`,
	}
	manager := &StateManager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		projectName: "demo",
		environment: "production",
		server:      "node-a",
	}

	err := manager.ReleaseLease(&LeaseInfo{
		ID:          "lease-1",
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("ReleaseLease returned error: %v", err)
	}
	if !client.deadlineWithin(leaseReleaseTimeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), leaseReleaseTimeout)
	}
}

func TestPruneAndSortDeploymentsDropsNilSortsAndLimits(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	got := pruneAndSortDeployments([]*DeploymentState{
		{ID: "old", Timestamp: base},
		nil,
		{ID: "new", Timestamp: base.Add(time.Hour)},
		{ID: "middle", Timestamp: base.Add(time.Minute)},
	}, 2)

	if len(got) != 2 {
		t.Fatalf("deployments = %d, want 2", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "middle" {
		t.Fatalf("deployments order = [%s %s], want [new middle]", got[0].ID, got[1].ID)
	}
}

type fakeStateManagerExecutor struct {
	output           string
	input            string
	startedAt        time.Time
	deadline         time.Time
	contextErr       error
	returnContextErr bool
	failures         []error
	inputs           []string
	calls            int
}

func (f *fakeStateManagerExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	f.captureContext(ctx)
	f.calls++
	if f.calls <= len(f.failures) {
		return "", f.failures[f.calls-1]
	}
	if f.returnContextErr && f.contextErr != nil {
		return "", f.contextErr
	}
	return f.output, nil
}

func (f *fakeStateManagerExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	f.captureContext(ctx)
	if data, err := io.ReadAll(input); err == nil {
		f.input = string(data)
		f.inputs = append(f.inputs, f.input)
	}
	f.calls++
	if f.calls <= len(f.failures) {
		return "", f.failures[f.calls-1]
	}
	if f.returnContextErr && f.contextErr != nil {
		return "", f.contextErr
	}
	return f.output, nil
}

func (f *fakeStateManagerExecutor) captureContext(ctx context.Context) {
	f.startedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = deadline
	}
	f.contextErr = ctx.Err()
}

func (f *fakeStateManagerExecutor) deadlineWithin(want time.Duration) bool {
	if f.deadline.IsZero() || f.startedAt.IsZero() {
		return false
	}
	got := f.deadline.Sub(f.startedAt)
	return got > want-time.Second && got < want+time.Second
}
