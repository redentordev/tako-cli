package secrets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
)

func TestNewManagerWritesGitignoreForSecretsAndProjectKey(t *testing.T) {
	withTempWorkingDir(t)

	if _, err := NewManager("production"); err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(".tako", ".gitignore"))
	if err != nil {
		t.Fatalf("failed to read .tako/.gitignore: %v", err)
	}

	got := string(data)
	for _, want := range []string{"secrets*", "encryption.key", "*.key", "*.env", "state.json", "deployments/", "logs/"} {
		if !strings.Contains(got, want) {
			t.Fatalf(".tako/.gitignore = %q, want %q", got, want)
		}
	}
}

func TestManagerSetPreservesExistingEncryptedSecrets(t *testing.T) {
	withTempWorkingDir(t)

	mgr, err := NewManager("production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if err := mgr.Set("FIRST_SECRET", "one", "production"); err != nil {
		t.Fatalf("Set FIRST_SECRET returned error: %v", err)
	}
	if err := mgr.Set("SECOND_SECRET", "two", "production"); err != nil {
		t.Fatalf("Set SECOND_SECRET returned error: %v", err)
	}

	reloaded, err := NewManager("production")
	if err != nil {
		t.Fatalf("reloading manager returned error: %v", err)
	}
	if got, err := reloaded.Get("FIRST_SECRET"); err != nil || got != "one" {
		t.Fatalf("FIRST_SECRET = %q, %v; want one", got, err)
	}
	if got, err := reloaded.Get("SECOND_SECRET"); err != nil || got != "two" {
		t.Fatalf("SECOND_SECRET = %q, %v; want two", got, err)
	}

	encrypted, err := crypto.IsFileEncrypted(filepath.Join(".tako", "secrets.production"))
	if err != nil {
		t.Fatalf("failed to inspect encrypted secrets file: %v", err)
	}
	if !encrypted {
		t.Fatal("secrets.production should be encrypted after Set")
	}
}

// TestManagerSetRoundTripsSpecialCharacterValues pins byte-identical
// Set/Get round trips for values that stress the env-file quoting layer.
// Dollar signs are the regression focus: godotenv expands unescaped $NAME
// and ${NAME} references inside double-quoted values on read, which used to
// silently corrupt secrets like passwords and JSON templates.
func TestManagerSetRoundTripsSpecialCharacterValues(t *testing.T) {
	withTempWorkingDir(t)

	cases := map[string]string{
		"PLAIN":        "hunter2",
		"SIMPLE_JSON":  `{"a":"b","n":1}`,
		"NESTED_JSON":  `{"msg":"say \"hi\""}`,
		"JSON_DOLLAR":  `{"tpl":"${GREETING} world"}`,
		"DOLLAR_REF":   "pre${HOME}post",
		"DOLLAR_NAME":  "a$HOMEz",
		"DOLLAR_BARE":  "cost: $5",
		"DOUBLE_DLR":   "pa$$word$$",
		"TRAIL_DLR":    "ends with $",
		"BSLASH_DLR":   `re: \$cost`,
		"BACKSLASH":    `C:\path\d+`,
		"EQUALS_URL":   "postgres://u:p@host:5432/db?sslmode=require&a=b",
		"SPACES_QUOTE": `it has spaces and "quotes"`,
		"APOSTROPHE":   "it's a $ecret with \"quotes\" inside",
		"LITERAL_NL":   `line1\nline2 isn't a newline`,
		"REAL_NL":      "line1\nline2",
		"PEM_LIKE":     "-----BEGIN KEY-----\nab$c/d+e=\n-----END KEY-----",
	}

	mgr, err := NewManager("production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	for key, value := range cases {
		if err := mgr.Set(key, value, "production"); err != nil {
			t.Fatalf("Set %s returned error: %v", key, err)
		}
	}

	reloaded, err := NewManager("production")
	if err != nil {
		t.Fatalf("reloading manager returned error: %v", err)
	}
	for key, want := range cases {
		got, err := reloaded.Get(key)
		if err != nil {
			t.Fatalf("Get %s returned error: %v", key, err)
		}
		if got != want {
			t.Fatalf("secret %s mangled through Set/Get: wrote %q, read %q", key, want, got)
		}
	}
}

// TestManagerSetRejectsUnrepresentableValues pins that the two value shapes
// godotenv quoting cannot round-trip fail loudly at Set time instead of
// being silently corrupted at read time.
func TestManagerSetRejectsUnrepresentableValues(t *testing.T) {
	withTempWorkingDir(t)

	mgr, err := NewManager("production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	for name, value := range map[string]string{
		"trailing backslash":               `spaced \`,
		"single quote plus trailing quote": `it's "quoted"`,
	} {
		if err := mgr.Set("BAD", value, "production"); err == nil || !strings.Contains(err.Error(), "cannot be stored") {
			t.Fatalf("%s: Set should reject unrepresentable value, got %v", name, err)
		}
	}
}

