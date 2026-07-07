package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/spf13/cobra"
)

func newSecretsTestCommand(env string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("env", "e", "", "")
	if env != "" {
		_ = cmd.Flags().Set("env", env)
	}
	return cmd
}

// TestRunSecretsListMachineOutputNeverCarriesValues is the redaction test the
// machine contract requires: `secrets list --output json` emits keys only.
func TestRunSecretsListMachineOutputNeverCarriesValues(t *testing.T) {
	switchToTempDir(t)
	mgr, err := secrets.NewManager("")
	if err != nil {
		t.Fatalf("failed to create secrets manager: %v", err)
	}
	const secretValue = "super-secret-value-xyzzy"
	if err := mgr.Set("API_KEY", secretValue, ""); err != nil {
		t.Fatalf("failed to set secret: %v", err)
	}

	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runSecretsList(newSecretsTestCommand(""), nil)
	})
	if runErr != nil {
		t.Fatalf("runSecretsList returned error: %v", runErr)
	}
	if strings.Contains(stdout, secretValue) {
		t.Fatalf("secret value leaked into machine output:\n%s", stdout)
	}

	var result engine.SecretsListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Kind != engine.KindSecretsListResult || result.Count != 1 {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if len(result.Keys) != 1 || result.Keys[0] != "API_KEY" {
		t.Fatalf("keys = %+v, want [API_KEY]", result.Keys)
	}
}

func TestRunSecretsValidateMachineOutputReportsMissing(t *testing.T) {
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
        secrets: [DATABASE_URL]
`)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	oldCfgFile := cfgFile
	cfgFile = ""
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { cfgFile, outputFormatFlag = oldCfgFile, restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runSecretsValidate(newSecretsTestCommand("production"), nil)
	})
	if runErr == nil {
		t.Fatal("runSecretsValidate should fail on missing secrets")
	}
	if engine.Classify(runErr) != engine.ClassInvalid {
		t.Fatalf("missing secrets classified as %d, want ClassInvalid", engine.Classify(runErr))
	}

	var result engine.SecretsValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Valid || result.Kind != engine.KindSecretsValidateResult {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "DATABASE_URL" {
		t.Fatalf("missing = %+v, want [DATABASE_URL]", result.Missing)
	}
}

func TestImportedSecretKeyStripsPrefixAndNormalizesPath(t *testing.T) {
	got := importedSecretKey("/truenextglobal/production/twenty/database-url", "/truenextglobal/production/")
	if got != "TWENTY_DATABASE_URL" {
		t.Fatalf("importedSecretKey = %q, want TWENTY_DATABASE_URL", got)
	}
}

func TestMapImportedSecretsUsesExplicitMappingsAndRejectsCollisions(t *testing.T) {
	values := map[string]string{
		"/app/prod/db/url":      "one",
		"/app/prod/db-url":      "two",
		"/app/prod/redis/url":   "three",
		"/app/prod/custom/name": "four",
	}

	_, err := mapImportedSecrets(values, "/app/prod/", nil)
	if err == nil {
		t.Fatal("expected collision without explicit mapping")
	}

	got, err := mapImportedSecrets(values, "/app/prod/", map[string]string{
		"/app/prod/db-url":      "DATABASE_URL_ALT",
		"/app/prod/custom/name": "CUSTOM_NAME",
	})
	if err != nil {
		t.Fatalf("mapImportedSecrets returned error: %v", err)
	}
	if got["DB_URL"] != "one" || got["DATABASE_URL_ALT"] != "two" || got["REDIS_URL"] != "three" || got["CUSTOM_NAME"] != "four" {
		t.Fatalf("mapped secrets = %#v", got)
	}
}

func TestParseSecretMappingsValidatesDestinationKeys(t *testing.T) {
	if _, err := parseSecretMappings([]string{"/path=DATABASE_URL"}); err != nil {
		t.Fatalf("parseSecretMappings returned error: %v", err)
	}
	if _, err := parseSecretMappings([]string{"/path=bad-key"}); err == nil {
		t.Fatal("expected invalid destination key to fail")
	}
}
