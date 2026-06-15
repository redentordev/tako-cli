package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFileExpandsBracedVariables(t *testing.T) {
	t.Setenv("DB_HOST", "db.internal")
	path := writeEnvFixture(t, "DATABASE_URL=postgres://${DB_HOST}:5432/app\n")

	vars, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("LoadEnvFile returned error: %v", err)
	}
	if vars["DATABASE_URL"] != "postgres://db.internal:5432/app" {
		t.Fatalf("DATABASE_URL = %q", vars["DATABASE_URL"])
	}
}

func TestLoadEnvFilePreservesBareDollarSecrets(t *testing.T) {
	path := writeEnvFixture(t, "PASSWORD_HASH=$2a$10$abcdefghijklmnopqrstuv\n")

	vars, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("LoadEnvFile returned error: %v", err)
	}
	if vars["PASSWORD_HASH"] != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatalf("PASSWORD_HASH = %q", vars["PASSWORD_HASH"])
	}
}

func TestLoadEnvFileReportsMissingBracedVariables(t *testing.T) {
	path := writeEnvFixture(t, "DATABASE_URL=postgres://${DB_HOST}:5432/app\n")

	_, err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("LoadEnvFile should report missing braced variables")
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Fatalf("error = %q, want DB_HOST", err)
	}
}

func writeEnvFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write env fixture: %v", err)
	}
	return path
}
