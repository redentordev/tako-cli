package takod

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func validRegistryAuthFixture() RegistryAuth {
	return RegistryAuth{Registry: "ghcr.io", Username: "octocat", Password: "s3cr3t-token"}
}

func TestValidateRegistryAuths(t *testing.T) {
	if err := validateRegistryAuths(nil); err != nil {
		t.Fatalf("nil auths rejected: %v", err)
	}
	if err := validateRegistryAuths([]RegistryAuth{validRegistryAuthFixture()}); err != nil {
		t.Fatalf("valid auth rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RegistryAuth)
	}{
		{"empty registry", func(a *RegistryAuth) { a.Registry = " " }},
		{"registry with space", func(a *RegistryAuth) { a.Registry = "ghcr io" }},
		{"registry control chars", func(a *RegistryAuth) { a.Registry = "ghcr\x00.io" }},
		{"missing username", func(a *RegistryAuth) { a.Username = "" }},
		{"missing password", func(a *RegistryAuth) { a.Password = "" }},
		{"password control chars", func(a *RegistryAuth) { a.Password = "a\x00b" }},
	}
	for _, tc := range cases {
		auth := validRegistryAuthFixture()
		tc.mutate(&auth)
		if err := validateRegistryAuths([]RegistryAuth{auth}); err == nil {
			t.Fatalf("%s: auth accepted", tc.name)
		}
	}
}

func TestNormalizeRegistryAuthKey(t *testing.T) {
	for input, want := range map[string]string{
		"ghcr.io":                             "ghcr.io",
		"docker.io":                           "https://index.docker.io/v1/",
		"index.docker.io":                     "https://index.docker.io/v1/",
		"registry-1.docker.io":                "https://index.docker.io/v1/",
		"123.dkr.ecr.us-east-1.amazonaws.com": "123.dkr.ecr.us-east-1.amazonaws.com",
	} {
		if got := normalizeRegistryAuthKey(input); got != want {
			t.Fatalf("normalizeRegistryAuthKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWriteEphemeralDockerConfig(t *testing.T) {
	dir, cleanup, err := writeEphemeralDockerConfig([]RegistryAuth{validRegistryAuthFixture()})
	if err != nil {
		t.Fatalf("writeEphemeralDockerConfig: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config.json missing: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("config.json mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	entry, ok := doc.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("auths = %v, want ghcr.io entry", doc.Auths)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil || string(decoded) != "octocat:s3cr3t-token" {
		t.Fatalf("auth = %q (decoded %q), want user:pass", entry.Auth, decoded)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral config dir survived cleanup: %v", err)
	}
}

func TestDockerAuthEnvSetsDockerConfig(t *testing.T) {
	env := dockerAuthEnv("/tmp/auth-dir")
	found := false
	for _, entry := range env {
		if entry == "DOCKER_CONFIG=/tmp/auth-dir" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DOCKER_CONFIG not set in env")
	}
}

func TestIsDockerAuthFailure(t *testing.T) {
	authFailures := []string{
		"Error response from daemon: unauthorized: authentication required",
		"pull access denied for ghcr.io/acme/app, repository does not exist or may require 'docker login'",
		"denied: requested access to the resource is denied",
		"error getting credentials: no basic auth credentials",
	}
	for _, output := range authFailures {
		if !isDockerAuthFailure(output) {
			t.Fatalf("not classified as auth failure: %q", output)
		}
		if !strings.HasPrefix(annotateRegistryAuthFailure(output), RegistryAuthFailedMarker) {
			t.Fatalf("marker missing for %q", output)
		}
	}
	other := []string{
		"manifest for ghcr.io/acme/app:v1 not found: manifest unknown",
		"failed to compute cache key: failed to walk",
	}
	for _, output := range other {
		if isDockerAuthFailure(output) {
			t.Fatalf("misclassified as auth failure: %q", output)
		}
		if annotateRegistryAuthFailure(output) != output {
			t.Fatalf("non-auth output altered: %q", output)
		}
	}
}

func TestValidateReconcileRequestRejectsBadRegistryAuth(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:       "demo",
		Environment:   "production",
		Service:       "web",
		Image:         "ghcr.io/acme/web:v1",
		Network:       "tako_demo_production",
		Containers:    []ContainerSpec{{Name: "tako_demo_production_web_1"}},
		RegistryAuths: []RegistryAuth{{Registry: "ghcr.io"}},
	}
	if err := validateReconcileServiceRequest(req); err == nil {
		t.Fatal("reconcile request with credential-less auth accepted")
	}
}
