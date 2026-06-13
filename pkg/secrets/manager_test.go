package secrets

import (
	"os"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestCreateEnvFileExpandsBracedEnvFromOSAndSecrets(t *testing.T) {
	withTempWorkingDir(t)
	t.Setenv("DB_HOST", "db.internal")
	mgr := &Manager{secrets: map[string]string{"TOKEN": "secret-token"}}

	envFile, err := mgr.CreateEnvFile(&config.ServiceConfig{
		Env: map[string]string{
			"DATABASE_URL": "postgres://${DB_HOST}:5432/app",
			"API_TOKEN":    "${TOKEN}",
		},
	})
	if err != nil {
		t.Fatalf("CreateEnvFile returned error: %v", err)
	}

	if got, _ := envFile.Get("DATABASE_URL"); got != "postgres://db.internal:5432/app" {
		t.Fatalf("DATABASE_URL = %q", got)
	}
	if got, _ := envFile.Get("API_TOKEN"); got != "secret-token" {
		t.Fatalf("API_TOKEN = %q", got)
	}
}

func TestCreateEnvFilePreservesBareDollarValues(t *testing.T) {
	withTempWorkingDir(t)
	mgr := &Manager{secrets: map[string]string{}}

	envFile, err := mgr.CreateEnvFile(&config.ServiceConfig{
		Env: map[string]string{
			"PASSWORD_HASH": "$2a$10$abcdefghijklmnopqrstuv",
		},
	})
	if err != nil {
		t.Fatalf("CreateEnvFile returned error: %v", err)
	}

	if got, _ := envFile.Get("PASSWORD_HASH"); got != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatalf("PASSWORD_HASH = %q", got)
	}
}

func TestCreateEnvFileReportsMissingBracedEnv(t *testing.T) {
	withTempWorkingDir(t)
	mgr := &Manager{secrets: map[string]string{}}

	_, err := mgr.CreateEnvFile(&config.ServiceConfig{
		Env: map[string]string{
			"DATABASE_URL": "postgres://${DB_HOST}:5432/app",
		},
	})
	if err == nil {
		t.Fatal("CreateEnvFile should report missing braced env references")
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Fatalf("error = %q, want DB_HOST", err)
	}
}

func TestExecuteCommandRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("TAKO_ALLOW_SECRET_COMMANDS", "")
	mgr := &Manager{}

	_, err := mgr.executeCommand("tako version")
	if err == nil {
		t.Fatal("executeCommand should reject command substitution by default")
	}
	if !strings.Contains(err.Error(), "TAKO_ALLOW_SECRET_COMMANDS=1") {
		t.Fatalf("error = %q, want opt-in hint", err)
	}
}

func TestExecuteCommandKeepsAllowlistWhenOptedIn(t *testing.T) {
	t.Setenv("TAKO_ALLOW_SECRET_COMMANDS", "1")
	mgr := &Manager{}

	_, err := mgr.executeCommand("sh -c echo unsafe")
	if err == nil {
		t.Fatal("executeCommand should reject commands outside the allowlist")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %q, want allowlist error", err)
	}
}

func withTempWorkingDir(t *testing.T) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("failed to switch working dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
}
