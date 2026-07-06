package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestHistoryBuildsResultAndFilterOptions(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	timestamp := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{{
		ID:             "deployment-1234567890",
		Timestamp:      timestamp,
		Version:        "v42",
		Status:         remotestate.StatusFailed,
		Duration:       1500 * time.Millisecond,
		GitCommitShort: "abc1234",
		GitCommitMsg:   "fix issue",
		Error:          "boom",
	}}}

	var gotHistory *remotestate.DeploymentHistory
	var gotOpts *remotestate.HistoryOptions
	result, err := New(Options{}).History(context.Background(), HistoryRequest{
		Config:        cfg,
		Environment:   "production",
		Server:        "node-a",
		Limit:         5,
		Status:        "failed",
		IncludeFailed: false,
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "node-b", history, nil
		},
		ListDeployments: func(h *remotestate.DeploymentHistory, opts *remotestate.HistoryOptions) []*remotestate.DeploymentState {
			gotHistory = h
			gotOpts = opts
			return h.Deployments
		},
	})
	if err != nil {
		t.Fatalf("History returned error: %v", err)
	}
	if gotHistory != history {
		t.Fatalf("lister history = %#v, want source history", gotHistory)
	}
	if gotOpts == nil || gotOpts.Limit != 5 || gotOpts.Status != remotestate.StatusFailed || gotOpts.IncludeFailed {
		t.Fatalf("history options = %#v, want limit/status/includeFailed", gotOpts)
	}
	if result.APIVersion != takoapi.APIVersionCurrent || result.Kind != KindHistoryResult || result.Project != "demo" || result.Environment != "production" {
		t.Fatalf("result identity = %#v", result)
	}
	if result.SourceServer != "node-b" || result.Server != "node-a" || result.Status != "failed" || result.Limit != 5 || result.IncludeFailed {
		t.Fatalf("result request/source fields = %#v", result)
	}
	if len(result.Deployments) != 1 {
		t.Fatalf("deployments = %#v, want one", result.Deployments)
	}
	dep := result.Deployments[0]
	if dep.ID != "deployment-1234567890" || dep.DisplayID != "deployment" || dep.Commit != "abc1234" || !dep.Timestamp.Equal(timestamp) {
		t.Fatalf("deployment identity fields = %#v", dep)
	}
	if dep.Version != "v42" || dep.Status != remotestate.StatusFailed || dep.DurationSeconds != 1.5 || dep.Duration != "1.5s" || dep.Message != "fix issue" || dep.Error != "boom" {
		t.Fatalf("deployment detail fields = %#v", dep)
	}
}

func TestHistoryReturnsEmptyResultWhenNoSourceHistory(t *testing.T) {
	result, err := New(Options{}).History(context.Background(), HistoryRequest{
		Config:        &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment:   "production",
		Limit:         10,
		IncludeFailed: true,
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "", nil, nil
		},
		ListDeployments: func(*remotestate.DeploymentHistory, *remotestate.HistoryOptions) []*remotestate.DeploymentState {
			t.Fatal("lister should not be called without a source history")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("History returned error: %v", err)
	}
	if result == nil || len(result.Deployments) != 0 {
		t.Fatalf("result = %#v, want empty deployments", result)
	}
}

func TestHistoryPropagatesHistorySourceError(t *testing.T) {
	wantErr := errors.New("mesh unavailable")
	_, err := New(Options{}).History(context.Background(), HistoryRequest{
		Config:        &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment:   "production",
		IncludeFailed: true,
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "", nil, wantErr
		},
		ListDeployments: func(*remotestate.DeploymentHistory, *remotestate.HistoryOptions) []*remotestate.DeploymentState {
			return nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("History error = %v, want %v", err, wantErr)
	}
}
