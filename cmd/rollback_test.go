package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
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

func TestPreviousStableServiceDeploymentFromHistorySkipsFailedCurrent(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("failed-newest", remotestate.StatusFailed, base.Add(2*time.Hour), "web"),
			rollbackHistoryDeployment("success-newer", remotestate.StatusSuccess, base.Add(time.Hour), "web"),
			rollbackHistoryDeployment("success-older", remotestate.StatusSuccess, base, "web"),
		},
	}

	deployment, err := previousStableServiceDeploymentFromHistory(history, "web")
	if err != nil {
		t.Fatalf("previousStableServiceDeploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "success-newer" {
		t.Fatalf("deployment ID = %q, want success-newer", deployment.ID)
	}
}

func TestPreviousStableServiceDeploymentFromHistorySelectsPreviousWhenCurrentIsStable(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("current", remotestate.StatusSuccess, base.Add(2*time.Hour), "web"),
			rollbackHistoryDeployment("previous-api", remotestate.StatusSuccess, base.Add(time.Hour), "api"),
			rollbackHistoryDeployment("previous-web", remotestate.StatusSuccess, base, "web"),
		},
	}

	deployment, err := previousStableServiceDeploymentFromHistory(history, "web")
	if err != nil {
		t.Fatalf("previousStableServiceDeploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "previous-web" {
		t.Fatalf("deployment ID = %q, want previous-web", deployment.ID)
	}
}

func TestPreviousStableServiceDeploymentFromHistoryRequiresPreviousStable(t *testing.T) {
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("only-current", remotestate.StatusSuccess, time.Now().UTC(), "web"),
		},
	}

	if _, err := previousStableServiceDeploymentFromHistory(history, "web"); err == nil {
		t.Fatal("previousStableServiceDeploymentFromHistory should reject history without a previous stable deployment")
	}
}

func TestSelectRollbackTargetRejectsFailedSpecificDeployment(t *testing.T) {
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("failed", remotestate.StatusFailed, time.Now().UTC(), "web"),
		},
	}

	_, err := selectRollbackTargetFromHistory(history, "failed", "web")
	if err == nil {
		t.Fatal("selectRollbackTargetFromHistory should reject failed specific targets")
	}
	if !strings.Contains(err.Error(), "not a stable rollback target") {
		t.Fatalf("error = %q, want stable target context", err)
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

func TestBuildRollbackDeploymentDoesNotMutateTarget(t *testing.T) {
	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	target := rollbackHistoryDeployment("target", remotestate.StatusSuccess, start.Add(-time.Hour), "web")
	target.Version = "1.2.3"
	target.GitCommit = "abcdef"
	serviceState := target.Services["web"]

	rollback := buildRollbackDeployment(testRollbackConfig(), "production", "node-a", start, 3*time.Second, target, "web", serviceState)

	if target.Status != remotestate.StatusSuccess {
		t.Fatalf("target status = %q, want preserved success", target.Status)
	}
	if rollback.Status != remotestate.StatusRolledBack {
		t.Fatalf("rollback status = %q, want rolled_back", rollback.Status)
	}
	if rollback.Services["web"].Image != "web:target" {
		t.Fatalf("rollback image = %q, want web:target", rollback.Services["web"].Image)
	}
	if rollback.GitCommit != "abcdef" {
		t.Fatalf("rollback git commit = %q, want abcdef", rollback.GitCommit)
	}
}

func rollbackHistoryDeployment(id string, status remotestate.DeploymentStatus, timestamp time.Time, services ...string) *remotestate.DeploymentState {
	serviceStates := make(map[string]remotestate.ServiceState, len(services))
	for _, serviceName := range services {
		serviceStates[serviceName] = remotestate.ServiceState{
			Name:     serviceName,
			Image:    serviceName + ":" + id,
			Port:     3000,
			Replicas: 1,
		}
	}
	return &remotestate.DeploymentState{
		ID:        id,
		Status:    status,
		Timestamp: timestamp,
		Services:  serviceStates,
	}
}

func testRollbackConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Name:    "demo",
			Version: "1.0.0",
		},
		Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod},
	}
}
