package secrets

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestResolveSecretSourcesMapsKeysAndLaterSourcesWin(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()
	resetSecretSourceCache()
	t.Cleanup(resetSecretSourceCache)

	got, err := ResolveSecretSources(context.Background(), []config.SecretSource{
		{Provider: "doppler", Project: "api", ProviderEnv: "prd"},
		{
			Provider:    "aws-ssm",
			Path:        "/app/prod",
			PrefixStrip: "/app/prod",
			Map:         map[string]string{"/app/prod/database-url": "DATABASE_URL"},
		},
	})
	if err != nil {
		t.Fatalf("ResolveSecretSources returned error: %v", err)
	}
	if got["API_KEY"] != "doppler-api" {
		t.Fatalf("doppler key missing: %#v", got)
	}
	// Both sources define DATABASE_URL; the later SSM source wins.
	if got["DATABASE_URL"] != "value-/app/prod/database-url" {
		t.Fatalf("DATABASE_URL = %q, want SSM value", got["DATABASE_URL"])
	}
}

func TestResolveSecretSourcesCachesPerSource(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()
	resetSecretSourceCache()
	t.Cleanup(resetSecretSourceCache)

	sources := []config.SecretSource{{Provider: "bitwarden", Names: []string{"API_KEY"}}}
	for i := 0; i < 3; i++ {
		if _, err := ResolveSecretSources(context.Background(), sources); err != nil {
			t.Fatalf("ResolveSecretSources returned error: %v", err)
		}
	}
	if entries := readProviderCommandLog(t, logPath); len(entries) != 1 {
		t.Fatalf("provider queried %d times, want 1: %#v", len(entries), entries)
	}
}

func TestResolveSecretSourcesReportsFailingSource(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()
	resetSecretSourceCache()
	t.Cleanup(resetSecretSourceCache)

	_, err := ResolveSecretSources(context.Background(), []config.SecretSource{
		{Provider: "doppler"},
		{Provider: "doppler", Names: []string{"MISSING"}},
	})
	if err == nil || !strings.Contains(err.Error(), "secret source 2 (doppler)") || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerLayersSourcedValuesOverLocalSecrets(t *testing.T) {
	m := &Manager{
		basePath: t.TempDir(),
		secrets:  map[string]string{},
		sourced:  map[string]string{"DATABASE_URL": "from-provider"},
		redactor: NewRedactor(),
	}
	if err := m.loadSecrets(); err != nil {
		t.Fatalf("loadSecrets returned error: %v", err)
	}
	if value, err := m.Get("DATABASE_URL"); err != nil || value != "from-provider" {
		t.Fatalf("Get = %q, %v", value, err)
	}
	if redacted := m.GetRedactor().Redact("dsn=from-provider"); strings.Contains(redacted, "from-provider") {
		t.Fatalf("sourced value not registered with redactor: %q", redacted)
	}
	if redacted := m.GetRedactor().Redact("dsn from-provider"); redacted != "dsn [REDACTED:secrets.DATABASE_URL]" {
		t.Fatalf("sourced value not labeled by key: %q", redacted)
	}
}
