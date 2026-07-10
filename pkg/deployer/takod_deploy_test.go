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
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	takounregistry "github.com/redentordev/tako-cli/pkg/unregistry"
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

func TestTakodDeployOptionsForService(t *testing.T) {
	buildService := &config.ServiceConfig{Build: "."}
	buildOptions := takodDeployOptionsForService(buildService, false)
	if !buildOptions.BuildImage || buildOptions.PullImage {
		t.Fatalf("build service options = %#v, want build without pull", buildOptions)
	}

	skipBuildOptions := takodDeployOptionsForService(buildService, true)
	if skipBuildOptions.BuildImage || skipBuildOptions.PullImage {
		t.Fatalf("skip-build options = %#v, want no build and no pull", skipBuildOptions)
	}

	imageOptions := takodDeployOptionsForService(&config.ServiceConfig{Image: "nginx:alpine"}, false)
	if imageOptions.BuildImage || !imageOptions.PullImage {
		t.Fatalf("image service options = %#v, want pull without build", imageOptions)
	}
}

func TestTakodRollbackDeployOptionsForService(t *testing.T) {
	buildOptions := takodRollbackDeployOptionsForService(&config.ServiceConfig{Build: "."})
	if !buildOptions.BuildImage || buildOptions.PullImage {
		t.Fatalf("build rollback options = %#v, want rebuild without pull", buildOptions)
	}

	imageOptions := takodRollbackDeployOptionsForService(&config.ServiceConfig{Image: "nginx:1.27"})
	if imageOptions.BuildImage || !imageOptions.PullImage {
		t.Fatalf("image rollback options = %#v, want pull without build", imageOptions)
	}

	emptyOptions := takodRollbackDeployOptionsForService(nil)
	if emptyOptions.BuildImage || emptyOptions.PullImage {
		t.Fatalf("nil rollback options = %#v, want no build and no pull", emptyOptions)
	}
}

func TestNormalizeLinuxBuildPlatform(t *testing.T) {
	tests := map[string]string{
		"x86_64\n": "linux/amd64",
		"amd64":    "linux/amd64",
		"aarch64":  "linux/arm64",
		"arm64\n":  "linux/arm64",
	}
	for input, want := range tests {
		got, err := normalizeLinuxBuildPlatform(input)
		if err != nil {
			t.Fatalf("normalizeLinuxBuildPlatform(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeLinuxBuildPlatform(%q) = %q, want %q", input, got, want)
		}
	}

	if _, err := normalizeLinuxBuildPlatform("riscv64"); err == nil {
		t.Fatal("normalizeLinuxBuildPlatform should reject unsupported architecture")
	}
}

func TestUnregistryPushTarget(t *testing.T) {
	target, key, err := unregistryPushTarget("node-a", config.ServerConfig{
		Host:   "203.0.113.10",
		User:   "deploy",
		Port:   2222,
		SSHKey: "/keys/id_ed25519",
	})
	if err != nil {
		t.Fatalf("unregistryPushTarget returned error: %v", err)
	}
	if target != "deploy@203.0.113.10:2222" {
		t.Fatalf("target = %q, want deploy@203.0.113.10:2222", target)
	}
	if key != "/keys/id_ed25519" {
		t.Fatalf("key = %q, want /keys/id_ed25519", key)
	}
}

func TestUnregistryPushTargetRejectsPasswordOnlyAuth(t *testing.T) {
	_, _, err := unregistryPushTarget("node-a", config.ServerConfig{
		Host:     "203.0.113.10",
		User:     "deploy",
		Password: "${SSH_PASSWORD}",
	})
	if err == nil {
		t.Fatal("unregistryPushTarget should reject password-only auth")
	}
	if !strings.Contains(err.Error(), "password-only SSH auth") {
		t.Fatalf("error = %q, want password-only guidance", err)
	}
}

func TestLocalBuildPushesSelectedPlatform(t *testing.T) {
	client := &recordingLocalImageClient{}
	deployer := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
			Servers: map[string]config.ServerConfig{
				"node-a": {Host: "203.0.113.10", User: "deploy", SSHKey: "/keys/id_ed25519"},
			},
		},
		localImageClient: client,
		verbose:          false,
	}
	if err := deployer.pushLocalImageToTakodNode(context.Background(), client, "demo/web:abc123", "linux/arm64", "node-a"); err != nil {
		t.Fatalf("pushLocalImageToTakodNode returned error: %v", err)
	}
	if len(client.pushes) != 1 {
		t.Fatalf("pushes = %#v, want 1", client.pushes)
	}
	if got := client.pushes[0].Platform; got != "linux/arm64" {
		t.Fatalf("push platform = %q, want linux/arm64", got)
	}
}

type recordingLocalImageClient struct {
	pushes []takounregistry.PushRequest
}

func (c *recordingLocalImageClient) CheckAvailable(context.Context) error {
	return nil
}

func (c *recordingLocalImageClient) Build(context.Context, takounregistry.BuildRequest) error {
	return nil
}

