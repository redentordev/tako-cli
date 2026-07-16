package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/fileutil"
)

const operationQueueDir = "queue"

const maxJournalMessageBytes = 256 << 10

var (
	operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	ErrOperationExists = errors.New("platform operation already exists")
)

type OperationSpec struct {
	APIVersion string          `json:"apiVersion"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

type OperationState struct {
	Spec      OperationSpec `json:"spec"`
	Status    string        `json:"status"`
	Phase     string        `json:"phase"`
	Attempts  int           `json:"attempts"`
	LastError string        `json:"lastError,omitempty"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

type OperationExecutor interface {
	Plan(OperationSpec) (OperationPlan, error)
	Execute(context.Context, OperationSpec) error
}

// OperationPlan is derived by the trusted executor from the validated
// endpoint and body. Resource and replay behavior must never be accepted from
// the operation producer.
type OperationPlan struct {
	OperationEffect
	ReplaySafe bool
}

type OperationEffect struct {
	DiskGrowth bool
	Build      bool
}

type OperationStore struct {
	dir     string
	journal *Journal
	nodeID  string
	now     func() time.Time
	mu      sync.Mutex
}

func NewOperationStore(stateDir string, nodeID string, now func() time.Time) (*OperationStore, error) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("operation state directory and node ID are required")
	}
	if now == nil {
		now = time.Now
	}
	dir := filepath.Join(stateDir, operationQueueDir)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}
	journal, _ := NewJournal(filepath.Join(stateDir, DefaultJournalName))
	return &OperationStore{dir: dir, journal: journal, nodeID: nodeID, now: now}, nil
}

func (s *OperationStore) Enqueue(spec OperationSpec) error {
	if err := validateOperationSpec(&spec, s.now); err != nil {
		return err
	}
	state := OperationState{Spec: spec, Status: "queued", Phase: "queued", UpdatedAt: s.now().UTC()}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.operationPath(spec.ID)
	file, err := os.CreateTemp(s.dir, ".operation-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0640); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); errors.Is(err, os.ErrExist) {
		return ErrOperationExists
	} else if err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	if err := fileutil.SyncDirectory(s.dir); err != nil {
		return err
	}
	if err := s.appendRecord(spec.ID, spec.Kind, "queued", "queued", "operation durably queued"); err != nil {
		return err
	}
	log.Printf("platform audit: operation=%s kind=%s phase=queued status=queued", spec.ID, spec.Kind)
	return nil
}

func (s *OperationStore) recoverIncomplete(executor OperationExecutor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	states, err := s.listLocked()
	if err != nil {
		return err
	}
	for _, state := range states {
		if state.Status != "running" {
			continue
		}
		plan, planErr := executor.Plan(state.Spec)
		if planErr != nil {
			state.Status = "failed"
			state.Phase = "recovery-failed"
			state.LastError = planErr.Error()
		} else if !plan.ReplaySafe {
			state.Status = "ambiguous"
			state.Phase = "outcome-unknown"
			state.LastError = "worker restarted after dispatching a non-replay-safe operation; operator reconciliation is required"
		} else {
			state.Status = "queued"
			state.Phase = "recovered"
			state.LastError = ""
		}
		state.UpdatedAt = s.now().UTC()
		if err := s.writeLocked(state); err != nil {
			return err
		}
		if err := s.appendRecord(state.Spec.ID, state.Spec.Kind, state.Phase, state.Status, state.LastError); err != nil {
			return err
		}
	}
	return nil
}

func (s *OperationStore) claimNext(inFlight map[string]struct{}) (*OperationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	for _, state := range states {
		if state.Status != "queued" {
			continue
		}
		if _, exists := inFlight[state.Spec.ID]; exists {
			continue
		}
		state.Status = "running"
		state.Phase = "executing"
		state.Attempts++
		state.UpdatedAt = s.now().UTC()
		if err := s.writeLocked(state); err != nil {
			return nil, err
		}
		if err := s.appendRecord(state.Spec.ID, state.Spec.Kind, "executing", "running", "operation execution started"); err != nil {
			return nil, err
		}
		return &state, nil
	}
	return nil, nil
}

