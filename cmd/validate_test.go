package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

func TestValidateCommandRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"validate"})
	if err != nil {
		t.Fatalf("Find(validate) returned error: %v", err)
	}
	if cmd != validateCmd {
		t.Fatalf("validate command was not registered")
	}
	if !validateCmd.SilenceUsage {
		t.Fatal("validate command should silence usage on validation errors")
	}
	if flag := validateCmd.Flags().Lookup("quiet"); flag == nil {
		t.Fatal("validate command missing --quiet flag")
	}
}

func TestRunValidateFailsInvalidYAMLBeforeGit(t *testing.T) {
	root := switchToTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte("project:\n  name: demo\n  version: [\n"), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	resetValidateGlobals(t)

	err := runValidate(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runValidate should fail on invalid YAML")
	}
	for _, want := range []string{"YAML syntax error in tako.yaml", "line 3", "3 |   version: [", "Check indentation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("validate should fail before git checks, got %q", err)
	}
}

func TestRunValidateFailsInvalidJSONWithLineColumnBeforeGit(t *testing.T) {
	root := switchToTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "tako.json"), []byte("{\n  \"project\": {\n    \"name\": \"demo\",\n  }\n}\n"), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	resetValidateGlobals(t)

	err := runValidate(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runValidate should fail on invalid JSON")
	}
	for _, want := range []string{"JSON syntax error in tako.json", "line 4, column 3", "4 |   }", "^", "Check indentation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("validate should fail before git checks, got %q", err)
	}
}

func TestRunValidateReportsTargetEnvironmentCounts(t *testing.T) {
	root := switchToTempDir(t)
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
		t.Fatalf("failed to write valid config: %v", err)
	}
	resetValidateGlobals(t)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runValidate(cmd, nil); err != nil {
		t.Fatalf("runValidate returned error: %v", err)
	}
	for _, want := range []string{
		"Config valid: tako.yaml",
		"Environment: production",
		"Runtime: takod",
		"State: replicated (consistency: lease)",
		"Mesh: enabled (10.210.0.0/16 via tako)",
		"Servers: 1",
		"Services: 1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
}

func TestRunValidateMachineOutputInvalidConfig(t *testing.T) {
	root := switchToTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte("project:\n  name: demo\n  version: [\n"), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	resetValidateGlobals(t)
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr == nil {
		t.Fatal("runValidate should fail on invalid YAML")
	}
	if engine.Classify(runErr) != engine.ClassInvalid {
		t.Fatalf("invalid config classified as %d, want ClassInvalid", engine.Classify(runErr))
	}

	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Kind != engine.KindValidateResult || result.Valid {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != engine.ValidateSeverityError {
		t.Fatalf("findings = %+v", result.Findings)
	}
	if !strings.Contains(result.Findings[0].Message, "YAML syntax error") {
		t.Fatalf("finding message = %q", result.Findings[0].Message)
	}
}

func TestRunValidateMachineOutputValidConfig(t *testing.T) {
	root := switchToTempDir(t)
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
		t.Fatalf("failed to write valid config: %v", err)
	}
	resetValidateGlobals(t)
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runValidate returned error: %v", runErr)
	}

	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if !result.Valid || result.Kind != engine.KindValidateResult {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if result.Project != "demo" || result.Environment != "production" {
		t.Fatalf("identity fields wrong: %+v", result)
	}
	if result.Servers != 1 || result.Services != 1 || result.Runtime != "takod" {
		t.Fatalf("summary fields wrong: %+v", result)
	}
	if !result.MeshEnabled || result.MeshNetworkCIDR == "" {
		t.Fatalf("mesh summary missing: %+v", result)
	}
}

func resetValidateGlobals(t *testing.T) {
	t.Helper()
	oldCfgFile := cfgFile
	oldEnvFlag := envFlag
	oldValidateQuiet := validateQuiet
	cfgFile = ""
	envFlag = ""
	validateQuiet = false
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		envFlag = oldEnvFlag
		validateQuiet = oldValidateQuiet
	})
}

func switchToTempDir(t *testing.T) string {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	return root
}
