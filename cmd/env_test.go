package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

func TestSupportedEnvBundleFilesAllowsConfiguredEnvFiles(t *testing.T) {
	allowed, paths, skipped := supportedEnvBundleFiles(
		map[string]string{
			".env.production": "prod-env",
			".env.local":      "local-env",
		},
		map[string]bool{".env.production": true},
	)

	wantPaths := []string{".env.production"}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	if allowed[".env.production"] != "prod-env" {
		t.Fatalf("allowed bundle = %#v, want configured env file", allowed)
	}
	if !slices.Equal(skipped, []string{".env.local"}) {
		t.Fatalf("skipped = %v, want .env.local", skipped)
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

func TestRestoreEnvBundleFilesRollsBackOnWriteFailure(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.WriteFile(".env", []byte("TOKEN=old\n"), 0644); err != nil {
		t.Fatalf("failed to seed .env: %v", err)
	}

	original := writeEnvBundleFileAtomic
	writeEnvBundleFileAtomic = func(path string, data []byte, perm os.FileMode) error {
		if path == ".tako/secrets.production" {
			return fmt.Errorf("disk full")
		}
		return original(path, data, perm)
	}
	t.Cleanup(func() {
		writeEnvBundleFileAtomic = original
	})

	restored, err := restoreEnvBundleFiles(map[string]string{
		".env":                     base64.StdEncoding.EncodeToString([]byte("TOKEN=new\n")),
		".tako/secrets.production": base64.StdEncoding.EncodeToString([]byte("API_KEY=value\n")),
	}, []string{".env", ".tako/secrets.production"})
	if err == nil {
		t.Fatal("restoreEnvBundleFiles should fail when a write fails")
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 after rollback", restored)
	}

	envData, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("failed to read rolled back .env: %v", err)
	}
	if string(envData) != "TOKEN=old\n" {
		t.Fatalf(".env = %q, want original content", envData)
	}
	if info, err := os.Stat(".env"); err != nil {
		t.Fatalf("failed to stat rolled back .env: %v", err)
	} else if info.Mode().Perm() != 0644 {
		t.Fatalf(".env mode = %04o, want original 0644", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(".tako", "secrets.production")); !os.IsNotExist(err) {
		t.Fatalf("secrets file should not remain after rollback, stat err=%v", err)
	}
}

func TestRestoreEnvBundleFilesRejectsSymlinkTarget(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.WriteFile("outside.env", []byte("outside\n"), 0600); err != nil {
		t.Fatalf("failed to seed outside file: %v", err)
	}
	if err := os.Symlink("outside.env", ".env"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	restored, err := restoreEnvBundleFiles(map[string]string{
		".env": base64.StdEncoding.EncodeToString([]byte("TOKEN=secret\n")),
	}, []string{".env"})
	if err == nil {
		t.Fatal("restoreEnvBundleFiles should reject symlink targets")
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}

	outside, err := os.ReadFile("outside.env")
	if err != nil {
		t.Fatalf("failed to read outside file: %v", err)
	}
	if string(outside) != "outside\n" {
		t.Fatalf("outside file = %q, want unchanged", outside)
	}
	if info, err := os.Lstat(".env"); err != nil {
		t.Fatalf("failed to lstat .env: %v", err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal(".env symlink should remain unchanged")
	}
}

func TestRestoreEnvBundleFilesRejectsSymlinkDirectory(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.Mkdir("outside", 0700); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}
	if err := os.Symlink("outside", ".tako"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	restored, err := restoreEnvBundleFiles(map[string]string{
		".tako/secrets.production": base64.StdEncoding.EncodeToString([]byte("API_KEY=value\n")),
	}, []string{".tako/secrets.production"})
	if err == nil {
		t.Fatal("restoreEnvBundleFiles should reject symlink directories")
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if _, err := os.Stat(filepath.Join("outside", "secrets.production")); !os.IsNotExist(err) {
		t.Fatalf("outside secrets file should not be written, stat err=%v", err)
	}
}

func TestRestoreDownloadedEnvBundleDecryptsAndWritesFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	passphrase := "correct horse battery staple"
	t.Setenv(envPassphraseVar, passphrase)

	bundleJSON, err := json.Marshal(map[string]string{
		".env": base64.StdEncoding.EncodeToString([]byte("TOKEN=secret\n")),
	})
	if err != nil {
		t.Fatalf("failed to marshal bundle: %v", err)
	}
	encrypted, err := crypto.EncryptWithPassphrase(bundleJSON, passphrase)
	if err != nil {
		t.Fatalf("EncryptWithPassphrase returned error: %v", err)
	}

	restored, skipped, err := restoreDownloadedEnvBundle(&takod.EnvBundleResponse{
		Found:   true,
		Content: base64.StdEncoding.EncodeToString(encrypted),
	}, false)
	if err != nil {
		t.Fatalf("restoreDownloadedEnvBundle returned error: %v", err)
	}
	if skipped {
		t.Fatal("restoreDownloadedEnvBundle skipped unexpectedly")
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}

	envData, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("failed to read restored .env: %v", err)
	}
	if string(envData) != "TOKEN=secret\n" {
		t.Fatalf(".env = %q", envData)
	}
}

func TestRestoreDownloadedEnvBundleAllowsConfiguredEnvFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	passphrase := "correct horse battery staple"
	t.Setenv(envPassphraseVar, passphrase)

	bundleJSON, err := json.Marshal(map[string]string{
		".env.production": base64.StdEncoding.EncodeToString([]byte("TOKEN=prod\n")),
	})
	if err != nil {
		t.Fatalf("failed to marshal bundle: %v", err)
	}
	encrypted, err := crypto.EncryptWithPassphrase(bundleJSON, passphrase)
	if err != nil {
		t.Fatalf("EncryptWithPassphrase returned error: %v", err)
	}

	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {EnvFile: ".env.production"},
				},
			},
		},
	}
	restored, skipped, err := restoreDownloadedEnvBundle(&takod.EnvBundleResponse{
		Found:   true,
		Content: base64.StdEncoding.EncodeToString(encrypted),
	}, false, cfg)
	if err != nil {
		t.Fatalf("restoreDownloadedEnvBundle returned error: %v", err)
	}
	if skipped {
		t.Fatal("restoreDownloadedEnvBundle skipped unexpectedly")
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}

	data, err := os.ReadFile(".env.production")
	if err != nil {
		t.Fatalf("failed to read restored .env.production: %v", err)
	}
	if string(data) != "TOKEN=prod\n" {
		t.Fatalf(".env.production = %q", data)
	}
}