func (s *OperationStore) complete(id string, executionErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readLocked(id)
	if err != nil {
		return err
	}
	status := "succeeded"
	message := "operation completed"
	state.Phase = "completed"
	if executionErr != nil {
		status = "failed"
		message = executionErr.Error()
		state.Phase = "failed"
		state.LastError = executionErr.Error()
	}
	state.Status = status
	state.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(*state); err != nil {
		return err
	}
	return s.appendRecord(id, state.Spec.Kind, state.Phase, status, message)
}

func (s *OperationStore) requeue(id string, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readLocked(id)
	if err != nil {
		return err
	}
	state.Status = "queued"
	state.Phase = "interrupted"
	state.LastError = message
	state.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(*state); err != nil {
		return err
	}
	return s.appendRecord(id, state.Spec.Kind, "interrupted", "queued", message)
}

func (s *OperationStore) markAmbiguous(id string, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readLocked(id)
	if err != nil {
		return err
	}
	state.Status = "ambiguous"
	state.Phase = "outcome-unknown"
	state.LastError = message
	state.UpdatedAt = s.now().UTC()
	if err := s.writeLocked(*state); err != nil {
		return err
	}
	return s.appendRecord(id, state.Spec.Kind, state.Phase, state.Status, message)
}

func (s *OperationStore) Read(id string) (*OperationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

func (s *OperationStore) listLocked() ([]OperationState, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	states := make([]OperationState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		state, err := s.readLocked(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		states = append(states, *state)
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].Spec.CreatedAt.Equal(states[j].Spec.CreatedAt) {
			return states[i].Spec.ID < states[j].Spec.ID
		}
		return states[i].Spec.CreatedAt.Before(states[j].Spec.CreatedAt)
	})
	return states, nil
}

func (s *OperationStore) readLocked(id string) (*OperationState, error) {
	if !operationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid operation ID")
	}
	data, err := os.ReadFile(s.operationPath(id))
	if err != nil {
		return nil, err
	}
	var state OperationState
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("operation state must contain one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := validateOperationSpec(&state.Spec, s.now); err != nil {
		return nil, err
	}
	if state.Spec.ID != id {
		return nil, fmt.Errorf("operation state ID does not match its durable filename")
	}
	if state.Status != "queued" && state.Status != "running" && state.Status != "succeeded" && state.Status != "failed" && state.Status != "ambiguous" {
		return nil, fmt.Errorf("operation state status is invalid")
	}
	return &state, nil
}

func (s *OperationStore) writeLocked(state OperationState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(s.operationPath(state.Spec.ID), append(data, '\n'), 0640)
}

func (s *OperationStore) operationPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *OperationStore) appendRecord(id string, operation string, phase string, status string, message string) error {
	return s.journal.Append(OperationRecord{
		OperationID: id, Operation: operation, Phase: phase, Status: status,
		NodeID: s.nodeID, Message: boundedJournalMessage(message), Timestamp: s.now().UTC(),
	})
}

func boundedJournalMessage(message string) string {
	if len(message) <= maxJournalMessageBytes {
		return message
	}
	hash := sha256.Sum256([]byte(message))
	prefix := strings.ToValidUTF8(message[:maxJournalMessageBytes], "�")
	return fmt.Sprintf("%s...[truncated original-bytes=%d sha256=%x]", prefix, len(message), hash)
}

func validateOperationSpec(spec *OperationSpec, now func() time.Time) error {
	if spec == nil || !operationIDPattern.MatchString(spec.ID) {
		return fmt.Errorf("operation ID is invalid")
	}
	if spec.APIVersion == "" {
		spec.APIVersion = APIVersion
	}
	if spec.APIVersion != APIVersion || strings.TrimSpace(spec.Kind) == "" {
		return fmt.Errorf("operation apiVersion or kind is invalid")
	}
	if len(spec.Payload) > 16<<20 {
		return fmt.Errorf("operation payload exceeds 16 MiB")
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = now().UTC()
	}
	return nil
}

