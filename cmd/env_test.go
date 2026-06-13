package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestPassphraseFromEnv(t *testing.T) {
	t.Setenv(envPassphraseVar, "correct horse battery staple")

	passphrase, ok, err := passphraseFromEnv()
	if err != nil {
		t.Fatalf("passphraseFromEnv returned error: %v", err)
	}
	if !ok {
		t.Fatal("passphraseFromEnv did not report an env passphrase")
	}
	if passphrase != "correct horse battery staple" {
		t.Fatalf("passphrase = %q", passphrase)
	}
}

func TestPassphraseFromEnvRejectsWeakSecret(t *testing.T) {
	t.Setenv(envPassphraseVar, "short")

	if _, ok, err := passphraseFromEnv(); !ok || err == nil {
		t.Fatalf("passphraseFromEnv ok=%v err=%v, want env detected with validation error", ok, err)
	}
}

func TestIsAllowedEnvBundlePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: ".env", want: true},
		{path: ".tako/secrets", want: true},
		{path: ".tako/secrets.production", want: true},
		{path: ".tako/../.env", want: false},
		{path: "../.env", want: false},
		{path: "/tmp/.env", want: false},
		{path: ".tako/known_hosts", want: false},
		{path: "secrets.production", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isAllowedEnvBundlePath(tt.path); got != tt.want {
				t.Fatalf("isAllowedEnvBundlePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSupportedEnvBundleFilesFiltersAndSortsPaths(t *testing.T) {
	allowed, paths, skipped := supportedEnvBundleFiles(map[string]string{
		".tako/secrets.production": "prod",
		"../.env":                  "escape",
		".env":                     "env",
		".tako/known_hosts":        "known-hosts",
	})

	wantPaths := []string{".env", ".tako/secrets.production"}
	if len(paths) != len(wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Fatalf("paths = %v, want %v", paths, wantPaths)
		}
	}
	if allowed[".env"] != "env" || allowed[".tako/secrets.production"] != "prod" {
		t.Fatalf("allowed bundle = %#v, want allowed files only", allowed)
	}
	wantSkipped := []string{"../.env", ".tako/known_hosts"}
	if len(skipped) != len(wantSkipped) {
		t.Fatalf("skipped = %v, want %v", skipped, wantSkipped)
	}
	for i := range wantSkipped {
		if skipped[i] != wantSkipped[i] {
			t.Fatalf("skipped = %v, want %v", skipped, wantSkipped)
		}
	}
}

func TestRestoreEnvBundleFilesWritesAllowedFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	restored, err := restoreEnvBundleFiles(map[string]string{
		".env":                     base64.StdEncoding.EncodeToString([]byte("TOKEN=secret\n")),
		".tako/secrets.production": base64.StdEncoding.EncodeToString([]byte("API_KEY=value\n")),
	}, []string{".env", ".tako/secrets.production"})
	if err != nil {
		t.Fatalf("restoreEnvBundleFiles returned error: %v", err)
	}
	if restored != 2 {
		t.Fatalf("restored = %d, want 2", restored)
	}

	envData, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("failed to read restored .env: %v", err)
	}
	if string(envData) != "TOKEN=secret\n" {
		t.Fatalf(".env = %q", envData)
	}

	info, err := os.Stat(filepath.Join(".tako", "secrets.production"))
	if err != nil {
		t.Fatalf("failed to stat restored secrets file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("restored file mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestRestoreEnvBundleFilesRejectsInvalidBase64WithoutPartialWrites(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	restored, err := restoreEnvBundleFiles(map[string]string{
		".env":                     base64.StdEncoding.EncodeToString([]byte("TOKEN=secret\n")),
		".tako/secrets.production": "not-base64!",
	}, []string{".env", ".tako/secrets.production"})
	if err == nil {
		t.Fatal("restoreEnvBundleFiles should reject invalid base64")
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if !strings.Contains(err.Error(), "failed to decode") {
		t.Fatalf("error = %q, want decode context", err)
	}
	if _, statErr := os.Stat(".env"); !os.IsNotExist(statErr) {
		t.Fatalf(".env should not be written on decode failure, statErr=%v", statErr)
	}
}

func TestDownloadEnvBundleFromMeshUsesFirstFoundBundle(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	calls := []string{}
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		calls = append(calls, serverName)
		switch serverName {
		case "node-a":
			return nil, fmt.Errorf("unreachable")
		case "node-b":
			return &takod.EnvBundleResponse{Found: false}, nil
		default:
			return &takod.EnvBundleResponse{Found: true, Content: "bundle"}, nil
		}
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	response, source, err := downloadEnvBundleFromMesh(cfg, "production")
	if err != nil {
		t.Fatalf("downloadEnvBundleFromMesh returned error: %v", err)
	}
	if response == nil || !response.Found || response.Content != "bundle" {
		t.Fatalf("response = %#v, want found bundle", response)
	}
	if source != "node-c" {
		t.Fatalf("source = %q, want node-c", source)
	}
	wantCalls := []string{"node-a", "node-b", "node-c"}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	for i := range wantCalls {
		if calls[i] != wantCalls[i] {
			t.Fatalf("calls = %v, want %v", calls, wantCalls)
		}
	}
}

func TestDownloadEnvBundleFromMeshReturnsNotFoundWhenReachableNodesAreEmpty(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *config.Config, _ string, _ string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		return &takod.EnvBundleResponse{Found: false}, nil
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	response, source, err := downloadEnvBundleFromMesh(cfg, "production")
	if err != nil {
		t.Fatalf("downloadEnvBundleFromMesh returned error: %v", err)
	}
	if response == nil || response.Found {
		t.Fatalf("response = %#v, want not found", response)
	}
	if source != "" {
		t.Fatalf("source = %q, want empty source", source)
	}
}

func TestDownloadEnvBundleFromMeshErrorsWhenAllNodesFail(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		return nil, fmt.Errorf("%s down", serverName)
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	if _, _, err := downloadEnvBundleFromMesh(cfg, "production"); err == nil {
		t.Fatal("downloadEnvBundleFromMesh should fail when all nodes fail")
	}
}

func TestUploadEnvBundleToMeshRunsConcurrently(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	serverNames := []string{"node-a", "node-b", "node-c"}
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	original := uploadEnvBundleToServerFunc
	uploadEnvBundleToServerFunc = func(_ *config.Config, serverName string, _ config.ServerConfig, _ takod.EnvBundleRequest) error {
		started <- serverName
		<-release
		return nil
	}
	t.Cleanup(func() {
		uploadEnvBundleToServerFunc = original
	})

	type result struct {
		uploaded int
		errors   []string
	}
	done := make(chan result, 1)
	go func() {
		uploaded, errors := uploadEnvBundleToMesh(cfg, serverNames, takod.EnvBundleRequest{})
		done <- result{uploaded: uploaded, errors: errors}
	}()

	waitForEnvUploadStarts(t, started, len(serverNames))
	close(release)

	got := <-done
	if got.uploaded != len(serverNames) || len(got.errors) != 0 {
		t.Fatalf("upload result = %#v, want all nodes uploaded", got)
	}
}

func TestUploadEnvBundleToMeshReportsErrorsInServerOrder(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	serverNames := []string{"node-a", "node-b", "node-c"}
	original := uploadEnvBundleToServerFunc
	uploadEnvBundleToServerFunc = func(_ *config.Config, serverName string, _ config.ServerConfig, _ takod.EnvBundleRequest) error {
		if serverName == "node-b" {
			return fmt.Errorf("down")
		}
		return nil
	}
	t.Cleanup(func() {
		uploadEnvBundleToServerFunc = original
	})

	uploaded, errors := uploadEnvBundleToMesh(cfg, serverNames, takod.EnvBundleRequest{})
	if uploaded != 2 {
		t.Fatalf("uploaded = %d, want 2", uploaded)
	}
	if !slices.Equal(errors, []string{"node-b: down"}) {
		t.Fatalf("errors = %#v, want node-b error", errors)
	}
}

func waitForEnvUploadStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for env upload fanout; saw %v", seen)
		}
	}
}

func envBundleMeshTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b", "node-c"},
			},
		},
	}
}
