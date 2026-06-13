package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	localstate "github.com/redentordev/tako-cli/pkg/state"
)

func TestSyncRemoteDeploymentsToLocalKeepsNewestAsCurrent(t *testing.T) {
	tempDir := t.TempDir()
	localMgr, err := localstate.NewManager(tempDir, "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	oldDeployment := remoteDeployment("old", base, "demo:v1")
	newDeployment := remoteDeployment("new", base.Add(time.Hour), "demo:v2")

	synced, err := syncRemoteDeploymentsToLocal(localMgr, []*remotestate.DeploymentState{
		newDeployment,
		oldDeployment,
	}, "production")
	if err != nil {
		t.Fatalf("syncRemoteDeploymentsToLocal returned error: %v", err)
	}
	if synced != 2 {
		t.Fatalf("synced = %d, want 2", synced)
	}

	current, err := localMgr.GetCurrentDeployment()
	if err != nil {
		t.Fatalf("GetCurrentDeployment returned error: %v", err)
	}
	if current == nil {
		t.Fatal("current deployment is nil")
	}
	if current.DeploymentID != "new" {
		t.Fatalf("current deployment = %q, want newest deployment", current.DeploymentID)
	}
	if got := current.Services["web"].Image; got != "demo:v2" {
		t.Fatalf("current web image = %q, want demo:v2", got)
	}
}

func TestLocalDeploymentStateExistsIgnoresSecretsOnlyTakoDirectory(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.MkdirAll(filepath.Join(".tako"), 0755); err != nil {
		t.Fatalf("failed to create .tako: %v", err)
	}
	if err := os.WriteFile(filepath.Join(".tako", "secrets.production"), []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatalf("failed to write secrets fixture: %v", err)
	}

	if localDeploymentStateExists("production") {
		t.Fatal("secrets-only .tako directory should not count as local deployment state")
	}

	localMgr, err := localstate.NewManager(".", "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if err := localMgr.SaveDeployment(&localstate.DeploymentState{
		DeploymentID: "current",
		Timestamp:    time.Now().UTC(),
		Environment:  "production",
		Status:       "success",
		Services:     map[string]*localstate.ServiceDeploy{},
	}); err != nil {
		t.Fatalf("SaveDeployment returned error: %v", err)
	}

	if !localDeploymentStateExists("production") {
		t.Fatal("current deployment should count as local deployment state")
	}
}

func remoteDeployment(id string, timestamp time.Time, image string) *remotestate.DeploymentState {
	return &remotestate.DeploymentState{
		ID:          id,
		Timestamp:   timestamp,
		ProjectName: "demo",
		Environment: "production",
		Status:      remotestate.StatusSuccess,
		Services: map[string]remotestate.ServiceState{
			"web": {
				Name:     "web",
				Image:    image,
				Port:     3000,
				Replicas: 1,
				HealthCheck: remotestate.HealthCheckState{
					Enabled: true,
					Healthy: true,
				},
			},
		},
		User: "tester",
	}
}