type OperationEngine struct {
	store     *OperationStore
	executor  OperationExecutor
	admission *AdmissionController
	maxOps    int
	poll      time.Duration
}

func NewOperationEngine(store *OperationStore, executor OperationExecutor, admission *AdmissionController, maxOps int) (*OperationEngine, error) {
	if store == nil || executor == nil || admission == nil || maxOps < 1 {
		return nil, fmt.Errorf("operation store, executor, admission, and positive concurrency are required")
	}
	if err := store.recoverIncomplete(executor); err != nil {
		return nil, err
	}
	return &OperationEngine{store: store, executor: executor, admission: admission, maxOps: maxOps, poll: 100 * time.Millisecond}, nil
}

func (e *OperationEngine) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(e.poll)
	defer ticker.Stop()
	inFlight := make(map[string]struct{})
	var mu sync.Mutex
	var workers sync.WaitGroup
	workerErrors := make(chan error, e.maxOps)
	defer func() {
		cancel()
		workers.Wait()
	}()
	dispatch := func() error {
		for {
			mu.Lock()
			if len(inFlight) >= e.maxOps {
				mu.Unlock()
				return nil
			}
			state, err := e.store.claimNext(inFlight)
			if err != nil {
				mu.Unlock()
				return err
			}
			if state == nil {
				mu.Unlock()
				return nil
			}
			inFlight[state.Spec.ID] = struct{}{}
			mu.Unlock()
			workers.Add(1)
			go func(state OperationState) {
				defer workers.Done()
				log.Printf("platform audit: operation=%s kind=%s phase=executing status=running attempt=%d", state.Spec.ID, state.Spec.Kind, state.Attempts)
				plan, planErr := e.executor.Plan(state.Spec)
				var release func()
				var admitErr error
				if planErr == nil {
					release, admitErr = e.admission.Admit(runCtx, plan.OperationEffect)
				}
				var executeErr error
				dispatched := false
				if planErr != nil {
					executeErr = planErr
				} else if admitErr != nil {
					executeErr = admitErr
				} else {
					dispatched = true
					executeErr = e.executor.Execute(runCtx, state.Spec)
					release()
				}
				var persistErr error
				if !dispatched && errors.Is(executeErr, context.Canceled) && runCtx.Err() != nil {
					persistErr = e.store.requeue(state.Spec.ID, "operation interrupted before dispatch by worker shutdown")
				} else if !dispatched {
					persistErr = e.store.complete(state.Spec.ID, executeErr)
				} else if errors.Is(executeErr, context.Canceled) && runCtx.Err() != nil {
					if plan.ReplaySafe {
						persistErr = e.store.requeue(state.Spec.ID, "operation interrupted by worker shutdown")
					} else {
						persistErr = e.store.markAmbiguous(state.Spec.ID, "worker stopped while a non-replay-safe operation was in flight; operator reconciliation is required")
					}
				} else if executeErr != nil && !plan.ReplaySafe {
					persistErr = e.store.markAmbiguous(state.Spec.ID, "non-replay-safe operation returned an uncertain post-dispatch result; operator reconciliation is required: "+executeErr.Error())
				} else {
					persistErr = e.store.complete(state.Spec.ID, executeErr)
				}
				phase, status := "completed", "succeeded"
				if !dispatched && errors.Is(executeErr, context.Canceled) && runCtx.Err() != nil {
					phase, status = "interrupted", "queued"
				} else if !dispatched && executeErr != nil {
					phase, status = "failed", "failed"
				} else if errors.Is(executeErr, context.Canceled) && runCtx.Err() != nil {
					if plan.ReplaySafe {
						phase, status = "interrupted", "queued"
					} else {
						phase, status = "outcome-unknown", "ambiguous"
					}
				} else if executeErr != nil && !plan.ReplaySafe {
					phase, status = "outcome-unknown", "ambiguous"
				} else if executeErr != nil {
					phase, status = "failed", "failed"
				}
				log.Printf("platform audit: operation=%s kind=%s phase=%s status=%s", state.Spec.ID, state.Spec.Kind, phase, status)
				mu.Lock()
				delete(inFlight, state.Spec.ID)
				mu.Unlock()
				if persistErr != nil {
					workerErrors <- persistErr
				}
			}(*state)
		}
	}
	if err := dispatch(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-workerErrors:
			return err
		case <-ticker.C:
			if err := dispatch(); err != nil {
				return err
			}
		}
	}
}

