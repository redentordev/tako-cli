package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRecordStartedDeploymentStateSavesInProgressAndAssignedID(t *testing.T) {
	saver := newHistoryDeploymentSaver()
	deployment := &remotestate.DeploymentState{Status: remotestate.StatusFailed}

	if err := RecordStartedDeploymentStateContext(context.Background(), saver, deployment); err != nil {
		t.Fatalf("RecordStartedDeploymentStateContext returned error: %v", err)
	}
	if deployment.ID == "" {
		t.Fatal("deployment ID was not populated")
	}
	if len(saver.history) != 1 {
		t.Fatalf("history entries = %d, want 1", len(saver.history))
	}
	if saver.history[0].Status != remotestate.StatusInProgress {
		t.Fatalf("saved status = %q, want %q", saver.history[0].Status, remotestate.StatusInProgress)
	}
	if saver.history[0].ID != deployment.ID {
		t.Fatalf("saved ID = %q, want assigned ID %q", saver.history[0].ID, deployment.ID)
	}
}

func TestRecordStartedThenFailedDeploymentStateReplacesHistoryEntry(t *testing.T) {
	saver := newHistoryDeploymentSaver()
	deployment := &remotestate.DeploymentState{}
	start := time.Now()

	if err := RecordStartedDeploymentStateContext(context.Background(), saver, deployment); err != nil {
		t.Fatalf("RecordStartedDeploymentStateContext returned error: %v", err)
	}
	assignedID := deployment.ID
	if err := RecordFailedDeploymentStateContext(context.Background(), saver, nil, deployment, testFailedStateConfig(), "production", []string{"node-a"}, nil, start, errors.New("failed after start")); err != nil {
		t.Fatalf("RecordFailedDeploymentStateContext returned error: %v", err)
	}

	if len(saver.history) != 1 {
		t.Fatalf("history entries = %d, want 1", len(saver.history))
	}
	if saver.history[0].ID != assignedID {
		t.Fatalf("history ID = %q, want assigned ID %q", saver.history[0].ID, assignedID)
	}
	if saver.history[0].Status != remotestate.StatusFailed {
		t.Fatalf("history status = %q, want %q", saver.history[0].Status, remotestate.StatusFailed)
	}
	if !strings.Contains(saver.history[0].Error, "failed after start") {
		t.Fatalf("history error = %q, want failed after start", saver.history[0].Error)
	}
}

func TestRecordStartedDeploymentStateHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	saver := &contextAwareDeploymentSaver{rejectCanceled: true}
	deployment := &remotestate.DeploymentState{}

	err := RecordStartedDeploymentStateContext(ctx, saver, deployment)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if saver.saved != nil {
		t.Fatalf("deployment saved despite canceled context: %#v", saver.saved)
	}
	if saver.seenCtx != nil {
		t.Fatalf("saver was called with context %#v, want no save before mutation", saver.seenCtx)
	}
}

func TestRecordFailedDeploymentStateUsesCleanupContextWhenParentCanceled(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	saver := &contextAwareDeploymentSaver{rejectCanceled: true}
	deployment := &remotestate.DeploymentState{ID: "deploy-canceled"}

	err := RecordFailedDeploymentStateContext(parent, saver, nil, deployment, testFailedStateConfig(), "production", []string{"node-a"}, nil, time.Now(), context.Canceled)
	if err != nil {
		t.Fatalf("RecordFailedDeploymentStateContext returned error: %v", err)
	}
	if saver.saved == nil {
		t.Fatal("deployment was not saved")
	}
	if saver.seenCtx == parent {
		t.Fatal("saved with canceled parent context, want cleanup context")
	}
	if saver.errDuringSave != nil {
		t.Fatalf("cleanup context err during save = %v, want active context", saver.errDuringSave)
	}
	if saver.saved.Status != remotestate.StatusFailed {
		t.Fatalf("status = %q, want %q", saver.saved.Status, remotestate.StatusFailed)
	}
	if !strings.Contains(saver.saved.Error, "context canceled") {
		t.Fatalf("error = %q, want context canceled", saver.saved.Error)
	}
}

