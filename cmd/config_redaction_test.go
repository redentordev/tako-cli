package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/providercli"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/spf13/cobra"
)

const jardinRedactionConfig = `project:
  name: jardin-cms
  version: 1.0.0
servers:
  app:
    host: 203.0.113.10
    user: root
    password: ${TAKO_APP_PASSWORD}
registries:
  ghcr.io:
    username: octocat
    password: ${GHCR_TOKEN}
notifications:
  slack: ${SLACK_WEBHOOK_URL}
environments:
  production:
    servers: [app]
    secretSources:
      - provider: aws-ssm
        names: [/prod/app/SENDGRID_KEY]
        prefixStrip: /prod/app/
    services:
      site-renderer:
        image: ghcr.io/jardin/site:1.0
        env:
          CMS_RUNTIME_MODE: site
          API_TOKEN: abc
      admin:
        image: ghcr.io/jardin/admin:1.0
        envFile: .env.production
        env:
          BETTER_AUTH_SECRET: better-auth-secret-value
          NODE_ENV: production
          MONGODB_URI: mongodb://mongo:27017/admin
        secrets:
          - STRIPE_KEY
          - SENDGRID_KEY
          - LEGACY_KEY
      mongo:
        image: mongo:7
        backup:
          storage:
            provider: s3
            bucket: jardin-backups
            region: us-east-1
            accessKeyId: ${BACKUP_ACCESS_KEY_ID}
            secretAccessKey: ${BACKUP_SECRET_ACCESS_KEY}
`

// jardinFixtureValues are every secret or env value of the fixture; none may
// appear in the report.
var jardinFixtureValues = []string{
	"ssh-app-password-value",
	"ghcr-token-value",
	"https://hooks.slack.example/services/T000/B000/slackhooktoken1234",
	"backup-access-key-id-value",
	"backup-secret-access-key-value",
	"better-auth-secret-value",
	"pa55w0rd-smtp",
	"stripe-local-secret-value",
	"/prod/app/",
	"abc",
}