func TestManagerDeletePreservesOtherEncryptedSecrets(t *testing.T) {
	withTempWorkingDir(t)

	mgr, err := NewManager("production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if err := mgr.Set("KEEP_SECRET", "keep", "production"); err != nil {
		t.Fatalf("Set KEEP_SECRET returned error: %v", err)
	}
	if err := mgr.Set("DELETE_SECRET", "delete", "production"); err != nil {
		t.Fatalf("Set DELETE_SECRET returned error: %v", err)
	}
	if err := mgr.Delete("DELETE_SECRET", "production"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	reloaded, err := NewManager("production")
	if err != nil {
		t.Fatalf("reloading manager returned error: %v", err)
	}
	if got, err := reloaded.Get("KEEP_SECRET"); err != nil || got != "keep" {
		t.Fatalf("KEEP_SECRET = %q, %v; want keep", got, err)
	}
	if _, err := reloaded.Get("DELETE_SECRET"); err == nil {
		t.Fatal("DELETE_SECRET should be removed")
	}
}

func TestManagerLoadsPlaintextPlaceholderSecretsFile(t *testing.T) {
	withTempWorkingDir(t)

	if err := os.MkdirAll(".tako", 0700); err != nil {
		t.Fatalf("failed to create .tako: %v", err)
	}
	placeholderPath := filepath.Join(".tako", "secrets.production")
	if err := os.WriteFile(placeholderPath, []byte("# placeholder from secrets init\n"), 0600); err != nil {
		t.Fatalf("failed to write placeholder: %v", err)
	}

	mgr, err := NewManager("production")
	if err != nil {
		t.Fatalf("NewManager should tolerate plaintext placeholders: %v", err)
	}
	if err := mgr.Set("PLACEHOLDER_SECRET", "value", "production"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	encrypted, err := crypto.IsFileEncrypted(placeholderPath)
	if err != nil {
		t.Fatalf("failed to inspect placeholder secrets file: %v", err)
	}
	if !encrypted {
		t.Fatal("placeholder secrets file should be encrypted after Set")
	}
}

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

func TestCreateEnvFileLoadsServiceEnvFileWithExplicitEnvAndSecretsTakingPriority(t *testing.T) {
	withTempWorkingDir(t)
	envPath := ".env.service"
	if err := os.WriteFile(envPath, []byte("DATABASE_URL=postgres://env-file\nTOKEN=env-file-token\nFROM_FILE=file-value\n"), 0600); err != nil {
		t.Fatalf("failed to write env file: %v", err)
	}
	mgr := &Manager{secrets: map[string]string{"TOKEN": "secret-token"}}

	envFile, err := mgr.CreateEnvFile(&config.ServiceConfig{
		EnvFile: envPath,
		Env: map[string]string{
			"DATABASE_URL": "postgres://explicit",
			"USES_FILE":    "${FROM_FILE}",
		},
		Secrets: []string{"TOKEN"},
	})
	if err != nil {
		t.Fatalf("CreateEnvFile returned error: %v", err)
	}

	if got, _ := envFile.Get("FROM_FILE"); got != "file-value" {
		t.Fatalf("FROM_FILE = %q, want file-value", got)
	}
	if got, _ := envFile.Get("USES_FILE"); got != "file-value" {
		t.Fatalf("USES_FILE = %q, want file-value", got)
	}
	if got, _ := envFile.Get("DATABASE_URL"); got != "postgres://explicit" {
		t.Fatalf("DATABASE_URL = %q, want explicit env override", got)
	}
	if got, _ := envFile.Get("TOKEN"); got != "secret-token" {
		t.Fatalf("TOKEN = %q, want secret override", got)
	}
}

func TestCreateEnvFileSupportsSecretAliases(t *testing.T) {
	withTempWorkingDir(t)
	mgr := &Manager{secrets: map[string]string{"INTERNAL_TOKEN": "secret-token"}}

	envFile, err := mgr.CreateEnvFile(&config.ServiceConfig{
		Secrets: []string{"API_TOKEN:INTERNAL_TOKEN"},
	})
	if err != nil {
		t.Fatalf("CreateEnvFile returned error: %v", err)
	}
	if got, _ := envFile.Get("API_TOKEN"); got != "secret-token" {
		t.Fatalf("API_TOKEN = %q, want aliased secret value", got)
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

func TestExecuteCommandTimesOutAllowedCommands(t *testing.T) {
	t.Setenv("TAKO_ALLOW_SECRET_COMMANDS", "1")
	restore := useFakeSecretCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_SECRET_COMMAND_SLEEP", "1")
	oldTimeout := secretCommandTimeout
	secretCommandTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		secretCommandTimeout = oldTimeout
	})
	mgr := &Manager{}

	_, err := mgr.executeCommand("tako token")
	if err == nil {
		t.Fatal("executeCommand should return a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout context", err)
	}
}

func useFakeSecretCommand(t *testing.T) func() {
	t.Helper()
	oldCommand := secretCommandContext
	secretCommandContext = fakeSecretCommandContext
	t.Setenv("GO_WANT_TAKO_SECRET_HELPER", "1")
	return func() {
		secretCommandContext = oldCommand
	}
}

func fakeSecretCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestSecretCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestSecretCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKO_SECRET_HELPER") != "1" {
		return
	}
	if os.Getenv("TAKO_FAKE_SECRET_COMMAND_SLEEP") == "1" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("secret\n")
	os.Exit(0)
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
