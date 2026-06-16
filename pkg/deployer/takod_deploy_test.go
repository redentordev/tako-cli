package deployer

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/utils"
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

func TestRollingNodeOrderDeploysAssignedNodesBeforeCleanupNodes(t *testing.T) {
	got := rollingNodeOrder(
		[]string{"node-a", "node-b", "node-c"},
		[]string{"node-c", "node-b", "node-c"},
	)
	want := []string{"node-c", "node-b", "node-a"}
	if !slices.Equal(got, want) {
		t.Fatalf("rolling node order = %#v, want %#v", got, want)
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
	want := expectedTakodHealthCommand("http://127.0.0.1:8080/health; touch /tmp/pwned")
	if got != want {
		t.Fatalf("health command = %q, want %q", got, want)
	}
}

func TestBuildTakodHealthCommandFallsBackThroughWgetAndNode(t *testing.T) {
	got := buildTakodHealthCommand(8080, "/health")
	for _, expected := range []string{
		"command -v curl",
		"curl -sf -- \"$url\"",
		"command -v wget",
		"wget -q -O /dev/null -- \"$url\"",
		"command -v node",
		"fetch(process.argv[1])",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("health command missing %q: %s", expected, got)
		}
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

	want := expectedTakodHealthCommand("http://127.0.0.1:8080/ready?token=a'b")
	if spec.Command != want {
		t.Fatalf("health command = %q, want %q", spec.Command, want)
	}
}

func expectedTakodHealthCommand(url string) string {
	nodeProbe := `fetch(process.argv[1]).then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))`
	script := fmt.Sprintf("url=%s; if command -v curl >/dev/null 2>&1; then curl -sf -- \"$url\" >/dev/null; elif command -v wget >/dev/null 2>&1; then wget -q -O /dev/null -- \"$url\"; elif command -v node >/dev/null 2>&1; then node -e %s \"$url\"; else echo 'curl, wget, or node is required for health checks' >&2; exit 127; fi", utils.ShellQuote(url), utils.ShellQuote(nodeProbe))
	return "sh -c " + utils.ShellQuote(script)
}

func TestBuildTakodHealthSpecWaitsLongerThanDockerRetryCount(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 80,
		HealthCheck: config.HealthCheckConfig{
			Path:        "/",
			Interval:    "30s",
			Timeout:     "5s",
			Retries:     3,
			StartPeriod: "10s",
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Retries != 3 {
		t.Fatalf("docker health retries = %d, want 3", spec.Retries)
	}
	if spec.WaitAttempts != 135 {
		t.Fatalf("deployment wait attempts = %d, want 135", spec.WaitAttempts)
	}
}

func TestDeploymentHealthWaitAttemptsUsesFloorAndCap(t *testing.T) {
	if got := deploymentHealthWaitAttempts("1s", "0s", 1); got != 30 {
		t.Fatalf("short health wait attempts = %d, want floor 30", got)
	}
	if got := deploymentHealthWaitAttempts("10m", "10m", 100); got != 600 {
		t.Fatalf("long health wait attempts = %d, want cap 600", got)
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

func TestTakodMeshServersIncludeImportServersBeforeEnvironmentServers(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"edge"})
	cfg.Servers["app"] = config.ServerConfig{Host: "app.example.test", User: "root"}
	cfg.Imports = map[string]config.ImportConfig{
		"jardin_admin": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "admin",
			Port:        "web",
			Servers:     []string{"app"},
		},
		"jardin_renderer": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "renderer",
			Port:        "web",
			Servers:     []string{"app"},
		},
	}
	deploy := &Deployer{config: cfg, environment: "production"}

	got, err := deploy.getTakodMeshServers()
	if err != nil {
		t.Fatalf("getTakodMeshServers returned error: %v", err)
	}
	want := []string{"app", "edge"}
	if !slices.Equal(got, want) {
		t.Fatalf("mesh servers = %#v, want %#v", got, want)
	}
	appAddress, err := deploy.meshAddress(0)
	if err != nil {
		t.Fatalf("meshAddress returned error: %v", err)
	}
	if appAddress != "10.210.0.1/24" {
		t.Fatalf("import producer address = %q, want exported endpoint source address", appAddress)
	}
}