func writeJardinRedactionFixture(t *testing.T) {
	t.Helper()
	root := switchToTempDir(t)
	t.Setenv("TAKO_APP_PASSWORD", "ssh-app-password-value")
	t.Setenv("GHCR_TOKEN", "ghcr-token-value")
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.example/services/T000/B000/slackhooktoken1234")
	t.Setenv("BACKUP_ACCESS_KEY_ID", "backup-access-key-id-value")
	t.Setenv("BACKUP_SECRET_ACCESS_KEY", "backup-secret-access-key-value")
	t.Setenv(secrets.RedactionLabelsEnv, "")
	for path, content := range map[string]string{
		"tako.yaml":       jardinRedactionConfig,
		".env.production": "SMTP_PASSWORD=pa55w0rd-smtp\nLOG_LEVEL=debug\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".tako"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(filepath.Join(root, ".tako", "encryption.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tako", "secrets.production"), []byte("STRIPE_KEY=stripe-local-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetConfigExplainGlobals(t)
	old := configExplainRedaction
	configExplainRedaction = true
	t.Cleanup(func() { configExplainRedaction = old })
}

func runRedactionReport(t *testing.T) string {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runConfigExplain(cmd, nil); err != nil {
		t.Fatalf("runConfigExplain --redaction returned error: %v", err)
	}
	return out.String()
}

func TestConfigExplainRedactionGolden(t *testing.T) {
	writeJardinRedactionFixture(t)
	got := runRedactionReport(t)
	want := `Redaction: production (config tako.yaml)
Labels: on (TAKO_REDACTION_LABELS)

Masked
  servers.app.password                   ssh password
  registries.ghcr.io.password            registry credential
  services.mongo.backup.accessKeyId      backup storage credential
  services.mongo.backup.secretAccessKey  backup storage credential
  notifications.slack                    notification URL

Services
  admin:
    masked   secrets.STRIPE_KEY      secrets list (from .tako/secrets.production)
    masked   secrets.SENDGRID_KEY    secrets list (from secretSources[0] aws-ssm; not resolved here)
    masked   env.BETTER_AUTH_SECRET  credential-like name
    masked   envFile.SMTP_PASSWORD   credential-like name (.env.production)
    visible  env.MONGODB_URI         plain setting (URL without credentials)
    visible  env.NODE_ENV            plain setting
    visible  envFile.LOG_LEVEL       plain setting (.env.production)
  mongo:
    (no secrets, env or envFile values)
  site-renderer:
    masked   env.API_TOKEN         credential-like name
    visible  env.CMS_RUNTIME_MODE  plain setting

Not resolvable
  secrets.LEGACY_KEY (admin)  not in .tako/secrets* and no secretSources; will not be redacted

Note: secretSources are not resolved here; sourced keys are verified at deploy/logs time (preview with ` + "`tako secrets fetch`" + `).

Always visible: project jardin-cms; environments production; servers app; service names; service images
Patterns: assignment, connection-string, stripe-key, github-token, npm-token, digitalocean-token, aws-access-key, google-api-key, hex64, jwt, base64, hex (heuristic, labeled pattern.<id>)
`
	if got != want {
		t.Fatalf("report mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestConfigExplainRedactionPrintsNoValues(t *testing.T) {
	writeJardinRedactionFixture(t)
	got := runRedactionReport(t)
	for _, value := range jardinFixtureValues {
		if value == "abc" {
			// `abc` must not appear as a value; it may occur inside words.
			for _, field := range strings.Fields(got) {
				if field == "abc" {
					t.Fatalf("report printed value %q:\n%s", value, got)
				}
			}
			continue
		}
		if strings.Contains(got, value) {
			t.Fatalf("report printed value %q:\n%s", value, got)
		}
	}
	for _, field := range strings.Fields(got) {
		if field == "site" || field == "debug" || field == "production" && !strings.Contains(got, "environments production") {
			t.Fatalf("report printed a visible value %q:\n%s", field, got)
		}
	}
}

func TestConfigExplainRedactionListsProviderRegistryWithoutMinting(t *testing.T) {
	writeJardinRedactionFixture(t)
	config := strings.Replace(jardinRedactionConfig, "registries:\n", "registries:\n  gcr.io:\n    provider: gcr\n", 1)
	if err := os.WriteFile("tako.yaml", []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := providercli.SetForTesting(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("config explain --redaction ran provider CLI %s", name)
		return nil
	}, func(name string) (string, error) {
		t.Fatalf("config explain --redaction looked up provider CLI %s", name)
		return "", nil
	})
	t.Cleanup(restore)
	got := runRedactionReport(t)
	if !regexp.MustCompile(`(?m)^  registries\.gcr\.io\.token +registry token \(provider\)$`).MatchString(got) {
		t.Fatalf("provider registry not reported as masked:\n%s", got)
	}
}

func TestConfigExplainRedactionLabelsOff(t *testing.T) {
	writeJardinRedactionFixture(t)
	t.Setenv(secrets.RedactionLabelsEnv, "off")
	got := runRedactionReport(t)
	if !strings.Contains(got, "Labels: off (TAKO_REDACTION_LABELS=off)") {
		t.Fatalf("report does not state labels are off:\n%s", got)
	}
}

func TestConfigExplainRedactionUnresolvableWithoutSources(t *testing.T) {
	writeJardinRedactionFixture(t)
	config := strings.Replace(jardinRedactionConfig, `    secretSources:
      - provider: aws-ssm
        names: [/prod/app/SENDGRID_KEY]
        prefixStrip: /prod/app/
`, "", 1)
	if err := os.WriteFile("tako.yaml", []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runRedactionReport(t)
	for _, want := range []string{
		"secrets.LEGACY_KEY (admin)  not in .tako/secrets* and no secretSources; will not be redacted",
		"secrets.SENDGRID_KEY (admin)  not in .tako/secrets*",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestConfigExplainRedactionListingSourceIsNotResolved(t *testing.T) {
	writeJardinRedactionFixture(t)
	config := strings.Replace(jardinRedactionConfig, `        names: [/prod/app/SENDGRID_KEY]
        prefixStrip: /prod/app/
`, `        path: /prod/
`, 1)
	if err := os.WriteFile("tako.yaml", []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runRedactionReport(t)
	if !strings.Contains(got, "secrets.SENDGRID_KEY    secrets list (from secretSources[0] aws-ssm; not resolved here)") {
		t.Fatalf("sourced key not reported:\n%s", got)
	}
	if strings.Contains(got, "/prod/") {
		t.Fatalf("provider locator leaked:\n%s", got)
	}
}

func TestConfigExplainRedactionReportsUnreadableEnvFile(t *testing.T) {
	writeJardinRedactionFixture(t)
	cfg, err := loadDeployConfig("")
	if err != nil {
		t.Fatal(err)
	}
	// Config validation rejects a missing file, so remove it after loading,
	// as if it vanished or became unreadable.
	if err := os.Remove(".env.production"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runConfigRedactionReport(&out, cfg, "tako.yaml", "production"); err != nil {
		t.Fatalf("an unreadable envFile must not fail the report: %v", err)
	}
	if !strings.Contains(out.String(), "envFile unreadable: .env.production; its values will not be redacted") {
		t.Fatalf("unreadable envFile not reported:\n%s", out.String())
	}
}

func TestConfigExplainRedactionRejectsMachineOutput(t *testing.T) {
	restoreOutput, restoreEvents := outputFormatFlag, eventsFormatFlag
	t.Cleanup(func() { outputFormatFlag, eventsFormatFlag = restoreOutput, restoreEvents })
	old := configExplainRedaction
	configExplainRedaction = true
	t.Cleanup(func() { configExplainRedaction = old })

	outputFormatFlag = outputFormatJSON
	err := rejectMachineOutputForHumanOnly(configExplainCmd)
	var invalid *engine.InvalidRequestError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "human-only") {
		t.Fatalf("err = %v, want the human-only rejection", err)
	}
	if configExplainCmd.Flags().Lookup("redaction") == nil {
		t.Fatalf("--redaction flag is not registered")
	}
}
