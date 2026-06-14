package deployer

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
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

func TestShouldInstallTakodRelease(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		statusJSON string
		statusErr  error
		want       bool
	}{
		{
			name:       "matching release",
			version:    "v1.2.3",
			statusJSON: `{"version":"v1.2.3"}`,
			want:       false,
		},
		{
			name:       "different release",
			version:    "v1.2.4",
			statusJSON: `{"version":"v1.2.3"}`,
			want:       true,
		},
		{
			name:      "missing agent",
			version:   "v1.2.4",
			statusErr: fmt.Errorf("takod unavailable"),
			want:      true,
		},
		{
			name:    "dev build does not download release",
			version: "dev",
			want:    false,
		},
		{
			name:    "empty version does not download release",
			version: "",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := &Deployer{cliVersion: tt.version, config: &config.Config{}}
			got := deploy.shouldInstallTakodRelease(fakeTakodStatusExecutor{
				output: tt.statusJSON,
				err:    tt.statusErr,
			})
			if got != tt.want {
				t.Fatalf("shouldInstallTakodRelease() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildTakodHealthCommandQuotesURL(t *testing.T) {
	got := buildTakodHealthCommand(8080, "/health; touch /tmp/pwned")
	want := "curl -sf -- 'http://127.0.0.1:8080/health; touch /tmp/pwned' || exit 1"
	if got != want {
		t.Fatalf("health command = %q, want %q", got, want)
	}
}

type fakeTakodStatusExecutor struct {
	output string
	err    error
}

func (f fakeTakodStatusExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	return f.output, f.err
}

func (f fakeTakodStatusExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	return f.output, f.err
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

func TestShouldPublishMeshUpstreamsOnlyForMultiNodeEnvironments(t *testing.T) {
	oneNode := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	got, err := oneNode.shouldPublishMeshUpstreams()
	if err != nil {
		t.Fatalf("one-node shouldPublishMeshUpstreams returned error: %v", err)
	}
	if got {
		t.Fatal("one-node environments should not publish mesh upstream host ports")
	}

	twoNodes := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	got, err = twoNodes.shouldPublishMeshUpstreams()
	if err != nil {
		t.Fatalf("two-node shouldPublishMeshUpstreams returned error: %v", err)
	}
	if !got {
		t.Fatal("multi-node environments should publish mesh upstream host ports")
	}
}

func TestBuildTakodContainerSpecDoesNotPublishPublicOneNodeService(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec("node-a", "web", &config.ServiceConfig{
		Port:  80,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
	}, 1, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 0 {
		t.Fatalf("public one-node service should not publish direct host ports: %#v", container.Publishes)
	}
}

func TestBuildTakodContainerSpecPublishesPublicMultiNodeServiceOnMeshIP(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	env := deploy.config.Environments["production"]
	env.Services = map[string]config.ServiceConfig{
		"web": {
			Port:  80,
			Proxy: &config.ProxyConfig{Domain: "example.com"},
		},
	}
	deploy.config.Environments["production"] = env

	container, err := deploy.buildTakodContainerSpec("node-a", "web", &config.ServiceConfig{
		Port:  80,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
	}, 1, 1, true)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 1 {
		t.Fatalf("public multi-node service should publish one mesh port: %#v", container.Publishes)
	}
	if !strings.HasPrefix(container.Publishes[0], "10.210.0.1:") || !strings.HasSuffix(container.Publishes[0], ":80") {
		t.Fatalf("public multi-node publish should bind the mesh IP and container port: %#v", container.Publishes)
	}
}

func TestBuildTakodContainerSpecPublishesInternalDirectPort(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec("node-a", "metrics", &config.ServiceConfig{Port: 9100}, 1, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if !slices.Equal(container.Publishes, []string{"9100:9100"}) {
		t.Fatalf("internal direct publish = %#v, want 9100:9100", container.Publishes)
	}
}

func TestBuildTakodContainerSpecRejectsReplicatedInternalDirectPort(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	_, err := deploy.buildTakodContainerSpec("node-a", "metrics", &config.ServiceConfig{Port: 9100}, 1, 2, false)
	if err == nil {
		t.Fatal("expected replicated direct host port to be rejected")
	}
}

func TestPlanTakodAssignmentsHonorsPlacementConstraints(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"node-a", "node-b", "node-c"})
	cfg.Servers["node-a"] = config.ServerConfig{Host: "node-a.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	cfg.Servers["node-b"] = config.ServerConfig{Host: "node-b.example.test", User: "root", Labels: map[string]string{"role": "worker"}}
	cfg.Servers["node-c"] = config.ServerConfig{Host: "node-c.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	deploy := &Deployer{config: cfg, environment: "production"}

	assignments, err := deploy.planTakodAssignments(&config.ServiceConfig{
		Replicas: 3,
		Placement: &config.PlacementConfig{
			Strategy:    "spread",
			Constraints: []string{"node.labels.role==web"},
		},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}

	got := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		got = append(got, assignment.ServerName)
	}
	want := []string{"node-a", "node-c", "node-a"}
	if !slices.Equal(got, want) {
		t.Fatalf("assignment servers = %#v, want %#v", got, want)
	}
}

func TestPlanTakodAssignmentsGlobalUsesConstrainedTargets(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"node-a", "node-b", "node-c"})
	cfg.Servers["node-a"] = config.ServerConfig{Host: "node-a.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	cfg.Servers["node-b"] = config.ServerConfig{Host: "node-b.example.test", User: "root", Labels: map[string]string{"role": "worker"}}
	cfg.Servers["node-c"] = config.ServerConfig{Host: "node-c.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	deploy := &Deployer{config: cfg, environment: "production"}

	assignments, err := deploy.planTakodAssignments(&config.ServiceConfig{
		Placement: &config.PlacementConfig{
			Strategy:    "global",
			Constraints: []string{"node.labels.role==web"},
		},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}
	if len(assignments) != 2 {
		t.Fatalf("assignments = %#v, want two constrained global assignments", assignments)
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

	labels := serviceRuntimeLabels("demo", "production", "web", service)
	if labels[reconcile.ConfigHashLabel] != wantHash {
		t.Fatalf("config hash label = %q, want %q", labels[reconcile.ConfigHashLabel], wantHash)
	}
	if labels[runtimeid.ServiceIdentityLabel] != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("runtime identity label = %q, want runtime identity", labels[runtimeid.ServiceIdentityLabel])
	}
}

func TestServiceRuntimeLabelsKeepRuntimeIdentityForEnvMaterial(t *testing.T) {
	labels := serviceRuntimeLabels("demo", "production", "web", config.ServiceConfig{
		Image: "nginx:1.27",
		Env:   map[string]string{"TOKEN": "secret"},
	})
	if _, ok := labels[reconcile.ConfigHashLabel]; ok {
		t.Fatalf("config hash label should be omitted for env material: %#v", labels)
	}
	if labels[runtimeid.ServiceIdentityLabel] != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("runtime identity label = %q, want runtime identity", labels[runtimeid.ServiceIdentityLabel])
	}
}

func TestTakodRuntimeNamesUseCollisionResistantAppStageIdentity(t *testing.T) {
	left := &Deployer{
		config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		environment: "prod_api",
	}
	right := &Deployer{
		config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		environment: "prod",
	}

	leftContainer := left.takodContainerName("web", 1)
	rightContainer := right.takodContainerName("api_web", 1)
	if leftContainer == rightContainer {
		t.Fatalf("container names collided: %q", leftContainer)
	}
	if leftContainer != runtimeid.ContainerName("demo", "prod_api", "web", 1) {
		t.Fatalf("left container = %q, want runtimeid container", leftContainer)
	}
	if takodNetworkName("demo", "prod_api") != runtimeid.NetworkName("demo", "prod_api") {
		t.Fatal("takod network should use runtimeid network name")
	}
}

func TestBuildTakodMountSpecsNamespacesNamedVolumes(t *testing.T) {
	deploy := &Deployer{
		config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		environment: "production",
	}

	mounts, err := deploy.buildTakodMountSpecs("web", &config.ServiceConfig{
		Volumes: []string{"/data", "cache:/cache"},
	})
	if err != nil {
		t.Fatalf("buildTakodMountSpecs returned error: %v", err)
	}
	want := []string{
		"type=volume,source=" + runtimeid.VolumeName("demo", "production", "/data") + ",target=/data",
		"type=volume,source=" + runtimeid.VolumeName("demo", "production", "cache") + ",target=/cache",
	}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", mounts, want)
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