func TestPlanTakodAssignmentsGlobalExpandsToAddedEnvironmentNode(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b", "node-c"}), environment: "production"}

	assignments, err := deploy.planTakodAssignments("agent", &config.ServiceConfig{
		Placement: &config.PlacementConfig{Strategy: "global"},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}
	want := []takodAssignment{
		{ServerName: "node-a", Slot: 1},
		{ServerName: "node-b", Slot: 2},
		{ServerName: "node-c", Slot: 3},
	}
	if !slices.Equal(assignments, want) {
		t.Fatalf("assignments = %#v, want %#v", assignments, want)
	}
}

func TestShouldDistributeBuiltImageWhenAssignedAwayFromSource(t *testing.T) {
	tests := []struct {
		name        string
		assignments []string
		source      string
		want        bool
	}{
		{
			name:        "only source node",
			assignments: []string{"node-a"},
			source:      "node-a",
			want:        false,
		},
		{
			name:        "source and peer",
			assignments: []string{"node-a", "node-b"},
			source:      "node-a",
			want:        true,
		},
		{
			name:        "pinned to peer only",
			assignments: []string{"node-b"},
			source:      "node-a",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDistributeBuiltImage(tt.assignments, tt.source); got != tt.want {
				t.Fatalf("shouldDistributeBuiltImage(%v, %q) = %v, want %v", tt.assignments, tt.source, got, tt.want)
			}
		})
	}
}

func TestBuildTakodContainerSpecDoesNotPublishPublicOneNodeService(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec(nil, "node-a", "web", &config.ServiceConfig{
		Port:  80,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
	}, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 0 {
		t.Fatalf("public one-node service should not publish direct host ports: %#v", container.Publishes)
	}
	wantAlias := runtimeid.ContainerNetworkAlias("demo", "production", "web", 1)
	if !slices.Contains(container.NetworkAliases, wantAlias) {
		t.Fatalf("container network aliases = %#v, want %q", container.NetworkAliases, wantAlias)
	}
	for _, alias := range container.NetworkAliases {
		if strings.Contains(alias, "_") {
			t.Fatalf("container network alias should be DNS-safe: %q", alias)
		}
	}
}

func TestServiceNetworkAliasesIncludeScopedDiscoveryNames(t *testing.T) {
	got := serviceNetworkAliases("demo-app", "production", "api")
	want := []string{
		"api",
		"api.tako.internal",
		"api.production.demo-app.tako.internal",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("serviceNetworkAliases() = %#v, want %#v", got, want)
	}
}

func TestTakodRegistryAuthMatchesConfiguredRegistry(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"node-a"})
	cfg.Registry = &config.RegistryConfig{
		URL:      "registry.example.com",
		Username: "deploy",
		Password: "secret-token",
	}

	auth := takodRegistryAuth(cfg, "registry.example.com/demo/web:1")
	if auth == nil {
		t.Fatal("expected registry auth")
	}
	if auth.Server != "registry.example.com" || auth.Username != "deploy" || auth.Password != "secret-token" {
		t.Fatalf("unexpected registry auth: %#v", auth)
	}
	if takodRegistryAuth(cfg, "ghcr.io/demo/web:1") != nil {
		t.Fatal("registry auth should not match a different registry host")
	}
}

