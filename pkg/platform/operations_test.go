package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingStructuredAgent struct {
	method   string
	endpoint string
}

func (a *recordingStructuredAgent) RequestJSON(_ context.Context, method string, endpoint string, _ any) (string, error) {
	a.method, a.endpoint = method, endpoint
	return `{}`, nil
}

type blockingOperationExecutor struct {
	mu         sync.Mutex
	running    int
	max        int
	started    chan string
	release    chan struct{}
	executed   map[string]int
	replaySafe bool
}

type uncertainOperationExecutor struct{}

func (uncertainOperationExecutor) Plan(OperationSpec) (OperationPlan, error) {
	return OperationPlan{OperationEffect: OperationEffect{DiskGrowth: true}, ReplaySafe: false}, nil
}

type largeEvidenceExecutor struct{}

func (largeEvidenceExecutor) Plan(OperationSpec) (OperationPlan, error) {
	return OperationPlan{OperationEffect: OperationEffect{DiskGrowth: true}, ReplaySafe: true}, nil
}
func (largeEvidenceExecutor) Execute(_ context.Context, spec OperationSpec) error {
	if spec.ID == "large-error" {
		return errors.New(strings.Repeat("remote evidence ", 200000))
	}
	return nil
}
func (uncertainOperationExecutor) Execute(context.Context, OperationSpec) error {
	return errors.New("transport EOF after dispatch")
}

func (e *blockingOperationExecutor) Plan(OperationSpec) (OperationPlan, error) {
	return OperationPlan{OperationEffect: OperationEffect{DiskGrowth: true}, ReplaySafe: e.replaySafe}, nil
}

func (e *blockingOperationExecutor) Execute(ctx context.Context, spec OperationSpec) error {
	e.mu.Lock()
	e.running++
	if e.running > e.max {
		e.max = e.running
	}
	if e.executed == nil {
		e.executed = make(map[string]int)
	}
	e.executed[spec.ID]++
	e.mu.Unlock()
	if e.started != nil {
		e.started <- spec.ID
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
		}
	}
	e.mu.Lock()
	e.running--
	e.mu.Unlock()
	return ctx.Err()
}

