package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
)

func TestDeploymentFromHistoryFindsRequestedDeployment(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			{ID: "old", Timestamp: base},
			{ID: "target", Timestamp: base.Add(time.Hour)},
		},
	}

	deployment, err := deploymentFromHistory(history, "target")
	if err != nil {
		t.Fatalf("deploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "target" {
		t.Fatalf("deployment ID = %q, want target", deployment.ID)
	}
}

func TestDeploymentFromHistoryRejectsMissingDeployment(t *testing.T) {
	if _, err := deploymentFromHistory(&remotestate.DeploymentHistory{}, "missing"); err == nil {
		t.Fatal("deploymentFromHistory should reject missing deployment")
	}
}

func TestLatestSuccessfulDeploymentFromHistorySkipsFailedDeployments(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			{ID: "failed-newest", Status: remotestate.StatusFailed, Timestamp: base.Add(2 * time.Hour)},
			{ID: "success-newer", Status: remotestate.StatusSuccess, Timestamp: base.Add(time.Hour)},
			{ID: "success-older", Status: remotestate.StatusSuccess, Timestamp: base},
		},
	}

	deployment, err := latestSuccessfulDeploymentFromHistory(history)
	if err != nil {
		t.Fatalf("latestSuccessfulDeploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "success-newer" {
		t.Fatalf("deployment ID = %q, want success-newer", deployment.ID)
	}
}

func TestLatestSuccessfulDeploymentFromHistoryRequiresSuccess(t *testing.T) {
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			{ID: "failed", Status: remotestate.StatusFailed, Timestamp: time.Now().UTC()},
		},
	}

	if _, err := latestSuccessfulDeploymentFromHistory(history); err == nil {
		t.Fatal("latestSuccessfulDeploymentFromHistory should reject history without successful deployments")
	}
}

func TestRollbackRemoteHistoryErrorFailsSuccessfulRuntimeMutation(t *testing.T) {
	err := rollbackRemoteHistoryError(errors.New("disk full"))
	if err == nil {
		t.Fatal("rollbackRemoteHistoryError returned nil")
	}
	for _, want := range []string{"rollback succeeded", "failed to update remote deployment history", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}