func TestValidateServicePlatformCompatibilityRejectsMismatch(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	deploy.nodeInfoInspector = func(serverName string) (*takod.NodeInfoResponse, error) {
		return &takod.NodeInfoResponse{Platform: "linux/amd64"}, nil
	}

	err := deploy.ValidateServicePlatformCompatibility("web", &config.ServiceConfig{
		Image:    "demo/web:1",
		Platform: "linux/arm64",
		Replicas: 2,
	})
	if err == nil {
		t.Fatal("expected platform mismatch to be rejected")
	}
	if !strings.Contains(err.Error(), "platform linux/arm64 does not match assigned node platform") {
		t.Fatalf("error = %q, want platform mismatch context", err)
	}
}

func TestValidateServicePlatformCompatibilityAllowsMatchingNodes(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	deploy.nodeInfoInspector = func(serverName string) (*takod.NodeInfoResponse, error) {
		return &takod.NodeInfoResponse{Platform: "linux/amd64"}, nil
	}

	err := deploy.ValidateServicePlatformCompatibility("web", &config.ServiceConfig{
		Build:    ".",
		Platform: "linux/amd64",
		Replicas: 2,
	})
	if err != nil {
		t.Fatalf("expected matching platform to pass: %v", err)
	}
}

func TestValidateBuildWithoutPlatformRejectsMixedAssignedPlatforms(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	deploy.nodeInfoInspector = func(serverName string) (*takod.NodeInfoResponse, error) {
		if serverName == "node-b" {
			return &takod.NodeInfoResponse{Platform: "linux/arm64"}, nil
		}
		return &takod.NodeInfoResponse{Platform: "linux/amd64"}, nil
	}

	err := deploy.ValidateServicePlatformCompatibility("web", &config.ServiceConfig{
		Build:    ".",
		Replicas: 2,
	})
	if err == nil {
		t.Fatal("expected mixed platform build to be rejected")
	}
	if !strings.Contains(err.Error(), "mixed platforms") {
		t.Fatalf("error = %q, want mixed platform context", err)
	}
}