type AgentRequestPayload struct {
	Method   string          `json:"method"`
	Endpoint string          `json:"endpoint"`
	Body     json.RawMessage `json:"body,omitempty"`
}

type StructuredAgent interface {
	RequestJSON(context.Context, string, string, any) (string, error)
}

type AgentOperationExecutor struct{ Agent StructuredAgent }

func (e AgentOperationExecutor) Plan(spec OperationSpec) (OperationPlan, error) {
	payload, parsed, err := parseAgentOperation(spec)
	if err != nil {
		return OperationPlan{}, err
	}
	plan := OperationPlan{OperationEffect: OperationEffect{DiskGrowth: true}}
	if payload.Method == http.MethodDelete {
		plan.DiskGrowth = false
		plan.ReplaySafe = true
	}
	switch parsed.Path {
	case "/v1/remove-service", "/v1/cleanup", "/v1/backups/cleanup":
		plan.DiskGrowth = false
		plan.ReplaySafe = true
	case "/v1/reconcile-service":
		var desired struct {
			Containers []json.RawMessage `json:"containers"`
			Files      []json.RawMessage `json:"files"`
		}
		if err := json.Unmarshal(payload.Body, &desired); err != nil {
			return OperationPlan{}, fmt.Errorf("decode reconcile operation: %w", err)
		}
		plan.DiskGrowth = len(desired.Containers) > 0 || len(desired.Files) > 0
		plan.ReplaySafe = true
	case "/v1/service-files", "/v1/proxy-file", "/v1/state", "/v1/env-bundle", "/v1/backup-schedule", "/v1/metadata":
		plan.ReplaySafe = payload.Method == http.MethodPut
	case "/v1/proxy", "/v1/mesh/apply", "/v1/jobs/apply":
		plan.ReplaySafe = payload.Method == http.MethodPost
	case "/v1/images/build", "/v1/images/import":
		plan.Build = true
	}
	return plan, nil
}

func (e AgentOperationExecutor) Execute(ctx context.Context, spec OperationSpec) error {
	payload, _, err := parseAgentOperation(spec)
	if err != nil {
		return err
	}
	if e.Agent == nil {
		return fmt.Errorf("agent operation executor is not configured")
	}
	var body any
	if len(payload.Body) > 0 {
		body = payload.Body
	}
	_, err = e.Agent.RequestJSON(ctx, payload.Method, payload.Endpoint, body)
	return err
}

func parseAgentOperation(spec OperationSpec) (AgentRequestPayload, *url.URL, error) {
	if spec.Kind != "agent.request" {
		return AgentRequestPayload{}, nil, fmt.Errorf("unsupported platform operation kind %q", spec.Kind)
	}
	var payload AgentRequestPayload
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		return AgentRequestPayload{}, nil, fmt.Errorf("decode agent operation: %w", err)
	}
	payload.Method = strings.ToUpper(strings.TrimSpace(payload.Method))
	if payload.Method != http.MethodPost && payload.Method != http.MethodPut && payload.Method != http.MethodDelete {
		return AgentRequestPayload{}, nil, fmt.Errorf("agent operation method is not a structured mutation")
	}
	parsed, err := url.ParseRequestURI(payload.Endpoint)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/v1/") {
		return AgentRequestPayload{}, nil, fmt.Errorf("agent operation endpoint is invalid")
	}
	return payload, parsed, nil
}