func TestOperationEngineDurablyQueuesBoundsAndCompletes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"op-1", "op-2", "op-3"} {
		if err := store.Enqueue(OperationSpec{ID: id, Kind: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Enqueue(OperationSpec{ID: "op-1", Kind: "test"}); !errors.Is(err, ErrOperationExists) {
		t.Fatalf("duplicate enqueue error = %v", err)
	}
	policy := DefaultResourcePolicy()
	policy.MaximumConcurrentOps = 2
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	executor := &blockingOperationExecutor{started: make(chan string, 3), release: make(chan struct{}, 3), replaySafe: true}
	engine, _ := NewOperationEngine(store, executor, admission, policy.MaximumConcurrentOps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	for range 2 {
		select {
		case <-executor.started:
		case <-time.After(time.Second):
			t.Fatal("two operations were not dispatched")
		}
	}
	executor.mu.Lock()
	if executor.max != 2 {
		t.Fatalf("maximum concurrent operations = %d", executor.max)
	}
	executor.mu.Unlock()
	executor.release <- struct{}{}
	executor.release <- struct{}{}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("third operation was not dispatched after capacity released")
	}
	executor.release <- struct{}{}
	waitOperationStatus(t, store, "op-3", "succeeded")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("engine exit = %v", err)
	}
}

func TestOperationEngineRecoversRunningOperationAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	if err := store.Enqueue(OperationSpec{ID: "recover-me", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	state, _ := store.readLocked("recover-me")
	state.Status, state.Phase, state.Attempts = "running", "executing", 1
	if err := store.writeLocked(*state); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	executor := &blockingOperationExecutor{replaySafe: true}
	engine, _ := NewOperationEngine(store, executor, admission, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	waitOperationStatus(t, store, "recover-me", "succeeded")
	finished, _ := store.Read("recover-me")
	executor.mu.Lock()
	executed := executor.executed["recover-me"]
	executor.mu.Unlock()
	if finished.Attempts != 2 || executed != 1 {
		t.Fatalf("recovered operation = %#v executed=%d", finished, executed)
	}
	cancel()
	<-done
}

func TestOperationEngineDoesNotReplayAmbiguousRunningOperation(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	if err := store.Enqueue(OperationSpec{ID: "unsafe", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	state, _ := store.readLocked("unsafe")
	state.Status, state.Phase, state.Attempts = "running", "executing", 1
	if err := store.writeLocked(*state); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	executor := &blockingOperationExecutor{replaySafe: false}
	if _, err := NewOperationEngine(store, executor, admission, 1); err != nil {
		t.Fatal(err)
	}
	finished, err := store.Read("unsafe")
	if err != nil || finished.Status != "ambiguous" || finished.Attempts != 1 {
		t.Fatalf("ambiguous recovery = %#v, %v", finished, err)
	}
	if len(executor.executed) != 0 {
		t.Fatalf("unsafe operation was replayed: %#v", executor.executed)
	}
}

func TestOperationRecoveryRetiresQueuedStreamsAndTerminalQueueRecords(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	payload, _ := json.Marshal(AgentRequestPayload{Method: "POST", Endpoint: "/v1/jobs/trigger"})
	if err := store.Enqueue(OperationSpec{ID: "queued-stream", Kind: "agent.stream", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(OperationSpec{ID: "terminal-in-queue", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	terminal, _ := store.readLocked("terminal-in-queue")
	terminal.Status, terminal.Phase = "succeeded", "completed"
	if err := store.writeLocked(*terminal); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	if _, err := NewOperationEngine(store, &blockingOperationExecutor{replaySafe: true}, admission, 1); err != nil {
		t.Fatal(err)
	}
	stream, err := store.Read("queued-stream")
	if err != nil || stream.Status != "failed" || stream.Phase != "not-dispatched" {
		t.Fatalf("queued stream recovery = %#v, %v", stream, err)
	}
	terminal, err = store.Read("terminal-in-queue")
	if err != nil || terminal.Status != "succeeded" {
		t.Fatalf("terminal recovery = %#v, %v", terminal, err)
	}
	queued, _ := os.ReadDir(filepath.Join(dir, operationQueueDir))
	if len(queued) != 0 {
		t.Fatalf("recovery left %d records in live queue", len(queued))
	}
}

func TestAgentRecoveryDoesNotReplayStateAppendOrCertificateSideEffects(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	requests := map[string]AgentRequestPayload{
		"state-append": {Method: "POST", Endpoint: "/v1/state", Body: json.RawMessage(`{}`)},
		"cert-push":    {Method: "PUT", Endpoint: "/v1/certs", Body: json.RawMessage(`{}`)},
		"acme-issue":   {Method: "PUT", Endpoint: "/v1/acme-dns", Body: json.RawMessage(`{}`)},
	}
	for id, request := range requests {
		payload, _ := json.Marshal(request)
		if err := store.Enqueue(OperationSpec{ID: id, Kind: "agent.request", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		state, _ := store.readLocked(id)
		state.Status, state.Phase, state.Attempts = "running", "executing", 1
		if err := store.writeLocked(*state); err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
		store.mu.Unlock()
	}
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	agent := &recordingStructuredAgent{}
	if _, err := NewOperationEngine(store, AgentOperationExecutor{Agent: agent}, admission, 1); err != nil {
		t.Fatal(err)
	}
	for id := range requests {
		state, err := store.Read(id)
		if err != nil || state.Status != "ambiguous" || state.Attempts != 1 {
			t.Fatalf("unsafe recovery %s = %#v, %v", id, state, err)
		}
	}
	if agent.method != "" || agent.endpoint != "" {
		t.Fatalf("unsafe recovery reached agent: %s %s", agent.method, agent.endpoint)
	}
}

func TestOperationEngineMarksUnsafePostDispatchErrorAmbiguous(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	if err := store.Enqueue(OperationSpec{ID: "uncertain", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	engine, err := NewOperationEngine(store, uncertainOperationExecutor{}, admission, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	waitOperationStatus(t, store, "uncertain", "ambiguous")
	state, _ := store.Read("uncertain")
	if !strings.Contains(state.LastError, "transport EOF") {
		t.Fatalf("ambiguous state lost transport evidence: %#v", state)
	}
	cancel()
	<-done
}

func TestOperationEngineBoundsOversizedJournalEvidenceAndContinues(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
	for _, id := range []string{"large-error", "next-operation"} {
		if err := store.Enqueue(OperationSpec{ID: id, Kind: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	policy := DefaultResourcePolicy()
	admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	engine, err := NewOperationEngine(store, largeEvidenceExecutor{}, admission, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	waitOperationStatus(t, store, "large-error", "failed")
	waitOperationStatus(t, store, "next-operation", "succeeded")
	state, _ := store.Read("large-error")
	if len(state.LastError) <= int(maxJournalFrameBytes) {
		t.Fatalf("test did not retain oversized evidence in state: %d bytes", len(state.LastError))
	}
	data, err := os.ReadFile(filepath.Join(dir, DefaultJournalName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if int64(len(line)+1) > maxJournalFrameBytes || !json.Valid(line) {
			t.Fatalf("journal contains oversized or invalid frame: %d bytes", len(line))
		}
	}
	if !bytes.Contains(data, []byte("[truncated original-bytes=")) {
		t.Fatal("journal did not record truncation evidence")
	}
	cancel()
	<-done
}

func TestOperationEngineFailsUnsafePreDispatchErrorsWithoutAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name      string
		request   AgentRequestPayload
		available int64
	}{
		{name: "plan failure", request: AgentRequestPayload{Method: "POST", Endpoint: "https://invalid.example/v1/jobs/trigger"}, available: 1 << 60},
		{name: "admission denial", request: AgentRequestPayload{Method: "POST", Endpoint: "/v1/jobs/trigger", Body: json.RawMessage(`{}`)}, available: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, _ := NewOperationStore(dir, "22222222-2222-4222-8222-222222222222", time.Now)
			payload, _ := json.Marshal(test.request)
			if err := store.Enqueue(OperationSpec{ID: "unsafe", Kind: "agent.request", Payload: payload}); err != nil {
				t.Fatal(err)
			}
			policy := DefaultResourcePolicy()
			admission, _ := NewAdmissionController(policy, fixedDiskProbe{available: test.available}, dir)
			agent := &recordingStructuredAgent{}
			engine, err := NewOperationEngine(store, AgentOperationExecutor{Agent: agent}, admission, 1)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- engine.Run(ctx) }()
			waitOperationStatus(t, store, "unsafe", "failed")
			cancel()
			<-done
			if agent.method != "" {
				t.Fatalf("pre-dispatch failure reached agent: %s %s", agent.method, agent.endpoint)
			}
		})
	}
}

func TestAgentOperationPlanDerivesFreeingEffectsFromRequest(t *testing.T) {
	executor := AgentOperationExecutor{}
	for _, test := range []struct {
		name       string
		payload    AgentRequestPayload
		diskGrowth bool
		replaySafe bool
	}{
		{name: "remove", payload: AgentRequestPayload{Method: "POST", Endpoint: "/v1/remove-service", Body: json.RawMessage(`{}`)}, replaySafe: true},
		{name: "scale zero", payload: AgentRequestPayload{Method: "POST", Endpoint: "/v1/reconcile-service", Body: json.RawMessage(`{"containers":[]}`)}, replaySafe: true},
		{name: "files still grow", payload: AgentRequestPayload{Method: "POST", Endpoint: "/v1/reconcile-service", Body: json.RawMessage(`{"containers":[],"files":[{}]}`)}, diskGrowth: true, replaySafe: true},
		{name: "job trigger ambiguous", payload: AgentRequestPayload{Method: "POST", Endpoint: "/v1/jobs/trigger", Body: json.RawMessage(`{}`)}, diskGrowth: true},
		{name: "state append ambiguous", payload: AgentRequestPayload{Method: "POST", Endpoint: "/v1/state", Body: json.RawMessage(`{}`)}, diskGrowth: true},
		{name: "state replace replay safe", payload: AgentRequestPayload{Method: "PUT", Endpoint: "/v1/state", Body: json.RawMessage(`{}`)}, diskGrowth: true, replaySafe: true},
		{name: "certificate push ambiguous", payload: AgentRequestPayload{Method: "PUT", Endpoint: "/v1/certs", Body: json.RawMessage(`{}`)}, diskGrowth: true},
		{name: "acme issuance ambiguous", payload: AgentRequestPayload{Method: "PUT", Endpoint: "/v1/acme-dns", Body: json.RawMessage(`{}`)}, diskGrowth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, _ := json.Marshal(test.payload)
			plan, err := executor.Plan(OperationSpec{ID: "op", Kind: "agent.request", Payload: data})
			if err != nil || plan.DiskGrowth != test.diskGrowth || plan.ReplaySafe != test.replaySafe {
				t.Fatalf("plan = %#v, %v", plan, err)
			}
		})
	}
}

func waitOperationStatus(t *testing.T, store *OperationStore, id string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := store.Read(id)
		if err == nil && state.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not reach %s: %#v, %v", id, want, state, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentOperationExecutorAllowsOnlyStructuredV1Mutations(t *testing.T) {
	agent := &recordingStructuredAgent{}
	executor := AgentOperationExecutor{Agent: agent}
	payload, _ := json.Marshal(AgentRequestPayload{Method: "POST", Endpoint: "/v1/reconcile-service", Body: json.RawMessage(`{"project":"demo"}`)})
	if err := executor.Execute(context.Background(), OperationSpec{ID: "op", Kind: "agent.request", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if agent.method != "POST" || agent.endpoint != "/v1/reconcile-service" {
		t.Fatalf("structured request = %s %s", agent.method, agent.endpoint)
	}
	for _, payload := range []AgentRequestPayload{
		{Method: "GET", Endpoint: "/v1/status"},
		{Method: "POST", Endpoint: "https://attacker.invalid/v1/reconcile-service"},
		{Method: "POST", Endpoint: "/healthz"},
	} {
		data, _ := json.Marshal(payload)
		if err := executor.Execute(context.Background(), OperationSpec{ID: "op", Kind: "agent.request", Payload: data}); err == nil {
			t.Fatalf("unsafe operation accepted: %#v", payload)
		}
	}
}

func TestOperationStoreRejectsPendingQueueOverflow(t *testing.T) {
	store, err := NewOperationStore(t.TempDir(), "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxPendingOperations; index++ {
		spec := OperationSpec{ID: fmt.Sprintf("op-%03d", index), Kind: "test", CreatedAt: time.Now()}
		if err := store.Enqueue(spec); err != nil {
			t.Fatalf("enqueue %d: %v", index, err)
		}
	}
	if err := store.Enqueue(OperationSpec{ID: "overflow", Kind: "test", CreatedAt: time.Now()}); err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("overflow error = %v", err)
	}
}