func TestValidateBuildWithoutPlatformRejectsSourceMismatch(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	deploy.nodeInfoInspector = func(serverName string) (*takod.NodeInfoResponse, error) {
		if serverName == "node-a" {
			return &takod.NodeInfoResponse{Platform: "linux/amd64"}, nil
		}
		return &takod.NodeInfoResponse{Platform: "linux/arm64"}, nil
	}

	err := deploy.ValidateServicePlatformCompatibility("web", &config.ServiceConfig{
		Build:    ".",
		Replicas: 1,
		Placement: &config.PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-b"},
		},
	})
	if err == nil {
		t.Fatal("expected source mismatch to be rejected")
	}
	if !strings.Contains(err.Error(), "source node node-a=linux/amd64") {
		t.Fatalf("error = %q, want source mismatch context", err)
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

	container, err := deploy.buildTakodContainerSpec(fakeTakodStatusExecutor{
		output: `{"hostPort":24567}`,
	}, "node-a", "web", &config.ServiceConfig{
		Port:  80,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
	}, 1, true)
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

func TestBuildTakodContainerSpecDoesNotPublishInternalServicePort(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec(nil, "node-a", "metrics", &config.ServiceConfig{Port: 9100}, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 0 {
		t.Fatalf("internal service should not publish host ports: %#v", container.Publishes)
	}
}

func TestBuildTakodContainerSpecPublishesExportedInternalPortOnMeshIP(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	service := config.ServiceConfig{
		Port: 4000,
		Export: &config.ServiceExportConfig{
			Ports: map[string]int{"web": 4000},
		},
	}
	env := deploy.config.Environments["production"]
	env.Services = map[string]config.ServiceConfig{"api": service}
	deploy.config.Environments["production"] = env

	container, err := deploy.buildTakodContainerSpec(fakeTakodStatusExecutor{
		output: `{"hostPort":24567}`,
	}, "node-a", "api", &service, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 1 || container.Publishes[0] != "10.210.0.1:24567:4000" {
		t.Fatalf("exported internal service publish = %#v, want mesh upstream", container.Publishes)
	}
}

func TestBuildTakodContainerSpecPublishesHostPortOnResolvedCIDR(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec(fakeTakodStatusExecutor{
		output: `{"hostPort":15353}`,
	}, "node-a", "dns", &config.ServiceConfig{
		Ports: []config.PortConfig{{
			Name:      "dns",
			Target:    5353,
			Published: 15353,
			Protocol:  "udp",
			Mode:      "host",
			HostIP:    "10.210.0.0/16",
		}},
	}, 1, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 1 || container.Publishes[0] != "10.210.0.1:15353:5353/udp" {
		t.Fatalf("host publish = %#v, want mesh CIDR UDP bind", container.Publishes)
	}
}

func TestAllocateMeshUpstreamPortUsesTakodAndCachesResult(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a", "node-b"}), environment: "production"}
	env := deploy.config.Environments["production"]
	env.Services = map[string]config.ServiceConfig{
		"web": {
			Port:  80,
			Proxy: &config.ProxyConfig{Domain: "example.com"},
		},
	}
	deploy.config.Environments["production"] = env

	port, err := deploy.allocateMeshUpstreamPort(fakeTakodStatusExecutor{
		output: `{"hostPort":24567}`,
	}, "node-a", "web", 1, 80)
	if err != nil {
		t.Fatalf("allocateMeshUpstreamPort returned error: %v", err)
	}
	if port != 24567 {
		t.Fatalf("allocated port = %d, want 24567", port)
	}

	port, err = deploy.allocateMeshUpstreamPort(fakeTakodStatusExecutor{
		err: fmt.Errorf("should not be called after cache fill"),
	}, "node-a", "web", 1, 80)
	if err != nil {
		t.Fatalf("cached allocateMeshUpstreamPort returned error: %v", err)
	}
	if port != 24567 {
		t.Fatalf("cached port = %d, want 24567", port)
	}
}

func TestPlanTakodAssignmentsHonorsPlacementConstraints(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"node-a", "node-b", "node-c"})
	cfg.Servers["node-a"] = config.ServerConfig{Host: "node-a.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	cfg.Servers["node-b"] = config.ServerConfig{Host: "node-b.example.test", User: "root", Labels: map[string]string{"role": "worker"}}
	cfg.Servers["node-c"] = config.ServerConfig{Host: "node-c.example.test", User: "root", Labels: map[string]string{"role": "web"}}
	deploy := &Deployer{config: cfg, environment: "production"}

	assignments, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
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

	assignments, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
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
		Env:   map[string]config.EnvValue{"TOKEN": config.PlainEnvValue("secret")},
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
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
			Volumes: map[string]config.VolumeConfig{
				"cache": {Name: "shared-cache"},
			},
		},
		environment: "production",
	}

	mounts, err := deploy.buildTakodMountSpecs("web", &config.ServiceConfig{
		Volumes: []string{"/data", "cache:/cache", "cache:/cache-ro:ro", "/srv/app/config:/config:ro"},
	})
	if err != nil {
		t.Fatalf("buildTakodMountSpecs returned error: %v", err)
	}
	want := []string{
		"type=volume,source=" + runtimeid.VolumeName("demo", "production", "/data") + ",target=/data",
		"type=volume,source=shared-cache,target=/cache",
		"type=volume,source=shared-cache,target=/cache-ro,readonly",
		"type=bind,source=/srv/app/config,target=/config,readonly",
	}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", mounts, want)
	}
}

func TestBuildTakodMountSpecsRejectsRelativeBindMount(t *testing.T) {
	deploy := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
		},
		environment: "production",
	}

	_, err := deploy.buildTakodMountSpecs("web", &config.ServiceConfig{
		Volumes: []string{"./data:/data"},
	})
	if err == nil {
		t.Fatal("buildTakodMountSpecs should reject relative bind mounts")
	}
	if !strings.Contains(err.Error(), "relative bind mount") {
		t.Fatalf("error = %q, want relative bind mount guidance", err)
	}
}

