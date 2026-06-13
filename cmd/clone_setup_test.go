package cmd

import (
	"fmt"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestCloneSetupStateRecoveryHookCanUseMeshActualFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	called := false
	originalCollect := cloneSetupCollectStatePullHistories
	originalSync := cloneSetupSyncBestDeploymentHistoryToLocal
	originalRecover := cloneSetupRecoverStateFromMeshActual
	cloneSetupCollectStatePullHistories = func(*config.Config, string, string) ([]stateHistoryCandidate, error) {
		return nil, nil
	}
	cloneSetupSyncBestDeploymentHistoryToLocal = func(*config.Config, string, []stateHistoryCandidate) (string, int, bool, error) {
		return "", 0, false, nil
	}
	cloneSetupRecoverStateFromMeshActual = func(cfg *config.Config, envName string, requestedServer string) error {
		called = true
		if cfg.Project.Name != "demo" {
			t.Fatalf("project = %q, want demo", cfg.Project.Name)
		}
		if envName != "production" {
			t.Fatalf("envName = %q, want production", envName)
		}
		if requestedServer != "" {
			t.Fatalf("requestedServer = %q, want empty", requestedServer)
		}
		return nil
	}
	t.Cleanup(func() {
		cloneSetupCollectStatePullHistories = originalCollect
		cloneSetupSyncBestDeploymentHistoryToLocal = originalSync
		cloneSetupRecoverStateFromMeshActual = originalRecover
	})

	message, err := cloneSetupSyncMissingState(&config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}, "production")
	if err != nil {
		t.Fatalf("cloneSetupSyncMissingState returned error: %v", err)
	}
	if message != "State recovered from remote mesh runtime" {
		t.Fatalf("message = %q, want mesh recovery message", message)
	}
	if !called {
		t.Fatal("clone setup mesh actual recovery hook was not called")
	}
}

func TestCloneSetupStateRecoveryHookPropagatesError(t *testing.T) {
	t.Chdir(t.TempDir())
	originalCollect := cloneSetupCollectStatePullHistories
	originalSync := cloneSetupSyncBestDeploymentHistoryToLocal
	originalRecover := cloneSetupRecoverStateFromMeshActual
	cloneSetupCollectStatePullHistories = func(*config.Config, string, string) ([]stateHistoryCandidate, error) {
		return nil, nil
	}
	cloneSetupSyncBestDeploymentHistoryToLocal = func(*config.Config, string, []stateHistoryCandidate) (string, int, bool, error) {
		return "", 0, false, nil
	}
	cloneSetupRecoverStateFromMeshActual = func(*config.Config, string, string) error {
		return fmt.Errorf("no mesh actual state found")
	}
	t.Cleanup(func() {
		cloneSetupCollectStatePullHistories = originalCollect
		cloneSetupSyncBestDeploymentHistoryToLocal = originalSync
		cloneSetupRecoverStateFromMeshActual = originalRecover
	})

	_, err := cloneSetupSyncMissingState(&config.Config{}, "production")
	if err == nil {
		t.Fatal("expected clone setup state sync error")
	}
	if got := err.Error(); got != "No remote state available (deploy first): no mesh actual state found" {
		t.Fatalf("error = %q, want mesh actual recovery error", got)
	}
}
