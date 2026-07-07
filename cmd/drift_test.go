package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/drift"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/spf13/cobra"
)

func writeDriftTestConfig(t *testing.T, root string) {
	t.Helper()
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	configData := []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 203.0.113.10
    user: deploy
    sshKey: ` + sshKey + `
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
}

func TestCheckDriftOnceMachineOutputReportsDrift(t *testing.T) {
	root := switchToTempDir(t)
	writeDriftTestConfig(t, root)
	cfg, err := config.LoadConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	detector := drift.NewDetectorWithActualProvider(cfg, "production", nil, false, func() (map[string]drift.ActualService, error) {
		return map[string]drift.ActualService{}, nil
	})

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = checkDriftOnce(detector)
	})
	if runErr == nil {
		t.Fatal("checkDriftOnce should surface detected drift")
	}
	if engine.Classify(runErr) != engine.ClassAttention {
		t.Fatalf("drift classified as %d, want ClassAttention", engine.Classify(runErr))
	}

	var result engine.DriftResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if !result.Drifted || result.Kind != engine.KindDriftResult {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if len(result.Drifts) == 0 || result.Drifts[0].Service != "web" {
		t.Fatalf("drifts = %+v", result.Drifts)
	}
}

func TestRunDriftRejectsWatchInMachineMode(t *testing.T) {
	root := switchToTempDir(t)
	writeDriftTestConfig(t, root)

	restoreOutput, restoreWatch := outputFormatFlag, driftWatch
	outputFormatFlag, driftWatch = outputFormatJSON, true
	t.Cleanup(func() { outputFormatFlag, driftWatch = restoreOutput, restoreWatch })
	oldCfgFile, oldEnvFlag := cfgFile, envFlag
	cfgFile, envFlag = "", ""
	t.Cleanup(func() { cfgFile, envFlag = oldCfgFile, oldEnvFlag })

	err := runDrift(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runDrift should reject --watch in machine modes")
	}
	if engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("watch rejection classified as %d, want ClassInvalid", engine.Classify(err))
	}
}

func TestDriftServicesFromReconcileUsesLogicalServiceNames(t *testing.T) {
	got := driftServicesFromReconcile(map[string]*reconcile.ActualService{
		"web": {
			Name:     "web",
			Image:    "demo:web",
			Replicas: 3,
		},
	})

	service, ok := got["web"]
	if !ok {
		t.Fatalf("converted services = %#v, want web", got)
	}
	if service.Name != "web" {
		t.Fatalf("service name = %q, want logical detector name", service.Name)
	}
	if service.Replicas != 3 || service.Running != 3 {
		t.Fatalf("replicas/running = %d/%d, want 3/3", service.Replicas, service.Running)
	}
}