func TestBuildTakodConfigFilesEncodesContents(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "Caddyfile")
	if err := os.WriteFile(configPath, []byte(":80 {\n respond \"edge\"\n}\n"), 0600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	deploy := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
			Configs: map[string]config.ConfigFileConfig{
				"caddyfile": {Source: configPath},
			},
		},
		environment: "production",
	}

	files, err := deploy.buildTakodConfigFiles("edge", &config.ServiceConfig{
		Configs: []config.ServiceConfigFileMount{{
			Source: "caddyfile",
			Target: "/etc/caddy/Caddyfile",
			Mode:   "0444",
		}},
	})
	if err != nil {
		t.Fatalf("buildTakodConfigFiles returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %#v, want one config file", files)
	}
	got, err := base64.StdEncoding.DecodeString(files[0].ContentBase64)
	if err != nil {
		t.Fatalf("invalid content base64: %v", err)
	}
	if string(got) != ":80 {\n respond \"edge\"\n}\n" {
		t.Fatalf("content = %q, want Caddyfile contents", string(got))
	}
	if files[0].Source != "caddyfile" || files[0].Target != "/etc/caddy/Caddyfile" || files[0].Mode != "0444" || files[0].ContentHash == "" {
		t.Fatalf("unexpected config file request: %#v", files[0])
	}
}

func TestHookTargetServerPrefersAssignedNode(t *testing.T) {
	got := hookTargetServer(
		[]takodAssignment{{ServerName: "node-b", Slot: 2}, {ServerName: "node-a", Slot: 1}},
		[]string{"node-c"},
	)
	if got != "node-a" {
		t.Fatalf("hook target = %q, want first sorted assigned node", got)
	}

	got = hookTargetServer(nil, []string{"node-c", "node-d"})
	if got != "node-c" {
		t.Fatalf("hook target fallback = %q, want first target node", got)
	}
}

func TestBuildHookRequestMergesServiceAndHookRuntimeSettings(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	deploy := &Deployer{
		config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		environment: "production",
	}
	service := &config.ServiceConfig{
		Image: "demo/web:1",
		Env: map[string]config.EnvValue{
			"BASE":     config.PlainEnvValue("service"),
			"SERVICE":  config.PlainEnvValue("yes"),
			"OVERRIDE": config.PlainEnvValue("service"),
		},
		Volumes: []string{"data:/data"},
	}
	hook := &config.HookConfig{
		Command:    "npm run migrate",
		Timeout:    "5m",
		User:       "1000:1000",
		WorkingDir: "/app",
		Env: map[string]string{
			"HOOK":     "yes",
			"OVERRIDE": "hook",
		},
	}

	request, err := deploy.buildHookRequest("web", service, "demo/web:1", "preDeploy", hook)
	if err != nil {
		t.Fatalf("buildHookRequest returned error: %v", err)
	}
	if request.Hook != "preDeploy" || request.Image != "demo/web:1" || request.User != "1000:1000" || request.WorkingDir != "/app" || request.Timeout != "5m" {
		t.Fatalf("unexpected hook request: %#v", request)
	}
	if !slices.Equal(request.Command, []string{"sh", "-c", "npm run migrate"}) {
		t.Fatalf("command = %#v, want shell command", request.Command)
	}
	if !strings.Contains(request.EnvFileContent, "SERVICE=yes") ||
		!strings.Contains(request.EnvFileContent, "HOOK=yes") ||
		!strings.Contains(request.EnvFileContent, "OVERRIDE=hook") ||
		strings.Contains(request.EnvFileContent, "OVERRIDE=service") {
		t.Fatalf("env file content did not merge as expected: %q", request.EnvFileContent)
	}
	if len(request.Mounts) != 1 || !strings.Contains(request.Mounts[0], "target=/data") {
		t.Fatalf("mounts = %#v, want service mounts", request.Mounts)
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