func (c *recordingLocalImageClient) Push(_ context.Context, req takounregistry.PushRequest) error {
	c.pushes = append(c.pushes, req)
	return nil
}

func TestBaseContextDefaultsToBackgroundAndThreadsCancellation(t *testing.T) {
	d := &Deployer{}
	if d.baseContext() != context.Background() {
		t.Fatalf("unset base context = %v, want context.Background()", d.baseContext())
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.SetBaseContext(ctx)
	if d.baseContext() != ctx {
		t.Fatalf("base context not threaded")
	}
	cancel()
	if err := d.baseContext().Err(); err != context.Canceled {
		t.Fatalf("base context err = %v, want context.Canceled", err)
	}
}

func TestServiceMemoryLimit(t *testing.T) {
	if got := serviceMemoryLimit(&config.ServiceConfig{}); got != "" {
		t.Fatalf("empty service memory limit = %q, want empty", got)
	}
	got := serviceMemoryLimit(&config.ServiceConfig{
		Resources: &config.ResourceLimitsConfig{Memory: "512m"},
	})
	if got != "512m" {
		t.Fatalf("service memory limit = %q, want 512m", got)
	}
}

func TestTakodRevisionPruneRequestsAreSortedAndKeepActiveRevision(t *testing.T) {
	got := takodRevisionPruneRequests("demo", "production", map[string]string{
		"web": "rev-web",
		"api": "rev-api",
	})
	if len(got) != 2 {
		t.Fatalf("requests = %#v, want 2", got)
	}
	if got[0].Service != "api" || got[0].KeepRevision != "rev-api" {
		t.Fatalf("first request = %#v, want api keep rev-api", got[0])
	}
	if got[1].Service != "web" || got[1].KeepRevision != "rev-web" {
		t.Fatalf("second request = %#v, want web keep rev-web", got[1])
	}
	for _, request := range got {
		if request.Project != "demo" || request.Environment != "production" {
			t.Fatalf("request identity = %#v, want demo/production", request)
		}
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

func TestEnsureTakodContainerArgvCapability(t *testing.T) {
	tests := []struct {
		name       string
		statusJSON string
		statusErr  error
		wantError  string
	}{
		{
			name:       "matching agent advertises support",
			statusJSON: `{"capabilities":["container.argv-v1"]}`,
		},
		{
			name:       "stale agent has no capabilities",
			statusJSON: `{"version":"dev"}`,
			wantError:  "does not support container argv payloads",
		},
		{
			name:       "malformed status fails closed",
			statusJSON: `{`,
			wantError:  "failed to parse takod status",
		},
		{
			name:      "unreachable status fails closed",
			statusErr: fmt.Errorf("takod unavailable"),
			wantError: "failed to verify takod capabilities",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := &Deployer{config: &config.Config{}}
			err := deploy.ensureTakodContainerArgvCapability(fakeTakodStatusExecutor{
				output: tt.statusJSON,
				err:    tt.statusErr,
			}, "node-a")
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ensureTakodContainerArgvCapability() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ensureTakodContainerArgvCapability() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestServiceNeedsContainerArgvCapability(t *testing.T) {
	tests := []struct {
		name    string
		service *config.ServiceConfig
		want    bool
	}{
		{name: "nil service", service: nil, want: false},
		{name: "legacy scalar command", service: &config.ServiceConfig{Command: config.StringValue("echo legacy")}, want: false},
		{name: "list command", service: &config.ServiceConfig{Command: config.ListValue("echo", "raw")}, want: true},
		{name: "scalar entrypoint", service: &config.ServiceConfig{Entrypoint: config.StringValue("/init")}, want: true},
		{name: "list entrypoint", service: &config.ServiceConfig{Entrypoint: config.ListValue("/init", "run")}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceNeedsContainerArgvCapability(tt.service); got != tt.want {
				t.Fatalf("serviceNeedsContainerArgvCapability() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceNeedsRuntimeControlsCapability(t *testing.T) {
	tests := []struct {
		name    string
		service *config.ServiceConfig
		want    bool
	}{
		{name: "nil", service: nil},
		{name: "none", service: &config.ServiceConfig{}},
		{name: "user", service: &config.ServiceConfig{User: "1000"}, want: true},
		{name: "working dir", service: &config.ServiceConfig{WorkingDir: "/work"}, want: true},
		{name: "stop grace", service: &config.ServiceConfig{StopGracePeriod: "60s"}, want: true},
		{name: "init", service: &config.ServiceConfig{Init: true}, want: true},
		{name: "extra hosts", service: &config.ServiceConfig{ExtraHosts: []string{"db:10.0.0.2"}}, want: true},
		{name: "ulimits", service: &config.ServiceConfig{Ulimits: map[string]config.UlimitConfig{"nofile": {Soft: 1, Hard: 1}}}, want: true},
		{name: "shm", service: &config.ServiceConfig{ShmSize: "256m"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceNeedsRuntimeControlsCapability(tt.service); got != tt.want {
				t.Fatalf("serviceNeedsRuntimeControlsCapability() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureTakodRuntimeControlsCapabilityFailsClosed(t *testing.T) {
	deploy := &Deployer{config: &config.Config{}}
	err := deploy.ensureTakodCapability(fakeTakodStatusExecutor{
		output: `{"capabilities":["container.argv-v1"]}`,
	}, "node-a", takod.CapabilityContainerRuntimeControlsV1, "container runtime controls")
	if err == nil || !strings.Contains(err.Error(), "does not support container runtime controls") {
		t.Fatalf("ensureTakodCapability() error = %v", err)
	}
}

func TestRunTakodBuildStrategyPreflightsEveryRemoteBuild(t *testing.T) {
	tests := []struct {
		name         string
		strategy     string
		localErr     error
		preflightErr error
		wantPhases   string
		wantError    bool
	}{
		{name: "remote stale", strategy: config.BuildStrategyRemote, preflightErr: fmt.Errorf("stale"), wantPhases: "preflight", wantError: true},
		{name: "remote current", strategy: config.BuildStrategyRemote, wantPhases: "preflight,remote"},
		{name: "auto local succeeds without daemon build capability", strategy: config.BuildStrategyAuto, wantPhases: "local"},
		{name: "auto fallback stale", strategy: config.BuildStrategyAuto, localErr: fmt.Errorf("local unavailable"), preflightErr: fmt.Errorf("stale"), wantPhases: "local,preflight", wantError: true},
		{name: "auto fallback current", strategy: config.BuildStrategyAuto, localErr: fmt.Errorf("local unavailable"), wantPhases: "local,preflight,remote"},
		{name: "local never checks daemon build capability", strategy: config.BuildStrategyLocal, wantPhases: "local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var phases []string
			_, err := runTakodBuildStrategy(tt.strategy,
				func() error { phases = append(phases, "local"); return tt.localErr },
				func() error { phases = append(phases, "preflight"); return tt.preflightErr },
				func() error { phases = append(phases, "remote"); return nil },
			)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			if got := strings.Join(phases, ","); got != tt.wantPhases {
				t.Fatalf("phases = %q, want %q", got, tt.wantPhases)
			}
		})
	}
}

func TestEnsureTakodBuildOptionsCapabilityFailsClosed(t *testing.T) {
	deploy := &Deployer{config: &config.Config{}}
	err := deploy.ensureTakodCapability(fakeTakodStatusExecutor{
		output: `{"capabilities":["container.argv-v1","container.runtime-controls-v1"]}`,
	}, "node-a", takod.CapabilityImageBuildOptionsV1, "structured image build options")
	if err == nil || !strings.Contains(err.Error(), "does not support structured image build options") {
		t.Fatalf("ensureTakodCapability() error = %v", err)
	}
}

func TestTakodServiceRolloutRuntimeControlsPreflightBeforeMutations(t *testing.T) {
	service := &config.ServiceConfig{User: "1000:1000", ShmSize: "256m"}
	var phases []string
	err := runTakodServiceRollout(serviceNeedsContainerArgvCapability(service) || serviceNeedsRuntimeControlsCapability(service), takodServiceRolloutOperations{
		Preflight: func() error {
			phases = append(phases, "preflight")
			deploy := &Deployer{config: &config.Config{}}
			return deploy.ensureTakodCapability(fakeTakodStatusExecutor{
				output: `{"capabilities":["container.argv-v1"]}`,
			}, "node-a", takod.CapabilityContainerRuntimeControlsV1, "container runtime controls")
		},
		Build: func() error {
			phases = append(phases, "build")
			return nil
		},
		Release: func() error {
			phases = append(phases, "release")
			return nil
		},
		Reconcile: func() error {
			phases = append(phases, "reconcile")
			return nil
		},
	})
	if err == nil {
		t.Fatal("runtime controls rollout succeeded with stale agent")
	}
	if got := strings.Join(phases, ","); got != "preflight" {
		t.Fatalf("phases = %q, want preflight only", got)
	}
}

func TestTakodJobBuildPreflightBeforeBuildAndRecord(t *testing.T) {
	var phases []string
	err := runTakodJobBuildPhases(func() error {
		phases = append(phases, "preflight")
		return fmt.Errorf("stale agent")
	}, func() error {
		phases = append(phases, "build")
		return nil
	}, func() {
		phases = append(phases, "record")
	})
	if err == nil {
		t.Fatal("job build phases succeeded after failed preflight")
	}
	if got := strings.Join(phases, ","); got != "preflight" {
		t.Fatalf("phases = %q, want preflight only", got)
	}
}

func TestTakodServiceRolloutPreflightsArgvBeforeEveryMutation(t *testing.T) {
	featureServices := []struct {
		name    string
		service *config.ServiceConfig
	}{
		{name: "list command", service: &config.ServiceConfig{Command: config.ListValue("echo", "raw")}},
		{name: "scalar entrypoint", service: &config.ServiceConfig{Entrypoint: config.StringValue("/init")}},
		{name: "list entrypoint", service: &config.ServiceConfig{Entrypoint: config.ListValue("/init", "run")}},
	}
	failures := []struct {
		name       string
		statusJSON string
		statusErr  error
	}{
		{name: "stale", statusJSON: `{"version":"dev"}`},
		{name: "malformed", statusJSON: `{`},
		{name: "unreachable", statusErr: fmt.Errorf("takod unavailable")},
	}
	serverNames := []string{"node-a", "node-b", "node-c"}

	for _, feature := range featureServices {
		for _, failure := range failures {
			t.Run(feature.name+"/"+failure.name, func(t *testing.T) {
				deploy := &Deployer{config: &config.Config{}}
				var mu sync.Mutex
				checked := make(map[string]bool)
				var mutations []string
				recordMutation := func(name string) error {
					mu.Lock()
					defer mu.Unlock()
					mutations = append(mutations, name)
					return nil
				}
				err := runTakodServiceRollout(serviceNeedsContainerArgvCapability(feature.service), takodServiceRolloutOperations{
					Preflight: func() error {
						return preflightTakodContainerArgvWithCheck(serverNames, func(serverName string) error {
							mu.Lock()
							checked[serverName] = true
							mu.Unlock()
							return deploy.ensureTakodContainerArgvCapability(fakeTakodStatusExecutor{
								output: failure.statusJSON,
								err:    failure.statusErr,
							}, serverName)
						})
					},
					Build:     func() error { return recordMutation("build") },
					Release:   func() error { return recordMutation("release") },
					Reconcile: func() error { return recordMutation("reconcile") },
				})
				if err == nil {
					t.Fatal("runTakodServiceRollout() succeeded with incompatible agents")
				}
				if len(checked) != len(serverNames) {
					t.Fatalf("preflight checked %v, want every target %v", checked, serverNames)
				}
				if len(mutations) != 0 {
					t.Fatalf("mutations ran after failed preflight: %v", mutations)
				}
			})
		}
	}
}

func TestTakodServiceRolloutLegacyScalarSkipsCapabilityAndReconciles(t *testing.T) {
	service := &config.ServiceConfig{Command: config.StringValue("echo legacy")}
	var phases []string
	record := func(name string) error {
		phases = append(phases, name)
		return nil
	}
	err := runTakodServiceRollout(serviceNeedsContainerArgvCapability(service), takodServiceRolloutOperations{
		Preflight: func() error {
			t.Fatal("legacy scalar command must not require a capability status")
			return nil
		},
		Build:     func() error { return record("build") },
		Release:   func() error { return record("release") },
		Reconcile: func() error { return record("reconcile") },
	})
	if err != nil {
		t.Fatalf("runTakodServiceRollout() error = %v", err)
	}
	if got, want := strings.Join(phases, ","), "build,release,reconcile"; got != want {
		t.Fatalf("phases = %q, want %q", got, want)
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

func TestBuildTakodHealthSpecUsesHostSideHTTPPathAndPort(t *testing.T) {
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

	if spec.Command != "" {
		t.Fatalf("health command = %q, want empty host-side health command", spec.Command)
	}
	if spec.Path != "/ready?token=a'b" {
		t.Fatalf("health path = %q, want configured path", spec.Path)
	}
	if spec.Port != 8080 {
		t.Fatalf("health port = %d, want 8080", spec.Port)
	}
	if spec.Scheme != "http" {
		t.Fatalf("health scheme = %q, want http", spec.Scheme)
	}
}

func TestBuildTakodHealthSpecUsesContainerCommand(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		HealthCheck: config.HealthCheckConfig{
			Command:     "test -f /tmp/healthy",
			Interval:    "30s",
			Timeout:     "5s",
			Retries:     4,
			StartPeriod: "2m",
		},
	})
	if spec == nil || spec.Command != "test -f /tmp/healthy" {
		t.Fatalf("health spec = %#v, want Docker command", spec)
	}
	if spec.Path != "" || spec.Port != 0 {
		t.Fatalf("command health should not synthesize host target: %#v", spec)
	}
}

func TestServiceRuntimeLabelsIncludeCustomLabelsWithoutOverridingTakoIdentity(t *testing.T) {
	service := config.ServiceConfig{
		Image: "nginx:latest",
		Labels: map[string]string{
			"com.example.role":             "frontend",
			runtimeid.ServiceIdentityLabel: "malicious",
		},
	}
	labels := serviceRuntimeLabels("demo", "production", "web", service)
	if labels["com.example.role"] != "frontend" {
		t.Fatalf("custom label missing: %#v", labels)
	}
	if labels[runtimeid.ServiceIdentityLabel] != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("runtime identity was overridden: %#v", labels)
	}
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

func TestBuildTakodHealthSpecUsesTCPPort(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 5432,
		HealthCheck: config.HealthCheckConfig{
			TCPPort: 5432,
			Timeout: "2s",
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "" {
		t.Fatalf("health path = %q, want empty TCP health path", spec.Path)
	}
	if spec.Scheme != "" {
		t.Fatalf("health scheme = %q, want empty TCP health scheme", spec.Scheme)
	}
	if spec.Port != 5432 {
		t.Fatalf("health port = %d, want 5432", spec.Port)
	}
	if spec.Timeout != "2s" {
		t.Fatalf("health timeout = %q, want 2s", spec.Timeout)
	}
}

func TestBuildTakodHealthSpecPrefersDeployReadinessHTTP(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		HealthCheck: config.HealthCheckConfig{
			Path:     "/health",
			Interval: "30s",
		},
		Deploy: config.DeployConfig{
			Readiness: config.DeployReadinessConfig{
				Path:     "/ready",
				Interval: "10s",
				Timeout:  "3s",
				Retries:  4,
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "/ready" {
		t.Fatalf("health path = %q, want deploy readiness path", spec.Path)
	}
	if spec.Port != 8080 || spec.Scheme != "http" {
		t.Fatalf("readiness target = %s/%d, want http/8080", spec.Scheme, spec.Port)
	}
	if spec.Interval != "10s" || spec.Timeout != "3s" || spec.Retries != 4 {
		t.Fatalf("readiness timing = interval %q timeout %q retries %d, want deploy readiness timing", spec.Interval, spec.Timeout, spec.Retries)
	}
	if spec.WaitAttempts != 55 {
		t.Fatalf("wait attempts = %d, want deploy readiness window", spec.WaitAttempts)
	}
}

func TestBuildTakodHealthSpecAttachesBlueGreenSmokeTest(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyBlueGreen,
			Readiness: config.DeployReadinessConfig{
				Path: "/ready",
			},
			SmokeTest: config.DeploySmokeTestConfig{
				Path:           "/smoke",
				ExpectedStatus: 204,
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "/ready" || spec.SmokePath != "/smoke" {
		t.Fatalf("health/smoke paths = %q/%q, want /ready and /smoke", spec.Path, spec.SmokePath)
	}
	if spec.SmokeExpectedStatus != 204 {
		t.Fatalf("smoke expected status = %d, want 204", spec.SmokeExpectedStatus)
	}
	if spec.SmokePort != 8080 {
		t.Fatalf("smoke port = %d, want service port 8080", spec.SmokePort)
	}
}

func TestBuildTakodHealthSpecSupportsSmokeOnlyBlueGreen(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyBlueGreen,
			SmokeTest: config.DeploySmokeTestConfig{
				Path: "/smoke",
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "" || spec.SmokePath != "/smoke" {
		t.Fatalf("health/smoke paths = %q/%q, want smoke-only", spec.Path, spec.SmokePath)
	}
	if spec.Port != 0 || spec.SmokePort != 8080 || spec.Scheme != "http" {
		t.Fatalf("smoke target = %s port=%d smokePort=%d, want http smokePort 8080", spec.Scheme, spec.Port, spec.SmokePort)
	}
	if spec.SmokeExpectedStatus != 200 {
		t.Fatalf("default smoke expected status = %d, want 200", spec.SmokeExpectedStatus)
	}
}

func TestBuildTakodHealthSpecKeepsReadinessTCPPortSeparateFromSmokePort(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyBlueGreen,
			Readiness: config.DeployReadinessConfig{
				TCPPort: 9000,
			},
			SmokeTest: config.DeploySmokeTestConfig{
				Path:           "/smoke",
				ExpectedStatus: 204,
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Port != 9000 || spec.SmokePort != 8080 {
		t.Fatalf("readiness/smoke ports = %d/%d, want 9000 and 8080", spec.Port, spec.SmokePort)
	}
}

func TestBuildTakodHealthSpecUsesDeployReadinessTCPPort(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 3000,
		Deploy: config.DeployConfig{
			Readiness: config.DeployReadinessConfig{
				TCPPort:  9000,
				Interval: "5s",
				Timeout:  "2s",
				Retries:  2,
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "" || spec.Scheme != "" {
		t.Fatalf("tcp readiness path/scheme = %q/%q, want empty", spec.Path, spec.Scheme)
	}
	if spec.Port != 9000 {
		t.Fatalf("readiness port = %d, want tcpPort", spec.Port)
	}
	if spec.Interval != "5s" || spec.Timeout != "2s" || spec.Retries != 2 {
		t.Fatalf("readiness timing = interval %q timeout %q retries %d, want configured timing", spec.Interval, spec.Timeout, spec.Retries)
	}
}

func TestBuildTakodHealthSpecIgnoresTimingOnlyDeployReadiness(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port: 8080,
		HealthCheck: config.HealthCheckConfig{
			Path: "/health",
		},
		Deploy: config.DeployConfig{
			Readiness: config.DeployReadinessConfig{
				Timeout: "1s",
			},
		},
	})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	if spec.Path != "/health" {
		t.Fatalf("health path = %q, want service healthCheck fallback", spec.Path)
	}
	if spec.Timeout != "5s" {
		t.Fatalf("health timeout = %q, want healthCheck default timeout", spec.Timeout)
	}
}

func TestDeploymentHealthWaitAttemptsUsesFloorAndCap(t *testing.T) {
	if got := deploymentHealthWaitAttempts("1s", "0s", 1); got != 30 {
		t.Fatalf("short health wait attempts = %d, want floor 30", got)
	}
	if got := deploymentHealthWaitAttempts("24h", "24h", 100); got != 24*60*60 {
		t.Fatalf("long health wait attempts = %d, want cap %d", got, 24*60*60)
	}
}

func TestBuildTakodCommandHealthSpecCoversTenMinuteStartPeriod(t *testing.T) {
	deploy := &Deployer{}
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{HealthCheck: config.HealthCheckConfig{
		Command: "test -f /tmp/heartbeat", Interval: "30s", Timeout: "5s", Retries: 3, StartPeriod: "10m",
	}})
	if spec == nil {
		t.Fatal("buildTakodHealthSpec returned nil")
	}
	want := 10*60 + 4*30 + 5
	if spec.WaitAttempts != want {
		t.Fatalf("wait attempts = %d, want %d to cover start period and retries", spec.WaitAttempts, want)
	}
	if spec.WaitAttempts <= 600 {
		t.Fatalf("wait attempts = %d, must extend beyond ten-minute grace", spec.WaitAttempts)
	}
}

func TestReconcileServiceRequestTimeoutCoversHealthWindowPerReplica(t *testing.T) {
	got := reconcileServiceRequestTimeout(takod.ReconcileServiceRequest{
		Containers: []takod.ContainerSpec{{Name: "web-1"}, {Name: "web-2"}},
		Health:     &takod.HealthSpec{WaitAttempts: 350},
	})
	want := 820 * time.Second
	if got != want {
		t.Fatalf("timeout = %s, want %s", got, want)
	}

	got = reconcileServiceRequestTimeout(takod.ReconcileServiceRequest{
		Containers: []takod.ContainerSpec{{Name: "web-1"}},
	})
	if got != takodclient.JSONRequestTimeout {
		t.Fatalf("default timeout = %s, want %s", got, takodclient.JSONRequestTimeout)
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
	}, 1, "rev-web", false, 0, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 0 {
		t.Fatalf("public one-node service should not publish direct host ports: %#v", container.Publishes)
	}
	if len(container.NetworkAliases) != 1 || container.NetworkAliases[0] != runtimeid.ContainerAlias("demo", "production", "web", 1) {
		t.Fatalf("network aliases = %#v, want generated DNS-safe alias", container.NetworkAliases)
	}
	for key, want := range map[string]string{
		reconcile.RevisionLabel:       "rev-web",
		reconcile.DeployStrategyLabel: config.DeployStrategyRecreate,
		reconcile.SlotLabel:           "1",
		reconcile.ActiveLabel:         "true",
	} {
		if container.Labels[key] != want {
			t.Fatalf("container label %s = %q, want %q in %#v", key, container.Labels[key], want, container.Labels)
		}
	}
}

func TestBuildTakodContainerSpecPublishesRawServicePorts(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	container, err := deploy.buildTakodContainerSpec("node-a", "game", &config.ServiceConfig{
		Ports: []string{"25565:25565/tcp", "127.0.0.1:9000:3000/udp"},
	}, 1, "rev-game", false, 0, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	want := []string{"25565:25565/tcp", "127.0.0.1:9000:3000/udp"}
	if len(container.Publishes) != len(want) {
		t.Fatalf("publishes = %#v, want %#v", container.Publishes, want)
	}
	for i, publish := range want {
		if container.Publishes[i] != publish {
			t.Fatalf("publishes[%d] = %q, want %q", i, container.Publishes[i], publish)
		}
	}
}

func TestBuildTakodContainerSpecUsesRevisionScopedIDsForNoDowntimeStrategies(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	service := &config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyRolling,
		},
	}

	container, err := deploy.buildTakodContainerSpec("node-a", "web", service, 2, "rev-green", false, 0, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if container.Name != runtimeid.RevisionContainerName("demo", "production", "web", "rev-green", 2) {
		t.Fatalf("container name = %q, want revision-scoped name", container.Name)
	}
	wantAlias := runtimeid.RevisionContainerAlias("demo", "production", "web", "rev-green", 2)
	if len(container.NetworkAliases) != 1 || container.NetworkAliases[0] != wantAlias {
		t.Fatalf("network aliases = %#v, want revision-scoped alias %q", container.NetworkAliases, wantAlias)
	}
	if got := container.Labels[reconcile.DeployStrategyLabel]; got != config.DeployStrategyRolling {
		t.Fatalf("strategy label = %q, want rolling", got)
	}
	if got := container.Labels[reconcile.RevisionLabel]; got != "rev-green" {
		t.Fatalf("revision label = %q, want rev-green", got)
	}
}

func TestBuildTakodContainerSpecMarksWarmOnlyRevisionInactive(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	service := &config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy:  config.DeployStrategyBlueGreen,
			Promotion: config.DeployPromotionManual,
		},
	}

	container, err := deploy.buildTakodContainerSpec("node-a", "web", service, 1, "rev-green", false, 0, true)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if got := container.Labels[reconcile.ActiveLabel]; got != "false" {
		t.Fatalf("active label = %q, want false for manual blue-green", got)
	}
}

func TestBuildTakodContainerSpecRequiresRevisionForRevisionScopedStrategies(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	_, err := deploy.buildTakodContainerSpec("node-a", "web", &config.ServiceConfig{
		Port: 8080,
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyBlueGreen,
		},
	}, 1, " ", false, 0, false)
	if err == nil {
		t.Fatal("expected missing revision to fail for blue-green container spec")
	}
	if !strings.Contains(err.Error(), "requires a revision-scoped container name") {
		t.Fatalf("error = %v, want revision-scoped container name message", err)
	}
}

func TestBuildTakodNetworkAttachmentsUsesServiceScopedExportNetworks(t *testing.T) {
	deploy := &Deployer{config: testTakodDeployConfig([]string{"node-a"}), environment: "production"}
	deploy.config.Project.Name = "frontend"

	got := deploy.buildTakodNetworkAttachments("web", &config.ServiceConfig{
		Export:  true,
		Imports: []string{"backend-api.api", "metrics.collector"},
	})

	if len(got) != 3 {
		t.Fatalf("network attachments = %#v, want export plus two imports", got)
	}
	if got[0].Network != runtimeid.ExportNetworkName("frontend", "production", "web") || !got[0].Create {
		t.Fatalf("export attachment = %#v, want created frontend web export network", got[0])
	}
	if len(got[0].Aliases) != 1 || got[0].Aliases[0] != runtimeid.ExportAlias("frontend", "production", "web") {
		t.Fatalf("export aliases = %#v, want readable export alias", got[0].Aliases)
	}
	for key, want := range map[string]string{
		"tako.runtime":      "takod",
		"tako.discovery":    "export",
		"tako.project":      "frontend",
		"tako.environment":  "production",
		"tako.service":      "web",
		"tako.export.alias": runtimeid.ExportAlias("frontend", "production", "web"),
	} {
		if got[0].Labels[key] != want {
			t.Fatalf("export label %s = %q, want %q in %#v", key, got[0].Labels[key], want, got[0].Labels)
		}
	}
	if got[1].Network != runtimeid.ExportNetworkName("backend-api", "production", "api") || got[1].Create {
		t.Fatalf("first import attachment = %#v, want backend api import network", got[1])
	}
	if got[2].Network != runtimeid.ExportNetworkName("metrics", "production", "collector") || got[2].Create {
		t.Fatalf("second import attachment = %#v, want metrics collector import network", got[2])
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
	}, 1, "rev-web", true, 24321, false)
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
	container, err := deploy.buildTakodContainerSpec("node-a", "metrics", &config.ServiceConfig{Port: 9100}, 1, "rev-metrics", false, 0, false)
	if err != nil {
		t.Fatalf("buildTakodContainerSpec returned error: %v", err)
	}
	if len(container.Publishes) != 0 {
		t.Fatalf("internal service should not publish host ports: %#v", container.Publishes)
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
	}, "node-a", "web", "", 1, 80)
	if err != nil {
		t.Fatalf("allocateMeshUpstreamPort returned error: %v", err)
	}
	if port != 24567 {
		t.Fatalf("allocated port = %d, want 24567", port)
	}

	port, err = deploy.allocateMeshUpstreamPort(fakeTakodStatusExecutor{
		err: fmt.Errorf("should not be called after cache fill"),
	}, "node-a", "web", "", 1, 80)
	if err != nil {
		t.Fatalf("cached allocateMeshUpstreamPort returned error: %v", err)
	}
	if port != 24567 {
		t.Fatalf("cached port = %d, want 24567", port)
	}
}

func TestMeshUpstreamRevisionForStrategyOnlyUsesNoDowntimeRevisions(t *testing.T) {
	if got := meshUpstreamRevisionForStrategy("rev-web", config.DeployStrategyRecreate); got != "" {
		t.Fatalf("recreate mesh revision = %q, want empty legacy allocation key", got)
	}
	if got := meshUpstreamRevisionForStrategy(" rev-web ", config.DeployStrategyRolling); got != "rev-web" {
		t.Fatalf("rolling mesh revision = %q, want trimmed revision", got)
	}
	if got := meshUpstreamRevisionForStrategy("rev-web", config.DeployStrategyBlueGreen); got != "rev-web" {
		t.Fatalf("blue-green mesh revision = %q, want revision", got)
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
		Image:      "postgres:16-alpine",
		Port:       5432,
		Persistent: true,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
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
	if labels["tako.persistent"] != "true" {
		t.Fatalf("persistent label = %q, want true", labels["tako.persistent"])
	}
}

func TestServiceRuntimeLabelsIncludeRedactedHashForEnvMaterial(t *testing.T) {
	service := config.ServiceConfig{
		Image: "nginx:1.27",
		Env:   map[string]string{"TOKEN": "secret"},
	}
	wantHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected redacted service hash")
	}

	labels := serviceRuntimeLabels("demo", "production", "web", service)
	if labels[reconcile.ConfigHashLabel] != wantHash {
		t.Fatalf("config hash label = %q, want %q", labels[reconcile.ConfigHashLabel], wantHash)
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

	mounts, externalVolumes, err := deploy.buildTakodMountSpecs("web", &config.ServiceConfig{
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
	if len(externalVolumes) != 0 {
		t.Fatalf("external volumes = %#v, want none", externalVolumes)
	}
}

func TestBuildTakodMountSpecsHonorsExternalVolumeNames(t *testing.T) {
	deploy := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
			Volumes: map[string]config.VolumeConfig{
				"n8n_data": {
					External: true,
					Name:     "captain--n8n-data",
				},
				"cache": {
					Name: "shared-cache",
				},
			},
		},
		environment: "production",
	}

	mounts, externalVolumes, err := deploy.buildTakodMountSpecs("n8n", &config.ServiceConfig{
		Volumes: []string{"n8n_data:/home/node/.n8n", "cache:/cache"},
	})
	if err != nil {
		t.Fatalf("buildTakodMountSpecs returned error: %v", err)
	}
	wantMounts := []string{
		"type=volume,source=captain--n8n-data,target=/home/node/.n8n",
		"type=volume,source=shared-cache,target=/cache",
	}
	if !slices.Equal(mounts, wantMounts) {
		t.Fatalf("mounts = %#v, want %#v", mounts, wantMounts)
	}
	wantExternal := []string{"captain--n8n-data"}
	if !slices.Equal(externalVolumes, wantExternal) {
		t.Fatalf("external volumes = %#v, want %#v", externalVolumes, wantExternal)
	}
}

func TestBuildTakodBackupScheduleRequestUsesConfiguredVolumesAndStorage(t *testing.T) {
	deploy := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
			Volumes: map[string]config.VolumeConfig{
				"pgdata": {External: true, Name: "captain--pgdata"},
			},
		},
		environment: "production",
	}

	request, err := deploy.buildTakodBackupScheduleRequest("postgres", &config.ServiceConfig{
		Volumes: []string{"pgdata:/var/lib/postgresql/data", "/cache", "/host/uploads:/uploads"},
		Backup: &config.BackupConfig{
			Schedule: "0 2 * * *",
			Retain:   14,
			Volumes:  []string{"pgdata"},
			Storage: &config.BackupStorageConfig{
				Provider:        config.BackupStorageProviderR2,
				Bucket:          "backups",
				Region:          "auto",
				Endpoint:        "https://account.r2.cloudflarestorage.com",
				Prefix:          "apps",
				AccessKeyID:     "access",
				SecretAccessKey: "secret",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildTakodBackupScheduleRequest returned error: %v", err)
	}
	if request.Project != "demo" || request.Environment != "production" || request.Service != "postgres" {
		t.Fatalf("request identity = %#v", request)
	}
	if request.Schedule != "0 2 * * *" || request.RetentionDays != 14 {
		t.Fatalf("request schedule = %q retain=%d", request.Schedule, request.RetentionDays)
	}
	wantVolume := takod.BackupScheduleVolume{
		Volume:         "pgdata",
		DockerVolume:   "captain--pgdata",
		ExternalVolume: true,
	}
	if !slices.Equal(request.Volumes, []takod.BackupScheduleVolume{wantVolume}) {
		t.Fatalf("request volumes = %#v, want %#v", request.Volumes, []takod.BackupScheduleVolume{wantVolume})
	}
	if request.Storage == nil || request.Storage.Provider != takod.BackupStorageProviderR2 || request.Storage.Bucket != "backups" {
		t.Fatalf("request storage = %#v", request.Storage)
	}
}

func TestBuildTakodBackupScheduleRequestDefaultsToBackupableServiceVolumes(t *testing.T) {
	deploy := &Deployer{
		config:      &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		environment: "production",
	}

	request, err := deploy.buildTakodBackupScheduleRequest("app", &config.ServiceConfig{
		Volumes: []string{"cache:/cache", "/data", "/host/uploads:/uploads", "broken:"},
		Backup:  &config.BackupConfig{Schedule: "@daily", Retain: 7},
	})
	if err != nil {
		t.Fatalf("buildTakodBackupScheduleRequest returned error: %v", err)
	}
	got := []string{request.Volumes[0].Volume, request.Volumes[1].Volume}
	slices.Sort(got)
	want := []string{"cache", "data"}
	if !slices.Equal(got, want) {
		t.Fatalf("backup volumes = %#v, want %#v", got, want)
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
