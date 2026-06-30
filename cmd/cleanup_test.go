package cmd

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
)

type sshClientRequest struct {
	host     string
	port     int
	user     string
	sshKey   string
	password string
}

type fakeSSHClientProvider struct {
	requests []sshClientRequest
	err      error
}

func (p *fakeSSHClientProvider) GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*ssh.Client, error) {
	p.requests = append(p.requests, sshClientRequest{
		host:     host,
		port:     port,
		user:     user,
		sshKey:   keyPath,
		password: password,
	})
	if p.err != nil {
		return nil, p.err
	}
	return nil, nil
}

func TestCollectCleanupNodesRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []cleanupNodeResult, 1)
	go func() {
		resultsDone <- collectCleanupNodes(servers, func(serverName string, _ config.ServerConfig) (*takod.CleanupResponse, error) {
			started <- serverName
			<-release
			return &takod.CleanupResponse{ImagesRemoved: len(serverName)}, nil
		})
	}()

	waitForCleanupStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].serverName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, want)
		}
		if results[i].response == nil || results[i].response.ImagesRemoved == 0 {
			t.Fatalf("result %d response = %#v", i, results[i].response)
		}
	}
}

func TestCollectCleanupNodesRecordsErrors(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}

	results := collectCleanupNodes(servers, func(serverName string, _ config.ServerConfig) (*takod.CleanupResponse, error) {
		if serverName == "node-a" {
			return nil, fmt.Errorf("cleanup failed")
		}
		return &takod.CleanupResponse{}, nil
	})

	if results[0].serverName != "node-a" || results[0].err == nil {
		t.Fatalf("node-a should record cleanup error: %#v", results[0])
	}
	if results[1].serverName != "node-b" || results[1].err != nil {
		t.Fatalf("node-b should succeed: %#v", results[1])
	}
}

func TestCleanupSingleNodeUsesProvidedPool(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	server := config.ServerConfig{
		Host:     "node-a.example.test",
		Port:     2222,
		User:     "deploy",
		SSHKey:   "/tmp/id_ed25519",
		Password: "fallback",
	}
	request := takod.CleanupRequest{Project: "demo", Environment: "production"}

	called := false
	response, err := cleanupSingleNodeWithExecutor(cfg, provider, server, request, func(client *ssh.Client, gotCfg *config.Config, gotRequest takod.CleanupRequest) (*takod.CleanupResponse, error) {
		called = true
		if gotCfg != cfg {
			t.Fatal("executor received different config pointer")
		}
		if gotRequest.Project != "demo" || gotRequest.Environment != "production" {
			t.Fatalf("cleanup request = %#v", gotRequest)
		}
		return &takod.CleanupResponse{ImagesRemoved: 1}, nil
	})
	if err != nil {
		t.Fatalf("cleanupSingleNodeWithExecutor returned error: %v", err)
	}
	if !called {
		t.Fatal("executor was not called")
	}
	if response == nil || response.ImagesRemoved != 1 {
		t.Fatalf("response = %#v", response)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("pool requests = %#v, want one", provider.requests)
	}
	got := provider.requests[0]
	if got.host != server.Host || got.port != server.Port || got.user != server.User || got.sshKey != server.SSHKey || got.password != server.Password {
		t.Fatalf("pool request = %#v, want server config", got)
	}
}

func TestCleanupSingleNodeReturnsPoolConnectionError(t *testing.T) {
	provider := &fakeSSHClientProvider{err: errors.New("dial failed")}
	called := false

	_, err := cleanupSingleNodeWithExecutor(&config.Config{}, provider, config.ServerConfig{Host: "node-a.example.test"}, takod.CleanupRequest{}, func(*ssh.Client, *config.Config, takod.CleanupRequest) (*takod.CleanupResponse, error) {
		called = true
		return &takod.CleanupResponse{}, nil
	})
	if err == nil {
		t.Fatal("cleanupSingleNodeWithExecutor returned nil, want connection error")
	}
	if called {
		t.Fatal("executor should not run after pool connection error")
	}
	if got := err.Error(); got != "failed to connect: dial failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestCleanupRequestForEnvironmentKeepsSharedDockerCacheOptIn(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	repositories := []string{"demo/web"}
	externalVolumes := []string{"shared-data"}

	request := cleanupRequestForEnvironment(cfg, "production", repositories, externalVolumes, 3, false, "10GB", true)
	if request.Project != "demo" || request.Environment != "production" {
		t.Fatalf("request scope = %s/%s, want demo/production", request.Project, request.Environment)
	}
	if !request.CleanOldImages || !request.CleanStoppedContainers || !request.CleanUnusedVolumes {
		t.Fatalf("request should enable project-owned cleanup: %#v", request)
	}
	if request.CleanDanglingImages || request.CleanBuildCache {
		t.Fatalf("default cleanup should not touch shared Docker cache: %#v", request)
	}
	if request.BuildCacheKeepStorage != "" {
		t.Fatalf("default cleanup should not send build cache keep storage: %#v", request)
	}
	if !request.SecureLogPermissions {
		t.Fatalf("secure flag not propagated: %#v", request)
	}

	request = cleanupRequestForEnvironment(cfg, "production", repositories, externalVolumes, 2, true, "8GB", false)
	if !request.CleanDanglingImages || !request.CleanBuildCache {
		t.Fatalf("docker cache flag should enable explicit shared Docker cache cleanup: %#v", request)
	}
	if request.BuildCacheKeepStorage != "8GB" {
		t.Fatalf("build cache keep storage = %q, want 8GB", request.BuildCacheKeepStorage)
	}
}

func waitForCleanupStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for cleanup fanout; saw %v", seen)
		}
	}
}
