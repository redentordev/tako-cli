package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func labeledEngine(t *testing.T) *Engine {
	t.Helper()
	t.Setenv(secrets.RedactionLabelsEnv, "")
	return New(Options{})
}

func TestRegistrationHelpersLabelValues(t *testing.T) {
	eng := labeledEngine(t)
	cfg := &config.Config{
		Registries: map[string]config.RegistryConfig{"ghcr.io": {Username: "octocat", Password: "gh-registry-token"}},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Proxy: &config.EnvironmentProxyConfig{ACME: &config.EnvironmentACMEConfig{
				DNSProvider: "cloudflare", Credentials: map[string]string{"apiToken": "dns-zone-token"},
			}}},
		},
		Servers:       map[string]config.ServerConfig{"app": {Host: "app.example.test", Password: "ssh-pass-word"}},
		Notifications: &config.NotificationsConfig{Slack: "https://hooks.slack.example/T1/B2/slack-hook-path"},
	}
	services := map[string]config.ServiceConfig{
		"web": {Env: map[string]string{"API_TOKEN": "literal-env-secret", "CMS_RUNTIME_MODE": "site"}},
		"mongo": {Backup: &config.BackupConfig{Storage: &config.BackupStorageConfig{
			AccessKeyID: "backup-access-id", SecretAccessKey: "backup-secret-key",
		}}},
	}
	eng.RegisterServiceEnvValues(services)
	eng.RegisterRegistrySecrets(cfg)
	eng.RegisterACMEDNSSecrets(cfg)
	eng.RegisterServerPasswords(cfg.Servers)
	eng.RegisterNotificationSecrets(cfg)
	eng.RegisterBackupSecrets(services)

	cases := map[string]string{
		"literal-env-secret": "[REDACTED:env.API_TOKEN]",
		"gh-registry-token":  "[REDACTED:registries.ghcr.io.password]",
		"dns-zone-token":     "[REDACTED:environments.production.proxy.acme.apiToken]",
		"ssh-pass-word":      "[REDACTED:servers.app.password]",
		"https://hooks.slack.example/T1/B2/slack-hook-path": "[REDACTED:notifications.slack]",
		"backup-access-id":  "[REDACTED:services.mongo.backup.accessKeyId]",
		"backup-secret-key": "[REDACTED:services.mongo.backup.secretAccessKey]",
	}
	for value, want := range cases {
		if got := eng.RedactSensitive("v " + value + " end"); got != "v "+want+" end" {
			t.Errorf("%s: got %q, want marker %s", value, got, want)
		}
	}
	if got := eng.RedactSensitive("mode site"); got != "mode site" {
		t.Errorf("plain setting was masked: %q", got)
	}
}

func TestBackupSecretMaskedInHealthFailureError(t *testing.T) {
	eng := labeledEngine(t)
	eng.RegisterBackupSecrets(map[string]config.ServiceConfig{
		"mongo": {Backup: &config.BackupConfig{Storage: &config.BackupStorageConfig{SecretAccessKey: "wJalrXUtnFEMI-backup-key"}}},
	})
	err := fmt.Errorf("service mongo failed health check; last log lines:\n  AWS_SECRET_ACCESS_KEY wJalrXUtnFEMI-backup-key rejected")
	got := eng.RedactSensitive(err.Error())
	if strings.Contains(got, "wJalrXUtnFEMI-backup-key") || !strings.Contains(got, "[REDACTED:services.mongo.backup.secretAccessKey]") {
		t.Fatalf("got %q", got)
	}
}

func writeEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterServiceEnvFileValues(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.production")
	writeEnvFile(t, envPath, "SMTP_PASSWORD=pa55w0rd-smtp\nLOG_LEVEL=debug\nSESSION_ID_SEED=abcd1234efgh5678ijkl\n")

	eng := labeledEngine(t)
	if err := eng.RegisterServiceEnvFileValues(map[string]config.ServiceConfig{"admin": {EnvFile: envPath}}); err != nil {
		t.Fatal(err)
	}
	if got := eng.RedactSensitive("pw pa55w0rd-smtp"); got != "pw [REDACTED:envFile.SMTP_PASSWORD]" {
		t.Errorf("credential-like name: %q", got)
	}
	if got := eng.RedactSensitive("level debug"); got != "level debug" {
		t.Errorf("plain setting masked: %q", got)
	}
	if got := eng.RedactSensitive("seed abcd1234efgh5678ijkl"); got != "seed [REDACTED:envFile.SESSION_ID_SEED]" {
		t.Errorf("token-shaped value: %q", got)
	}

	missing := labeledEngine(t)
	err := missing.RegisterServiceEnvFileValues(map[string]config.ServiceConfig{"admin": {EnvFile: filepath.Join(dir, "missing.env")}})
	if err == nil {
		t.Fatalf("unreadable envFile must return an error")
	}
	failures := EnvFileUnreadableErrors(err)
	if len(failures) != 1 || failures[0].Service != "admin" {
		t.Fatalf("failures = %#v", failures)
	}
	if labels := missing.redactor.Labels(); len(labels) != 0 {
		t.Fatalf("unreadable envFile registered labels: %v", labels)
	}
}

func TestStreamLogsRedactsEnvFileValues(t *testing.T) {
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("web", "1"), lineFrame("web", "1", "smtp login pa55w0rd-smtp failed")}}
	t.Setenv(secrets.RedactionLabelsEnv, "")
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	writeEnvFile(t, ".env.production", "SMTP_PASSWORD=pa55w0rd-smtp\n")
	cfg := logsTestConfig("node-a")
	web := cfg.Environments["production"].Services["web"]
	web.EnvFile = ".env.production"
	cfg.Environments["production"].Services["web"] = web

	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: cfg, Environment: "production", Service: "web"}); err != nil {
		t.Fatal(err)
	}
	lines := logLineEvents(sink)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
	want := "smtp login [REDACTED:envFile.SMTP_PASSWORD] failed"
	if lines[0].Data["data"] != want || !strings.Contains(lines[0].Message, want) {
		t.Fatalf("message = %q data = %v", lines[0].Message, lines[0].Data["data"])
	}
	if warnings := eventsOfType(sink, events.TypeWarning); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestStreamLogsWarnsOnceForUnreadableEnvFile(t *testing.T) {
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("web", "1"), lineFrame("web", "1", "still streaming")}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	cfg := logsTestConfig("node-a")
	web := cfg.Environments["production"].Services["web"]
	web.EnvFile = ".env.production"
	cfg.Environments["production"].Services["web"] = web

	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: cfg, Environment: "production", Service: "web"}); err != nil {
		t.Fatal(err)
	}
	var unreadable []events.Event
	for _, warning := range eventsOfType(sink, events.TypeWarning) {
		if warning.Data["reason"] == EnvFileUnreadableReason {
			unreadable = append(unreadable, warning)
		}
	}
	if len(unreadable) != 1 {
		t.Fatalf("envfile-unreadable warnings = %#v", unreadable)
	}
	if !strings.Contains(unreadable[0].Message, "could not read envFile for web; its values will not be redacted") {
		t.Fatalf("message = %q", unreadable[0].Message)
	}
	if lines := logLineEvents(sink); len(lines) != 1 || lines[0].Data["data"] != "still streaming" {
		t.Fatalf("lines = %#v", lines)
	}
}

