package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

const scopedCommandConfig = `project:
  name: scoped-app
  version: 1.0.0
servers:
  preview:
    host: ${CMD_SCOPE_PREVIEW_HOST}
    user: deploy
    sshKey: ${CMD_SCOPE_SSH_KEY}
  production:
    host: ${CMD_SCOPE_PRODUCTION_HOST}
    user: deploy
    sshKey: ${CMD_SCOPE_SSH_KEY}
environments:
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${CMD_SCOPE_PREVIEW_URL}
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${CMD_SCOPE_PRODUCTION_URL}
`

// writeScopedCommandConfig writes a two-environment config with only the
// preview variables set.
func writeScopedCommandConfig(t *testing.T) {
	t.Helper()
	root := switchToTempDir(t)
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	t.Setenv("CMD_SCOPE_SSH_KEY", filepath.ToSlash(sshKey))
	t.Setenv("CMD_SCOPE_PREVIEW_HOST", "203.0.113.10")
	t.Setenv("CMD_SCOPE_PREVIEW_URL", "https://preview.example.com")
	for _, name := range []string{"CMD_SCOPE_PRODUCTION_HOST", "CMD_SCOPE_PRODUCTION_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(scopedCommandConfig), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
}

func runScopedValidate(t *testing.T) (engine.ValidateResult, error) {
	t.Helper()
	restoreOutput := outputFormatFlag
	t.Cleanup(func() { outputFormatFlag = restoreOutput })
	return runValidateJSON(t)
}

func TestValidateWithEnvironmentSkipsOtherEnvironmentVariables(t *testing.T) {
	writeScopedCommandConfig(t)
	resetValidateGlobals(t)
	envFlag = "preview"

	result, err := runScopedValidate(t)
	if err != nil {
		t.Fatalf("runValidate -e preview: %v", err)
	}
	if !result.Valid || result.Environment != "preview" || result.Servers != 1 {
		t.Fatalf("result = %+v, want valid preview with one server", result)
	}
	var warned bool
	for _, finding := range result.Findings {
		if finding.Severity == engine.ValidateSeverityWarn && strings.Contains(finding.Message, "CMD_SCOPE_PRODUCTION_HOST, CMD_SCOPE_PRODUCTION_URL") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("findings = %+v, want a warning naming the skipped variables", result.Findings)
	}
}

func TestValidateWithoutEnvironmentRequiresEveryVariable(t *testing.T) {
	writeScopedCommandConfig(t)
	resetValidateGlobals(t)

	result, err := runScopedValidate(t)
	if err == nil || result.Valid {
		t.Fatalf("result = %+v err = %v, want failure", result, err)
	}
	if !strings.Contains(err.Error(), "missing environment variable(s): CMD_SCOPE_PRODUCTION_HOST, CMD_SCOPE_PRODUCTION_URL") {
		t.Fatalf("err = %v, want every missing variable", err)
	}
}

func TestValidateWithEnvironmentRequiresItsOwnVariables(t *testing.T) {
	writeScopedCommandConfig(t)
	resetValidateGlobals(t)
	envFlag = "preview"
	os.Unsetenv("CMD_SCOPE_PREVIEW_URL")

	_, err := runScopedValidate(t)
	if err == nil || !strings.Contains(err.Error(), "missing environment variable(s): CMD_SCOPE_PREVIEW_URL") || strings.Contains(err.Error(), "PRODUCTION") {
		t.Fatalf("err = %v, want only the preview variable", err)
	}
}

func TestDeployConfigLoadIsScopedToSelectedEnvironment(t *testing.T) {
	writeScopedCommandConfig(t)
	resetValidateGlobals(t)

	// Deploy, validate, and config explain load through the scoped loader.
	envFlag = "preview"
	cfg, err := loadSelectedEnvironmentConfig("")
	if err != nil {
		t.Fatalf("loadSelectedEnvironmentConfig -e preview: %v", err)
	}
	if _, ok := cfg.Servers["production"]; ok {
		t.Fatal("production server with unset host is still in the deploy config")
	}
	if _, ok := cfg.Environments["production"]; ok {
		t.Fatal("production environment is still in the deploy config")
	}
	servers, err := cfg.GetEnvironmentServers(getEnvironmentName(cfg))
	if err != nil || len(servers) != 1 || servers[0] != "preview" {
		t.Fatalf("servers = %v, %v", servers, err)
	}

	// Commands that span every server (upgrade servers) and loads without
	// -e still require every variable.
	if _, err := loadDeployConfig(""); err == nil || !strings.Contains(err.Error(), "CMD_SCOPE_PRODUCTION_HOST") {
		t.Fatalf("loadDeployConfig err = %v, want the production variables", err)
	}
	envFlag = ""
	if _, err := loadSelectedEnvironmentConfig(""); err == nil || !strings.Contains(err.Error(), "CMD_SCOPE_PRODUCTION_URL") {
		t.Fatalf("loadSelectedEnvironmentConfig without -e err = %v, want the production variables", err)
	}
}

func TestConfigExplainWithEnvironmentSkipsOtherEnvironmentVariables(t *testing.T) {
	writeScopedCommandConfig(t)
	resetValidateGlobals(t)
	envFlag = "preview"

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runConfigExplain(cmd, nil); err != nil {
		t.Fatalf("config explain -e preview: %v", err)
	}
	if !strings.Contains(out.String(), "Environment: preview") || !strings.Contains(out.String(), "host: 203.0.113.10 (env)") {
		t.Fatalf("explain output = %s", out.String())
	}
}

// scopedPreviewCommandConfig adds an abstract preview base (review) that
// extends the preview environment; production stays out of its scope.
const scopedPreviewCommandConfig = scopedCommandConfig + `  review:
    abstract: true
    extends: preview
    preview:
      ttl: 72h
`

func TestPreviewConfigLoadIsScopedToPreviewBase(t *testing.T) {
	writeScopedCommandConfig(t)
	if err := os.WriteFile("tako.yaml", []byte(scopedPreviewCommandConfig), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	resetValidateGlobals(t)
	setPreviewFlag(t, "pr-42")

	// deploy --preview -e review and config explain --preview -e review.
	envFlag = "review"
	cfg, err := loadSelectedEnvironmentConfig("")
	if err != nil {
		t.Fatalf("loadSelectedEnvironmentConfig --preview -e review: %v", err)
	}
	if instance := cfg.PreviewInstance(); instance == nil || instance.Identity != "preview-pr-42" || instance.Base != "review" {
		t.Fatalf("preview instance = %+v", instance)
	}
	if getEnvironmentName(cfg) != "preview-pr-42" {
		t.Fatalf("environment = %q", getEnvironmentName(cfg))
	}
	if _, ok := cfg.Environments["production"]; ok {
		t.Fatal("production environment is still in the preview config")
	}
	if _, ok := cfg.Servers["production"]; ok {
		t.Fatal("production server with unset host is still in the preview config")
	}

	// ps/logs/exec/history/rollback --preview -e review.
	stubPreviewRecord(t, previewListItem("pr-42", "review"), nil)
	cfg, err = loadCommandConfigOrError(context.Background(), "")
	if err != nil {
		t.Fatalf("loadCommandConfigOrError --preview -e review: %v", err)
	}
	if getEnvironmentName(cfg) != "preview-pr-42" {
		t.Fatalf("environment = %q", getEnvironmentName(cfg))
	}
	if _, ok := cfg.Servers["production"]; ok {
		t.Fatal("production server is still in the preview command config")
	}

	// The base chain's variables are required.
	os.Unsetenv("CMD_SCOPE_PREVIEW_URL")
	if _, err := loadSelectedEnvironmentConfig(""); err == nil || !strings.Contains(err.Error(), "CMD_SCOPE_PREVIEW_URL") || strings.Contains(err.Error(), "PRODUCTION") {
		t.Fatalf("err = %v, want only the base variable", err)
	}
	t.Setenv("CMD_SCOPE_PREVIEW_URL", "https://preview.example.com")

	// Without -e every variable is required.
	envFlag = ""
	if _, err := loadSelectedEnvironmentConfig(""); err == nil || !strings.Contains(err.Error(), "CMD_SCOPE_PRODUCTION") {
		t.Fatalf("err = %v, want the production variables without -e", err)
	}
}