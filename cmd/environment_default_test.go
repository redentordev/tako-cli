package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

const implicitDefaultNotice = "no -e and no 'production'; using 'dev'; pass -e\n"

// captureStdoutAndStderr runs fn with both process streams redirected.
func captureStdoutAndStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}
	os.Stderr = writer
	var stdout string
	func() {
		defer func() { os.Stderr = original }()
		stdout = captureStdout(t, fn)
	}()
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close stderr pipe: %v", err)
	}
	stderr, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read stderr pipe: %v", err)
	}
	return stdout, string(stderr)
}

func writeDefaultEnvironmentConfig(t *testing.T, environments ...string) {
	t.Helper()
	root := switchToTempDir(t)
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	var b strings.Builder
	b.WriteString("project:\n  name: demo\n  version: 1.0.0\nservers:\n  node-a:\n    host: 203.0.113.10\n    user: deploy\n    sshKey: " + filepath.ToSlash(sshKey) + "\nenvironments:\n")
	for _, name := range environments {
		b.WriteString("  " + name + ":\n    servers: [node-a]\n    services:\n      web:\n        image: nginx:alpine\n")
	}
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(b.String()), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
}

func resetImplicitEnvironmentNotice(t *testing.T) {
	t.Helper()
	implicitEnvironmentNoticeMu.Lock()
	implicitEnvironmentNoticeShown = map[string]bool{}
	implicitEnvironmentNoticeMu.Unlock()
	t.Cleanup(func() {
		implicitEnvironmentNoticeMu.Lock()
		implicitEnvironmentNoticeShown = map[string]bool{}
		implicitEnvironmentNoticeMu.Unlock()
	})
}

func runValidateJSONCapture(t *testing.T) (engine.ValidateResult, string) {
	t.Helper()
	validateQuiet = true
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runValidate returned error: %v", runErr)
	}
	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	return result, stderr
}

func TestImplicitDefaultEnvironmentNoticeGoesToStderrOnly(t *testing.T) {
	writeDefaultEnvironmentConfig(t, "staging", "dev")
	resetValidateGlobals(t)
	resetImplicitEnvironmentNotice(t)

	result, stderr := runValidateJSONCapture(t)
	if result.Environment != "dev" || !result.Valid {
		t.Fatalf("result = %+v, want dev", result)
	}
	if stderr != implicitDefaultNotice {
		t.Fatalf("stderr = %q, want exactly %q", stderr, implicitDefaultNotice)
	}

	// The notice is printed once per process.
	if name := getEnvironmentName(mustLoadDefaultEnvironmentConfig(t)); name != "dev" {
		t.Fatalf("second pick = %q", name)
	}
}

func mustLoadDefaultEnvironmentConfig(t *testing.T) interface {
	ResolveDefaultEnvironment() (string, bool)
} {
	t.Helper()
	cfg, err := loadDeployConfig("")
	if err != nil {
		t.Fatalf("loadDeployConfig: %v", err)
	}
	return cfg
}

func TestImplicitDefaultEnvironmentIsStable(t *testing.T) {
	writeDefaultEnvironmentConfig(t, "staging", "dev")
	resetValidateGlobals(t)
	resetImplicitEnvironmentNotice(t)
	cfg := mustLoadDefaultEnvironmentConfig(t)
	for i := 0; i < 50; i++ {
		var name string
		_, stderr := captureStdoutAndStderr(t, func() { name = getEnvironmentName(cfg) })
		if name != "dev" {
			t.Fatalf("run %d picked %q, want dev", i, name)
		}
		if i > 0 && stderr != "" {
			t.Fatalf("run %d repeated the notice: %q", i, stderr)
		}
	}
}

func TestNoImplicitDefaultNoticeWithProductionOrEnvFlag(t *testing.T) {
	t.Run("production present", func(t *testing.T) {
		writeDefaultEnvironmentConfig(t, "production", "staging", "dev")
		resetValidateGlobals(t)
		resetImplicitEnvironmentNotice(t)
		result, stderr := runValidateJSONCapture(t)
		if result.Environment != "production" || stderr != "" {
			t.Fatalf("environment = %q stderr = %q, want production and no notice", result.Environment, stderr)
		}
	})
	t.Run("-e given", func(t *testing.T) {
		writeDefaultEnvironmentConfig(t, "staging", "dev")
		resetValidateGlobals(t)
		resetImplicitEnvironmentNotice(t)
		envFlag = "staging"
		result, stderr := runValidateJSONCapture(t)
		if result.Environment != "staging" || stderr != "" {
			t.Fatalf("environment = %q stderr = %q, want staging and no notice", result.Environment, stderr)
		}
	})
}

func TestEnvFlagHelpDescribesDefaultSelection(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("env")
	if flag == nil {
		t.Fatal("missing --env flag")
	}
	if want := "environment to use (default: the only environment, else production, else the first alphabetically)"; flag.Usage != want {
		t.Fatalf("--env usage = %q, want %q", flag.Usage, want)
	}
}