func TestRecordFailedDeploymentStatePassesActiveContextThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saver := &contextAwareDeploymentSaver{
		onSave: func(got context.Context, _ *remotestate.DeploymentState) error {
			if got != ctx {
				t.Fatalf("context = %#v, want original active context", got)
			}
			cancel()
			return got.Err()
		},
	}

	err := RecordFailedDeploymentStateContext(ctx, saver, nil, &remotestate.DeploymentState{ID: "deploy-active"}, testFailedStateConfig(), "production", []string{"node-a"}, nil, time.Now(), errors.New("boom"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if saver.saved != nil {
		t.Fatalf("deployment saved despite saver error: %#v", saver.saved)
	}
}

func TestRecordFailedDeploymentStateRepeatedIDReplacesHistoryEntry(t *testing.T) {
	saver := newHistoryDeploymentSaver()
	deployment := &remotestate.DeploymentState{ID: "deploy-repeat"}
	start := time.Now()

	if err := RecordFailedDeploymentStateContext(context.Background(), saver, nil, deployment, testFailedStateConfig(), "production", []string{"node-a"}, nil, start, errors.New("first failure")); err != nil {
		t.Fatalf("first RecordFailedDeploymentStateContext returned error: %v", err)
	}
	if err := RecordFailedDeploymentStateContext(context.Background(), saver, nil, deployment, testFailedStateConfig(), "production", []string{"node-a"}, nil, start, errors.New("second failure")); err != nil {
		t.Fatalf("second RecordFailedDeploymentStateContext returned error: %v", err)
	}

	if len(saver.history) != 1 {
		t.Fatalf("history entries = %d, want 1", len(saver.history))
	}
	if saver.history[0].ID != "deploy-repeat" {
		t.Fatalf("history ID = %q, want deploy-repeat", saver.history[0].ID)
	}
	if !strings.Contains(saver.history[0].Error, "second failure") {
		t.Fatalf("history error = %q, want second failure", saver.history[0].Error)
	}
}

func testFailedStateConfig() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Name: "demo"}}
}

type contextAwareDeploymentSaver struct {
	rejectCanceled bool
	onSave         func(context.Context, *remotestate.DeploymentState) error
	seenCtx        context.Context
	errDuringSave  error
	saved          *remotestate.DeploymentState
}

func (s *contextAwareDeploymentSaver) SaveDeployment(deployment *remotestate.DeploymentState) error {
	copy := *deployment
	s.saved = &copy
	return nil
}

func (s *contextAwareDeploymentSaver) SaveDeploymentContext(ctx context.Context, deployment *remotestate.DeploymentState) error {
	s.seenCtx = ctx
	s.errDuringSave = ctx.Err()
	if s.rejectCanceled {
		if err := s.errDuringSave; err != nil {
			return err
		}
	}
	if s.onSave != nil {
		if err := s.onSave(ctx, deployment); err != nil {
			return err
		}
	}
	copy := *deployment
	s.saved = &copy
	return nil
}

type historyDeploymentSaver struct {
	byID    map[string]*remotestate.DeploymentState
	history []*remotestate.DeploymentState
	nextID  int
}

func newHistoryDeploymentSaver() *historyDeploymentSaver {
	return &historyDeploymentSaver{byID: map[string]*remotestate.DeploymentState{}}
}

func (s *historyDeploymentSaver) SaveDeployment(deployment *remotestate.DeploymentState) error {
	return s.SaveDeploymentContext(context.Background(), deployment)
}

func (s *historyDeploymentSaver) SaveDeploymentContext(ctx context.Context, deployment *remotestate.DeploymentState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deployment.ID == "" {
		s.nextID++
		deployment.ID = fmt.Sprintf("fake-deploy-%d", s.nextID)
	}
	copy := *deployment
	s.byID[copy.ID] = &copy
	for i, existing := range s.history {
		if existing != nil && existing.ID == copy.ID {
			s.history[i] = &copy
			return nil
		}
	}
	s.history = append(s.history, &copy)
	return nil
}
