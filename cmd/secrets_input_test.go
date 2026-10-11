package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/secrets"
)

const fakeSecretValue = "fake-test-value-not-a-secret"

func TestResolveSecretSetArgForms(t *testing.T) {
	dir := t.TempDir()
	valueFile := filepath.Join(dir, "value.txt")
	if err := os.WriteFile(valueFile, []byte(fakeSecretValue+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	multiLineFile := filepath.Join(dir, "multi.txt")
	if err := os.WriteFile(multiLineFile, []byte("line-one\nline-two\n\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		arg      string
		stdin    string
		useStdin bool
		wantKey  string
		want     string
	}{
		{name: "literal", arg: "API_TOKEN=" + fakeSecretValue, wantKey: "API_TOKEN", want: fakeSecretValue},
		{name: "literal with equals", arg: "DSN=a=b", wantKey: "DSN", want: "a=b"},
		{name: "empty literal", arg: "EMPTY=", wantKey: "EMPTY", want: ""},
		{name: "escaped at", arg: "HANDLE=@@team", wantKey: "HANDLE", want: "@team"},
		{name: "file", arg: "API_TOKEN=@" + valueFile, wantKey: "API_TOKEN", want: fakeSecretValue},
		{name: "file strips one newline only", arg: "CERT=@" + multiLineFile, wantKey: "CERT", want: "line-one\nline-two\n"},
		{name: "stdin", arg: "API_TOKEN", useStdin: true, stdin: fakeSecretValue + "\n", wantKey: "API_TOKEN", want: fakeSecretValue},
		{name: "stdin crlf", arg: "API_TOKEN", useStdin: true, stdin: fakeSecretValue + "\r\n", wantKey: "API_TOKEN", want: fakeSecretValue},
		{name: "stdin no newline", arg: "API_TOKEN", useStdin: true, stdin: fakeSecretValue, wantKey: "API_TOKEN", want: fakeSecretValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, value, err := resolveSecretSetArg(tc.arg, tc.useStdin, strings.NewReader(tc.stdin))
			if err != nil {
				t.Fatalf("resolveSecretSetArg returned error: %v", err)
			}
			if key != tc.wantKey || value != tc.want {
				t.Fatalf("got (%q, %q), want (%q, %q)", key, value, tc.wantKey, tc.want)
			}
		})
	}
}

func TestResolveSecretSetArgRejectsInvalidForms(t *testing.T) {
	for _, tc := range []struct {
		arg      string
		useStdin bool
	}{
		{arg: "API_TOKEN"},
		{arg: "API_TOKEN=" + fakeSecretValue, useStdin: true},
		{arg: "bad-key", useStdin: true},
		{arg: "API_TOKEN=@"},
		{arg: "API_TOKEN=@" + filepath.Join(t.TempDir(), "missing")},
	} {
		_, _, err := resolveSecretSetArg(tc.arg, tc.useStdin, strings.NewReader(fakeSecretValue))
		if err == nil {
			t.Fatalf("resolveSecretSetArg(%q, stdin=%v) should fail", tc.arg, tc.useStdin)
		}
		if strings.Contains(err.Error(), fakeSecretValue) {
			t.Fatalf("error leaked the value: %v", err)
		}
	}
}

func TestSecretsSetStdinStoresValueWithoutEchoing(t *testing.T) {
	switchToTempDir(t)
	if err := secretsSetCmd.Flags().Set("stdin", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secretsSetCmd.Flags().Set("stdin", "false")
		secretsSetCmd.SetIn(nil)
		secretsSetCmd.SetOut(nil)
	})
	var out bytes.Buffer
	secretsSetCmd.SetIn(strings.NewReader(fakeSecretValue + "\n"))
	secretsSetCmd.SetOut(&out)

	if err := secretsSetCmd.RunE(secretsSetCmd, []string{"API_TOKEN"}); err != nil {
		t.Fatalf("secrets set --stdin returned error: %v", err)
	}
	if strings.Contains(out.String(), fakeSecretValue) {
		t.Fatalf("value echoed to output: %q", out.String())
	}
	mgr, err := secrets.NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := mgr.Get("API_TOKEN"); err != nil || got != fakeSecretValue {
		t.Fatalf("stored value = %q, %v", got, err)
	}
}

func TestSecretsImportDotenvFromStdin(t *testing.T) {
	switchToTempDir(t)
	oldFormat, oldFile := secretsImportFormat, secretsImportFile
	oldWrite, oldEnv, oldMaps := secretsProviderFlags.write, secretsProviderFlags.env, secretsProviderFlags.maps
	t.Cleanup(func() {
		secretsImportFormat, secretsImportFile = oldFormat, oldFile
		secretsProviderFlags.write, secretsProviderFlags.env, secretsProviderFlags.maps = oldWrite, oldEnv, oldMaps
		secretsImportCmd.SetIn(nil)
	})
	secretsImportFormat = "env"
	secretsImportFile = ""
	secretsProviderFlags.write = true
	secretsProviderFlags.env = "production"
	secretsProviderFlags.maps = []string{"old_name=RENAMED"}

	stream := "# comment\nAPI_TOKEN=" + fakeSecretValue + "\nexport db_url='postgres://user:fake@localhost/db'\nold_name=\"quoted value\"\n"
	secretsImportCmd.SetIn(strings.NewReader(stream))

	if err := secretsImportCmd.Args(secretsImportCmd, nil); err != nil {
		t.Fatalf("--format env should accept no positional args: %v", err)
	}
	if err := secretsImportCmd.Args(secretsImportCmd, []string{"aws-ssm"}); err == nil {
		t.Fatal("--format env should reject a provider argument")
	}

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = secretsImportCmd.RunE(secretsImportCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("secrets import --format env returned error: %v", runErr)
	}
	if strings.Contains(stdout, fakeSecretValue) || strings.Contains(stdout, "quoted value") {
		t.Fatalf("value echoed to output:\n%s", stdout)
	}

	mgr, err := secrets.NewManager("production")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"API_TOKEN": fakeSecretValue,
		"db_url":    "postgres://user:fake@localhost/db",
		"RENAMED":   "quoted value",
	} {
		if got, err := mgr.Get(key); err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", key, got, err, want)
		}
	}
}

func TestReadDotenvSecretsFromFileAndRejectsBadKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prod.env")
	if err := os.WriteFile(path, []byte("API_TOKEN="+fakeSecretValue+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := readDotenvSecrets(path, strings.NewReader("IGNORED=1\n"))
	if err != nil {
		t.Fatalf("readDotenvSecrets returned error: %v", err)
	}
	if len(values) != 1 || values["API_TOKEN"] != fakeSecretValue {
		t.Fatalf("values = %v", values)
	}
	if _, err := mapDotenvSecrets(map[string]string{"bad.key": "x"}, nil); err == nil {
		t.Fatal("expected invalid dotenv key to be rejected")
	}
}