func TestRunEnvPushAcquiresAndReleasesRemoteLeases(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	configPath := filepath.Join(tempDir, "tako.yaml")
	if err := os.WriteFile(configPath, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: ${TEST_SSH_PASSWORD}
  node-b:
    host: 10.0.0.2
    user: deploy
    password: ${TEST_SSH_PASSWORD}
environments:
  production:
    servers: [node-a, node-b]
    services:
      web:
        image: nginx:alpine
`), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	if err := os.WriteFile(".env", []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}
	t.Setenv("TEST_SSH_PASSWORD", "secret")
	t.Setenv(envPassphraseVar, "correct horse battery staple")

	oldCfgFile := cfgFile
	oldEnvFlag := envFlag
	oldVerbose := verbose
	cfgFile = configPath
	envFlag = "production"
	verbose = false
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		envFlag = oldEnvFlag
		verbose = oldVerbose
	})

	manager := &recordingLeaseManager{}
	var leasedOperation string
	var leasedEnvironment string
	var leasedServers []string
	leaseAcquired := false
	originalAcquire := acquireRemoteOperationLeasesFunc
	acquireRemoteOperationLeasesFunc = func(pool *ssh.Pool, _ *config.Config, envName string, serverNames []string, operation string) (*remoteOperationLeaseSet, error) {
		if pool == nil {
			return nil, fmt.Errorf("missing ssh pool")
		}
		leasedOperation = operation
		leasedEnvironment = envName
		leasedServers = append([]string(nil), serverNames...)
		leaseAcquired = true
		return &remoteOperationLeaseSet{
			operation: operation,
			leases: []remoteOperationLease{
				{serverName: "node-a", manager: manager, lease: &remotestate.LeaseInfo{ID: "lease-node-a", Environment: envName}},
				{serverName: "node-b", manager: manager, lease: &remotestate.LeaseInfo{ID: "lease-node-b", Environment: envName}},
			},
		}, nil
	}
	t.Cleanup(func() {
		acquireRemoteOperationLeasesFunc = originalAcquire
	})

	originalUpload := uploadEnvBundleToServerFunc
	var uploaded []string
	var uploadedMu sync.Mutex
	uploadEnvBundleToServerFunc = func(pool *ssh.Pool, _ *config.Config, serverName string, _ config.ServerConfig, request takod.EnvBundleRequest) error {
		if pool == nil {
			return fmt.Errorf("missing ssh pool")
		}
		if !leaseAcquired {
			return fmt.Errorf("upload started before lease acquisition")
		}
		if request.Project != "demo" || request.Environment != "production" || strings.TrimSpace(request.Content) == "" {
			return fmt.Errorf("unexpected request: %#v", request)
		}
		uploadedMu.Lock()
		uploaded = append(uploaded, serverName)
		uploadedMu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		uploadEnvBundleToServerFunc = originalUpload
	})

	if err := runEnvPush(nil, nil); err != nil {
		t.Fatalf("runEnvPush returned error: %v", err)
	}

	if leasedOperation != "env-push" {
		t.Fatalf("lease operation = %q, want env-push", leasedOperation)
	}
	if leasedEnvironment != "production" {
		t.Fatalf("lease environment = %q, want production", leasedEnvironment)
	}
	if !slices.Equal(leasedServers, []string{"node-a", "node-b"}) {
		t.Fatalf("leased servers = %#v, want node-a,node-b", leasedServers)
	}

	uploadedMu.Lock()
	slices.Sort(uploaded)
	gotUploaded := append([]string(nil), uploaded...)
	uploadedMu.Unlock()
	if !slices.Equal(gotUploaded, []string{"node-a", "node-b"}) {
		t.Fatalf("uploaded servers = %#v, want node-a,node-b", gotUploaded)
	}

	released := manager.Released()
	if got, want := strings.Join(released, ","), "lease-node-b,lease-node-a"; got != want {
		t.Fatalf("released leases = %q, want %q", got, want)
	}
}

func TestDownloadEnvBundleFromMeshUsesFirstFoundBundle(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	updatedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	calls := []string{}
	var callsMu sync.Mutex
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		callsMu.Lock()
		calls = append(calls, serverName)
		callsMu.Unlock()
		switch serverName {
		case "node-a":
			return nil, fmt.Errorf("unreachable")
		case "node-b":
			return &takod.EnvBundleResponse{Found: false}, nil
		default:
			return &takod.EnvBundleResponse{Found: true, Content: "bundle", UpdatedAt: updatedAt}, nil
		}
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	response, source, err := downloadEnvBundleFromMesh(nil, cfg, "production")
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
	callsMu.Lock()
	gotCalls := slices.Clone(calls)
	callsMu.Unlock()
	slices.Sort(gotCalls)
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", gotCalls, wantCalls)
	}
	for i := range wantCalls {
		if gotCalls[i] != wantCalls[i] {
			t.Fatalf("calls = %v, want %v", gotCalls, wantCalls)
		}
	}
}

func TestDownloadEnvBundleFromMeshUsesNewestBundle(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		switch serverName {
		case "node-a":
			return &takod.EnvBundleResponse{Found: false}, nil
		case "node-b":
			return &takod.EnvBundleResponse{Found: true, Content: "old", UpdatedAt: older}, nil
		default:
			return &takod.EnvBundleResponse{Found: true, Content: "new", UpdatedAt: newer}, nil
		}
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	response, source, err := downloadEnvBundleFromMesh(nil, cfg, "production")
	if err != nil {
		t.Fatalf("downloadEnvBundleFromMesh returned error: %v", err)
	}
	if source != "node-c" {
		t.Fatalf("source = %q, want node-c", source)
	}
	if response == nil || response.Content != "new" {
		t.Fatalf("response = %#v, want newest bundle", response)
	}
}

func TestSelectFreshestEnvBundleCandidateFallsBackToServerOrderForEqualTimestamps(t *testing.T) {
	updatedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	candidates := []envBundleDownloadCandidate{
		{response: &takod.EnvBundleResponse{Found: true, Content: "first", UpdatedAt: updatedAt}, source: "node-a", index: 0},
		{response: &takod.EnvBundleResponse{Found: true, Content: "second", UpdatedAt: updatedAt}, source: "node-b", index: 1},
	}

	selected := selectFreshestEnvBundleCandidate(candidates)
	if selected.source != "node-a" || selected.response.Content != "first" {
		t.Fatalf("selected = %#v, want first server for equal timestamp", selected)
	}
}

func TestDownloadEnvBundleFromMeshRejectsMissingUpdatedAtMetadata(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, _ string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		return &takod.EnvBundleResponse{Found: true, Content: "bundle"}, nil
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	_, _, err := downloadEnvBundleFromMesh(nil, cfg, "production")
	if err == nil {
		t.Fatal("downloadEnvBundleFromMesh should reject bundles without metadata")
	}
	if !strings.Contains(err.Error(), "missing updatedAt metadata") {
		t.Fatalf("error = %q, want missing metadata context", err)
	}
}

func TestDownloadEnvBundleFromMeshReturnsNotFoundWhenReachableNodesAreEmpty(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, _ string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		return &takod.EnvBundleResponse{Found: false}, nil
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	response, source, err := downloadEnvBundleFromMesh(nil, cfg, "production")
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
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		return nil, fmt.Errorf("%s down", serverName)
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	if _, _, err := downloadEnvBundleFromMesh(nil, cfg, "production"); err == nil {
		t.Fatal("downloadEnvBundleFromMesh should fail when all nodes fail")
	}
}

func TestDownloadEnvBundleFromMeshRunsConcurrently(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	serverNames := []string{"node-a", "node-b", "node-c"}
	updatedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	original := downloadEnvBundleFromServerFunc
	downloadEnvBundleFromServerFunc = func(_ *ssh.Pool, _ *config.Config, _ string, serverName string, _ config.ServerConfig) (*takod.EnvBundleResponse, error) {
		started <- serverName
		<-release
		if serverName == "node-b" {
			return &takod.EnvBundleResponse{Found: true, Content: "bundle", UpdatedAt: updatedAt}, nil
		}
		return &takod.EnvBundleResponse{Found: false}, nil
	}
	t.Cleanup(func() {
		downloadEnvBundleFromServerFunc = original
	})

	type result struct {
		response *takod.EnvBundleResponse
		source   string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, source, err := downloadEnvBundleFromMesh(nil, cfg, "production")
		done <- result{response: response, source: source, err: err}
	}()

	waitForEnvMeshStarts(t, started, len(serverNames))
	close(release)

	got := <-done
	if got.err != nil {
		t.Fatalf("downloadEnvBundleFromMesh returned error: %v", got.err)
	}
	if got.source != "node-b" {
		t.Fatalf("source = %q, want node-b", got.source)
	}
	if got.response == nil || !got.response.Found || got.response.Content != "bundle" {
		t.Fatalf("response = %#v, want node-b bundle", got.response)
	}
}

func TestUploadEnvBundleToMeshRunsConcurrently(t *testing.T) {
	cfg := envBundleMeshTestConfig()
	serverNames := []string{"node-a", "node-b", "node-c"}
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	original := uploadEnvBundleToServerFunc
	uploadEnvBundleToServerFunc = func(_ *ssh.Pool, _ *config.Config, serverName string, _ config.ServerConfig, _ takod.EnvBundleRequest) error {
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
		uploaded, errors := uploadEnvBundleToMesh(nil, cfg, serverNames, takod.EnvBundleRequest{})
		done <- result{uploaded: uploaded, errors: errors}
	}()

	waitForEnvMeshStarts(t, started, len(serverNames))
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
	uploadEnvBundleToServerFunc = func(_ *ssh.Pool, _ *config.Config, serverName string, _ config.ServerConfig, _ takod.EnvBundleRequest) error {
		if serverName == "node-b" {
			return fmt.Errorf("down")
		}
		return nil
	}
	t.Cleanup(func() {
		uploadEnvBundleToServerFunc = original
	})

	uploaded, errors := uploadEnvBundleToMesh(nil, cfg, serverNames, takod.EnvBundleRequest{})
	if uploaded != 2 {
		t.Fatalf("uploaded = %d, want 2", uploaded)
	}
	if !slices.Equal(errors, []string{"node-b: down"}) {
		t.Fatalf("errors = %#v, want node-b error", errors)
	}
}

func waitForEnvMeshStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for env mesh fanout; saw %v", seen)
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
