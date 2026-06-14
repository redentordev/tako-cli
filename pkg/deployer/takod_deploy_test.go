package deployer

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestEnsureTakodMeshKeysWithRunsConcurrently(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	deploy := &Deployer{config: testTakodDeployConfig(serverNames)}
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	keysDone := make(chan map[string]string, 1)
	errDone := make(chan error, 1)
	go func() {
		keys, err := deploy.ensureTakodMeshKeysWith(serverNames, func(serverName string) (string, error) {
			started <- serverName
			<-release
			return " key-" + serverName + "\n", nil
		})
		keysDone <- keys
		errDone <- err
	}()

	waitForTakodDeployStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("ensureTakodMeshKeysWith returned error: %v", err)
	}
	keys := <-keysDone
	for _, serverName := range serverNames {
		if got, want := keys[serverName], "key-"+serverName; got != want {
			t.Fatalf("key for %s = %q, want %q", serverName, got, want)
		}
	}
}

func TestPrepareTakodNodesWithRunsConcurrentlyAndPreservesIndices(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	deploy := &Deployer{config: testTakodDeployConfig(serverNames)}
	started := make(chan string, len(serverNames))
	release := make(chan struct{})
	var mu sync.Mutex
	var calls []string

	errDone := make(chan error, 1)
	go func() {
		errDone <- deploy.prepareTakodNodesWith(serverNames, func(index int, serverName string, server config.ServerConfig) error {
			started <- serverName
			<-release
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, fmt.Sprintf("%d:%s:%s", index, serverName, server.Host))
			return nil
		})
	}()

	waitForTakodDeployStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("prepareTakodNodesWith returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	slices.Sort(calls)
	want := []string{
		"0:node-a:node-a.example.test",
		"1:node-b:node-b.example.test",
		"2:node-c:node-c.example.test",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("prepare calls = %#v, want %#v", calls, want)
	}
}

func TestRunTakodNodeActionsRunsConcurrently(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	errDone := make(chan error, 1)
	go func() {
		errDone <- runTakodNodeActions(serverNames, func(serverName string) error {
			started <- serverName
			<-release
			return nil
		})
	}()

	waitForTakodDeployStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("runTakodNodeActions returned error: %v", err)
	}
}

func TestRunTakodNodeActionsAggregatesSortedErrors(t *testing.T) {
	err := runTakodNodeActions([]string{"node-b", "node-a"}, func(serverName string) error {
		return fmt.Errorf("failed")
	})
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if got, want := err.Error(), "node-a: failed; node-b: failed"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want to contain %q", got, want)
	}
}

func TestBuildTakodHealthCommandQuotesURL(t *testing.T) {
	got := buildTakodHealthCommand(8080, "/health; touch /tmp/pwned")
	want := "curl -sf -- 'http://127.0.0.1:8080/health; touch /tmp/pwned' || exit 1"
	if got != want {
		t.Fatalf("health command = %q, want %q", got, want)
	}
}

func TestBuildTakodHealthSpecUsesQuotedCommand(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		HealthCheck: config.HealthCheckConfig{
			Path: "/ready?token=a'b",
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}

	want := "curl -sf -- 'http://127.0.0.1:8080/ready?token=a'\"'\"'b' || exit 1"
	if spec.Command != want {
		t.Fatalf("health command = %q, want %q", spec.Command, want)
	}
}

func TestServiceRuntimeLabelsIncludeSafeConfigHash(t *testing.T) {
	service := config.ServiceConfig{
		Image: "nginx:1.27",
		Port:  8080,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
	}
	wantHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	labels := serviceRuntimeLabels(service)
	if labels[reconcile.ConfigHashLabel] != wantHash {
		t.Fatalf("config hash label = %q, want %q", labels[reconcile.ConfigHashLabel], wantHash)
	}
}

func TestServiceRuntimeLabelsSkipEnvMaterial(t *testing.T) {
	labels := serviceRuntimeLabels(config.ServiceConfig{
		Image: "nginx:1.27",
		Env:   map[string]string{"TOKEN": "secret"},
	})
	if labels != nil {
		t.Fatalf("labels = %#v, want nil", labels)
	}
}

func waitForTakodDeployStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for takod setup fanout; saw %v", seen)
		}
	}
}

func testTakodDeployConfig(serverNames []string) *config.Config {
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		servers[serverName] = config.ServerConfig{
			Host: serverName + ".example.test",
			User: "root",
		}
	}
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Mesh: &config.MeshConfig{
			Enabled:      testBoolPointer(true),
			NetworkCIDR:  "10.210.0.0/16",
			Interface:    "tako",
			ListenPort:   51820,
			SubnetBits:   24,
			NATTraversal: true,
		},
		Servers: servers,
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: serverNames,
			},
		},
	}
}

func testBoolPointer(value bool) *bool {
	return &value
}
