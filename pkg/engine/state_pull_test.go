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

func TestStatePullSyncsSelectedHistory(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	oldDep := &remotestate.DeploymentState{ID: "deploy-old", Timestamp: base, Status: remotestate.StatusSuccess, User: "alice"}
	newDep := &remotestate.DeploymentState{ID: "deployment-1234567890", Timestamp: base.Add(time.Hour), Status: remotestate.StatusWarmed, User: "bob", GitCommitShort: "abc1234"}
	var synced []*remotestate.DeploymentState

	result, err := New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment: "production",
		Server:      "node-a",
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "node-b", &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{oldDep, newDep}}, nil
		},
		SyncDeployments: func(deployments []*remotestate.DeploymentState) (int, error) {
			synced = deployments
			return len(deployments), nil
		},
		RecoverFromMeshActual: func() (StatePullRecoveryResult, error) {
			t.Fatal("mesh recovery should not be called when history exists")
			return StatePullRecoveryResult{}, nil
		},
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) {
			t.Fatal("running recovery should not be called when history exists")
			return StatePullRecoveryResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("StatePull returned error: %v", err)
	}
	if result.APIVersion != takoapi.APIVersionCurrent || result.Kind != KindStatePullResult || result.Project != "demo" || result.Environment != "production" {
		t.Fatalf("result identity = %#v", result)
	}
	if result.Status != StatePullStatusSyncedHistory || result.SourceServer != "node-b" || result.Server != "node-a" || result.SyncedCount != 2 {
		t.Fatalf("result sync fields = %#v", result)
	}
	if len(synced) != 2 || synced[1] != newDep {
		t.Fatalf("synced deployments = %#v", synced)
	}
	if result.Latest == nil || result.Latest.ID != "deployment-1234567890" || result.Latest.DisplayID != "deployment" || result.Latest.Status != remotestate.StatusWarmed || result.Latest.User != "bob" || result.Latest.Commit != "abc1234" || !result.Latest.Timestamp.Equal(newDep.Timestamp) {
		t.Fatalf("latest = %#v", result.Latest)
	}
}

func TestStatePullFallsBackToMeshActualRecovery(t *testing.T) {
	result, err := New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment: "production",
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "", nil, nil
		},
		SyncDeployments: func([]*remotestate.DeploymentState) (int, error) {
			t.Fatal("sync should not be called without history")
			return 0, nil
		},
		RecoverFromMeshActual: func() (StatePullRecoveryResult, error) {
			return StatePullRecoveryResult{ServiceCount: 3}, nil
		},
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) {
			t.Fatal("running recovery should not be called after mesh recovery succeeds")
			return StatePullRecoveryResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("StatePull returned error: %v", err)
	}
	if result.Status != StatePullStatusRecoveredActual || result.Recovered == nil || result.Recovered.ServiceCount != 3 {
		t.Fatalf("result = %#v, want mesh recovery", result)
	}
}

func TestStatePullFallsBackToRunningMeshRecovery(t *testing.T) {
	meshErr := errors.New("no mesh actual state found")
	result, err := New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment: "production",
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "", &remotestate.DeploymentHistory{}, nil
		},
		SyncDeployments: func([]*remotestate.DeploymentState) (int, error) { return 0, nil },
		RecoverFromMeshActual: func() (StatePullRecoveryResult, error) {
			return StatePullRecoveryResult{}, meshErr
		},
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) {
			return StatePullRecoveryResult{ServiceCount: 2}, nil
		},
	})
	if err != nil {
		t.Fatalf("StatePull returned error: %v", err)
	}
	if result.Status != StatePullStatusRecoveredRunning || result.Recovered == nil || result.Recovered.ServiceCount != 2 {
		t.Fatalf("result = %#v, want running recovery", result)
	}
	if result.MeshActualError != meshErr.Error() || len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v meshErr = %q", result.Warnings, result.MeshActualError)
	}
}

func TestStatePullNoneFoundIsNonFatal(t *testing.T) {
	meshErr := errors.New("no mesh actual state found")
	runningErr := errors.New("no running takod containers found")
	result, err := New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment: "production",
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "", nil, nil
		},
		SyncDeployments: func([]*remotestate.DeploymentState) (int, error) { return 0, nil },
		RecoverFromMeshActual: func() (StatePullRecoveryResult, error) {
			return StatePullRecoveryResult{}, meshErr
		},
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) {
			return StatePullRecoveryResult{}, runningErr
		},
	})
	if err != nil {
		t.Fatalf("StatePull returned fatal error: %v", err)
	}
	if result.Status != StatePullStatusNoneFound || result.MeshActualError != meshErr.Error() || result.RunningMeshError != runningErr.Error() || len(result.Warnings) != 2 {
		t.Fatalf("result = %#v, want none_found with warning details", result)
	}
}

func TestStatePullPropagatesHistoryAndSyncErrors(t *testing.T) {
	wantHistoryErr := errors.New("history unavailable")
	_, err := New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:                 &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment:            "production",
		HistorySource:          func() (string, *remotestate.DeploymentHistory, error) { return "", nil, wantHistoryErr },
		SyncDeployments:        func([]*remotestate.DeploymentState) (int, error) { return 0, nil },
		RecoverFromMeshActual:  func() (StatePullRecoveryResult, error) { return StatePullRecoveryResult{}, nil },
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) { return StatePullRecoveryResult{}, nil },
	})
	if !errors.Is(err, wantHistoryErr) {
		t.Fatalf("history error = %v, want %v", err, wantHistoryErr)
	}

	wantSyncErr := errors.New("local write failed")
	_, err = New(Options{}).StatePull(context.Background(), StatePullRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		Environment: "production",
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			return "node-a", &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{{ID: "deploy-1"}}}, nil
		},
		SyncDeployments:        func([]*remotestate.DeploymentState) (int, error) { return 0, wantSyncErr },
		RecoverFromMeshActual:  func() (StatePullRecoveryResult, error) { return StatePullRecoveryResult{}, nil },
		RecoverFromRunningMesh: func() (StatePullRecoveryResult, error) { return StatePullRecoveryResult{}, nil },
	})
	if !errors.Is(err, wantSyncErr) {
		t.Fatalf("sync error = %v, want %v", err, wantSyncErr)
	}
}