// TestClassifierParityWithEngineRegistration checks that the labels the
// engine registers equal the Masked entries `config explain --redaction`
// derives from the same classifier.
func TestClassifierParityWithEngineRegistration(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.production")
	writeEnvFile(t, envPath, "SMTP_PASSWORD=pa55w0rd-smtp\nLOG_LEVEL=debug\n")
	cfg := &config.Config{
		Project:       config.ProjectConfig{Name: "demo"},
		Registries:    map[string]config.RegistryConfig{"ghcr.io": {Password: "gh-registry-token"}, "gcr.io": {Provider: "gcr"}},
		Servers:       map[string]config.ServerConfig{"app": {Password: "ssh-pass-word"}, "db": {}},
		Notifications: &config.NotificationsConfig{Discord: "https://discord.example/api/webhooks/123/discord-hook-token"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Proxy: &config.EnvironmentProxyConfig{ACME: &config.EnvironmentACMEConfig{
				DNSProvider: "cloudflare", Credentials: map[string]string{"apiToken": "dns-zone-token"},
			}}},
		},
	}
	services := map[string]config.ServiceConfig{
		"web": {EnvFile: envPath, Env: map[string]string{"API_TOKEN": "literal-env-secret", "NODE_ENV": "production"}, Secrets: []string{"API_KEY:STRIPE_KEY"}},
		"mongo": {Backup: &config.BackupConfig{Storage: &config.BackupStorageConfig{
			AccessKeyID: "backup-access-id", SecretAccessKey: "backup-secret-key",
		}}},
	}
	// secrets: and passEnv values are supplied by the test.
	secretValues := map[string]string{"STRIPE_KEY": "sk-stripe-value"}
	passEnv := map[string]string{"DEPLOY_TOKEN": "deploy-token-value"}

	eng := labeledEngine(t)
	eng.RegisterServiceEnvValues(services)
	if err := eng.RegisterServiceEnvFileValues(services); err != nil {
		t.Fatal(err)
	}
	eng.RegisterServerPasswords(cfg.Servers)
	eng.RegisterRegistrySecrets(cfg)
	eng.RegisterACMEDNSSecrets(cfg)
	eng.RegisterBackupSecrets(services)
	eng.RegisterNotificationSecrets(cfg)
	for key, value := range secretValues {
		eng.RegisterSecretAs(value, secrets.SecretLabel(key))
	}
	for name, value := range passEnv {
		eng.RegisterSecretAs(value, secrets.HookPassEnvLabel(name))
	}
	registered := eng.redactor.Labels()

	entries := secrets.ClassifyConfigCredentials(cfg, services)
	entries = append(entries, secrets.ClassifyServiceEnv(services)...)
	fileEntries, failures := secrets.ClassifyServiceEnvFiles(services)
	if len(failures) != 0 {
		t.Fatalf("failures = %#v", failures)
	}
	entries = append(entries, fileEntries...)
	var classified []string
	for _, entry := range entries {
		if entry.Masked && entry.Value != "" {
			classified = append(classified, entry.Label)
		}
		// A provider registry is reported as masked without a value: its
		// token is minted (and registered) only by an operation that pulls
		// or builds, never by registration or classification.
		if entry.Reason == secrets.ReasonRegistryProviderToken && (entry.Value != "" || entry.Label != secrets.RegistryTokenLabel("gcr.io")) {
			t.Fatalf("provider registry entry = %+v", entry)
		}
	}
	for key := range secretValues {
		classified = append(classified, secrets.SecretLabel(key))
	}
	for name := range passEnv {
		classified = append(classified, secrets.HookPassEnvLabel(name))
	}
	sort.Strings(classified)
	if strings.Join(registered, ",") != strings.Join(classified, ",") {
		t.Fatalf("registered labels differ from classifier:\n registered %v\n classified %v", registered, classified)
	}
}

// TestNoUnlabeledRegisterSecretCalls fails when non-test code under
// pkg/engine or cmd registers a secret without a label.
func TestNoUnlabeledRegisterSecretCalls(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate source")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	call := regexp.MustCompile(`\bRegisterSecret\(`)
	var offenders []string
	for _, dir := range []string{"pkg/engine", "cmd"} {
		paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				if call.MatchString(line) && !strings.Contains(line, "func (e *Engine) RegisterSecret(") {
					offenders = append(offenders, fmt.Sprintf("%s:%d", filepath.ToSlash(path), i+1))
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("unlabeled RegisterSecret calls (use RegisterSecretAs with a label):\n%s", strings.Join(offenders, "\n"))
	}
}
