package takod

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestValidateReconcileServiceRequest(t *testing.T) {
	valid := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers: []ContainerSpec{
			{Name: "demo_production_web_1"},
		},
	}
	if err := validateReconcileServiceRequest(valid); err != nil {
		t.Fatalf("valid request returned error: %v", err)
	}

	invalid := valid
	invalid.EnvFile = "/tmp/demo.env"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected external env file path to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected empty container name to be rejected")
	}

	invalid = valid
	invalid.EnvFileContent = string(make([]byte, (1<<20)+1))
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized envFileContent to be rejected")
	}

	invalid = valid
	invalid.Project = "../demo"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe project name to be rejected")
	}

	invalid = valid
	invalid.Service = "Web"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe service name to be rejected")
	}

	invalid = valid
	invalid.Image = "demo\nweb"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe image name to be rejected")
	}

	invalid = valid
	invalid.Restart = "always;reboot"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe restart policy to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "bad/name"}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe container name to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_1", NetworkAliases: []string{"bad_alias"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe container network alias to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_1", Publishes: []string{"80:80\n--privileged"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe publish value to be rejected")
	}

	invalid = valid
	invalid.Mounts = []string{"type=bind,source=/data,target=/data\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe mount value to be rejected")
	}

	invalid = valid
	invalid.ConfigFiles = []ConfigFileMount{{
		Source:        "caddyfile",
		Target:        "etc/caddy/Caddyfile",
		Mode:          "0444",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(":80 {}\n")),
	}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected relative config target to be rejected")
	}

	invalid = valid
	invalid.ConfigFiles = []ConfigFileMount{{
		Source:        "caddyfile",
		Target:        "/etc/caddy/Caddyfile",
		Mode:          "0644",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(":80 {}\n")),
	}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected writable config mode to be rejected")
	}

	invalid = valid
	invalid.Labels = map[string]string{"tako.configHash": "abc\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe label value to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Command: "curl -sf /health\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe health command to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Interval: "not-a-duration"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected invalid health interval to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Retries: maxHealthRetries + 1}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized health retries to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{WaitAttempts: maxHealthWaitAttempts + 1}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized health waitAttempts to be rejected")
	}
}

func TestValidateRemoveServiceRequest(t *testing.T) {
	valid := RemoveServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
	}
	if err := validateRemoveServiceRequest(valid); err != nil {
		t.Fatalf("valid remove request returned error: %v", err)
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe project name to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod\n"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe environment name to be rejected")
	}

	invalid = valid
	invalid.Service = "Web"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe service name to be rejected")
	}
}

func TestPrepareServiceConfigFilesWritesReadOnlyFileAndMount(t *testing.T) {
	content := []byte(":80 {\n respond \"edge\"\n}\n")
	req := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "edge",
		ConfigDir:   t.TempDir(),
		ConfigFiles: []ConfigFileMount{{
			Source:        "caddyfile",
			Target:        "/etc/caddy/Caddyfile",
			Mode:          "0444",
			ContentBase64: base64.StdEncoding.EncodeToString(content),
		}},
	}

	if err := validateReconcileServiceRequest(ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "edge",
		Image:       "caddy:2.9-alpine",
		Network:     "tako_demo_production",
		ConfigFiles: req.ConfigFiles,
		Containers:  []ContainerSpec{{Name: "demo_production_edge_1"}},
	}); err != nil {
		t.Fatalf("valid config file request rejected: %v", err)
	}
	if err := prepareServiceConfigFiles(&req); err != nil {
		t.Fatalf("prepareServiceConfigFiles returned error: %v", err)
	}
	if len(req.Mounts) != 1 {
		t.Fatalf("mounts = %#v, want one generated mount", req.Mounts)
	}
	fields := parseDockerMountFields(req.Mounts[0])
	configPath := fields["source"]
	if configPath == "" || fields["target"] != "/etc/caddy/Caddyfile" || fields["type"] != "bind" {
		t.Fatalf("unexpected generated mount: %q", req.Mounts[0])
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read generated config file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("config content = %q, want %q", string(got), string(content))
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("failed to stat generated config file: %v", err)
	}
	if info.Mode().Perm() != 0444 {
		t.Fatalf("config file mode = %v, want 0444", info.Mode().Perm())
	}
}

func TestBuildServiceContainerArgs(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:      "demo",
		Environment:  "production",
		Service:      "web",
		Image:        "registry.example.com/demo/web:abc",
		Restart:      "unless-stopped",
		Network:      "tako_demo_production",
		NetworkAlias: "web",
		NetworkAliases: []string{
			"web",
		},
		EnvFile: "/tmp/web.env",
		Labels: map[string]string{
			"tako.role": "frontend",
		},
		Mounts:  []string{"type=volume,source=demo_data,target=/data"},
		Command: "npm run worker",
		Health: &HealthSpec{
			Command:     "curl -sf http://localhost:3000/health || exit 1",
			Interval:    "10s",
			Timeout:     "5s",
			Retries:     3,
			StartPeriod: "10s",
		},
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name:      "demo_production_web_1",
		Publishes: []string{"10.42.0.2:31001:3000"},
	})

	want := []string{
		"run", "-d",
		"--name", "demo_production_web_1",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--network-alias", "web",
		"--label", "tako.environment=production",
		"--label", "tako.project=demo",
		"--label", "tako.role=frontend",
		"--label", "tako.runtime=takod",
		"--label", "tako.service=web",
		"--env-file", "/tmp/web.env",
		"--mount", "type=volume,source=demo_data,target=/data",
		"--publish", "10.42.0.2:31001:3000",
		"--health-cmd", "curl -sf http://localhost:3000/health || exit 1",
		"--health-interval", "10s",
		"--health-timeout", "5s",
		"--health-retries", "3",
		"--health-start-period", "10s",
		"registry.example.com/demo/web:abc",
		"sh", "-c", "npm run worker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected docker args:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildServiceContainerArgsIncludesDiscoveryAliases(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
		Image:       "nginx:1.27",
		Restart:     "unless-stopped",
		Network:     "tako_demo_production",
		NetworkAliases: []string{
			"api",
			"api.tako.internal",
			"api.production.demo.tako.internal",
		},
	}

	got := buildServiceContainerArgs(req, ContainerSpec{Name: "demo_production_api_1"})
	for _, want := range []string{
		"--network-alias\x00api",
		"--network-alias\x00api.tako.internal",
		"--network-alias\x00api.production.demo.tako.internal",
	} {
		if !strings.Contains(strings.Join(got, "\x00"), want) {
			t.Fatalf("docker args missing alias %q: %#v", want, got)
		}
	}
}

func TestBuildServiceContainerArgsIncludesContainerNetworkAliases(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
		Image:       "nginx:1.27",
		Restart:     "unless-stopped",
		Network:     "tako_demo_production",
		NetworkAliases: []string{
			"api",
		},
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name:           "demo_production_api_1",
		NetworkAliases: []string{"tako-demo-production-api-1-abcdef1234"},
	})
	if !strings.Contains(strings.Join(got, "\x00"), "--network-alias\x00tako-demo-production-api-1-abcdef1234") {
		t.Fatalf("docker args missing container alias: %#v", got)
	}
}

func TestBuildServiceContainerArgsIncludesSlotLabel(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "nginx:1.27",
		Restart:     "unless-stopped",
		Network:     "tako_demo_production",
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name: "demo_production_web_2",
		Slot: 2,
	})
	if !slices.Contains(got, "--label") || !slices.Contains(got, "tako.slot=2") {
		t.Fatalf("docker args missing slot label: %#v", got)
	}
}

func TestNamedVolumeSourcesFromMountsReturnsUniqueSortedVolumes(t *testing.T) {
	got := namedVolumeSourcesFromMounts([]string{
		"type=volume,source=tako_demo_production_cache,target=/cache",
		"type=bind,source=/srv/data,target=/data",
		"type=volume,src=tako_demo_production_uploads,target=/uploads",
		"type=volume,source=tako_demo_production_cache,target=/cache2",
	})
	want := []string{"tako_demo_production_cache", "tako_demo_production_uploads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volume sources = %#v, want %#v", got, want)
	}
}

func TestReconcileServiceCreatesNamedVolumesWithRuntimeLabels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Mounts: []string{
			"type=volume,source=tako_demo_production_data,target=/data",
			"type=bind,source=/srv/demo,target=/srv/demo",
		},
		Containers: []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	want := "docker volume create --label tako.project=demo --label tako.environment=production --label tako.runtime=takod --label tako.service=web tako_demo_production_data"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing labeled volume create %q in %#v", want, entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "volume create") && strings.Contains(entry, "/srv/demo") {
			t.Fatalf("bind mount should not create Docker volume: %#v", entries)
		}
	}
}

func TestPrepareServiceEnvFileWritesAndCleansUpTempFile(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		EnvFileContent: "TOKEN=value\n",
	}

	cleanup, err := prepareServiceEnvFile(&req)
	if err != nil {
		t.Fatalf("prepareServiceEnvFile returned error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup function")
	}
	if req.EnvFileContent != "" {
		t.Fatal("expected env file content to be cleared after writing")
	}
	if req.EnvFile == "" || !filepath.IsAbs(req.EnvFile) {
		t.Fatalf("expected absolute env file path, got %q", req.EnvFile)
	}

	data, err := os.ReadFile(req.EnvFile)
	if err != nil {
		t.Fatalf("failed to read temp env file: %v", err)
	}
	if string(data) != "TOKEN=value\n" {
		t.Fatalf("temp env file content = %q", string(data))
	}

	cleanup()
	if _, err := os.Stat(req.EnvFile); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove temp env file, stat err=%v", err)
	}
}

func TestReconcileServiceRunsDockerMutation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) == 0 {
		t.Fatalf("expected docker commands, got %#v", entries)
	}
	if !strings.HasPrefix(entries[0], "docker ps ") {
		t.Fatalf("expected first Docker mutation to list old containers, got %#v", entries)
	}
}

func TestReconcileServiceRollingStartFirstRemovesOldAfterNewStarts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "oldid|demo_production_web_1|running\n")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:new",
		Network:     "tako_demo_production",
		Strategy:    "rolling",
		Order:       "start-first",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1", Slot: 1}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	runIndex := firstEntryContaining(entries, "docker run -d --name demo_production_web_1_next_")
	rmIndex := firstEntryContaining(entries, "docker rm -f demo_production_web_1")
	renameIndex := firstEntryContaining(entries, "docker rename demo_production_web_1_next_")
	if runIndex == -1 || rmIndex == -1 || renameIndex == -1 {
		t.Fatalf("rolling command log missing run/rm/rename: %#v", entries)
	}
	if !(runIndex < rmIndex && rmIndex < renameIndex) {
		t.Fatalf("start-first order should be run new, remove old, rename new; log=%#v", entries)
	}
}

func TestReconcileServiceRollingFallsBackToStopFirstOnPortConflict(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	marker := filepath.Join(t.TempDir(), "run-failed")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "oldid|demo_production_web_1|running\n")
	t.Setenv("TAKO_FAKE_RUN_FAIL_ONCE_CONTAINS", "demo_production_web_1_next_")
	t.Setenv("TAKO_FAKE_RUN_FAIL_MARKER", marker)

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:new",
		Network:     "tako_demo_production",
		Strategy:    "rolling",
		Order:       "start-first",
		Containers: []ContainerSpec{{
			Name:      "demo_production_web_1",
			Slot:      1,
			Publishes: []string{"127.0.0.1:3000:3000"},
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	firstRun := firstEntryContaining(entries, "docker run -d --name demo_production_web_1_next_")
	stopIndex := firstEntryContaining(entries, "docker stop demo_production_web_1")
	secondRun := nextEntryContaining(entries, "docker run -d --name demo_production_web_1_next_", firstRun+1)
	renameIndex := firstEntryContaining(entries, "docker rename demo_production_web_1_next_")
	if firstRun == -1 || stopIndex == -1 || secondRun == -1 || renameIndex == -1 {
		t.Fatalf("rolling fallback log missing run/stop/run/rename: %#v", entries)
	}
	if !(firstRun < stopIndex && stopIndex < secondRun && secondRun < renameIndex) {
		t.Fatalf("fallback order should be failed start-first, stop old, run new, rename; log=%#v", entries)
	}
}

func TestReconcileServiceRollingStartFirstKeepsOldContainerOnReplacementHealthFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "oldid|demo_production_renderer_1|running\n")
	t.Setenv("TAKO_FAKE_HEALTH_STATUS", "unhealthy")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "renderer",
		Image:       "registry.example.com/demo/renderer:new",
		Network:     "tako_demo_production",
		Strategy:    "rolling",
		Order:       "start-first",
		Containers:  []ContainerSpec{{Name: "demo_production_renderer_1", Slot: 1}},
		Health: &HealthSpec{
			Command:      "curl -sf http://127.0.0.1:3000/api/health || exit 1",
			WaitAttempts: 1,
		},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail when replacement health fails")
	}
	if !strings.Contains(err.Error(), "container health check failed") || !strings.Contains(err.Error(), "last logs") {
		t.Fatalf("error = %q, want health failure with logs", err)
	}

	entries := readCommandLog(t, logPath)
	if firstEntryContaining(entries, "docker rm -f demo_production_renderer_1_next_") == -1 {
		t.Fatalf("docker log missing failed replacement cleanup: %#v", entries)
	}
	if slices.Contains(entries, "docker rm -f demo_production_renderer_1") {
		t.Fatalf("old container should not be removed when replacement health fails: %#v", entries)
	}
	if firstEntryContaining(entries, "docker rename demo_production_renderer_1_next_") != -1 {
		t.Fatalf("replacement should not be renamed over old container: %#v", entries)
	}
}

func TestReconcileServiceCleansStartedContainersOnHealthFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_HEALTH_STATUS", "unhealthy")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			Command: "curl -sf http://127.0.0.1:3000/health || exit 1",
		},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail for unhealthy containers")
	}
	if !strings.Contains(err.Error(), "last logs") || !strings.Contains(err.Error(), "logs") {
		t.Fatalf("error = %q, want recent container logs", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker rm -f demo_production_web_1") {
		t.Fatalf("docker log missing cleanup of started container in %#v", entries)
	}
}

func TestContainerHealthWaitAttemptsDoesNotUseDockerRetryCount(t *testing.T) {
	got := containerHealthWaitAttempts(&HealthSpec{
		Command: "curl -sf http://127.0.0.1:3000/health || exit 1",
		Retries: 1,
	})
	if got != 30 {
		t.Fatalf("wait attempts = %d, want default 30", got)
	}
	if got := containerHealthWaitAttempts(&HealthSpec{Retries: 1, WaitAttempts: 45}); got != 45 {
		t.Fatalf("explicit wait attempts = %d, want 45", got)
	}
}

func TestRemoveServiceRemovesMatchingContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "abc123\ndef456\n")

	response, err := RemoveService(context.Background(), RemoveServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "old-api",
	})
	if err != nil {
		t.Fatalf("RemoveService returned error: %v", err)
	}
	if response.RemovedContainers != 2 {
		t.Fatalf("removed containers = %d, want 2", response.RemovedContainers)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("docker entries = %#v, want ps and rm", entries)
	}
	for _, want := range []string{
		"docker ps -aq --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=old-api",
		"docker rm -f abc123 def456",
	} {
		found := false
		for _, entry := range entries {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
}

func TestEnsureDockerNetworkCreatesLabeledNetwork(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_FAIL_NETWORK_INSPECT", "1")

	err := ensureDockerNetwork(context.Background(), "tako_demo_production", dockerNetworkOwner{
		Project:     "demo",
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("ensureDockerNetwork returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	want := "docker network create --label tako.project=demo --label tako.environment=production --label tako.runtime=takod tako_demo_production"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing labeled network create %q in %#v", want, entries)
	}
}

func useFakeCommands(t *testing.T, logPath string) func() {
	t.Helper()
	oldDocker := dockerCommandContext
	dockerCommandContext = fakeCommandContext
	t.Setenv("GO_WANT_TAKOD_COMMAND_HELPER", "1")
	t.Setenv("TAKO_COMMAND_LOG", logPath)
	return func() {
		dockerCommandContext = oldDocker
	}
}

func fakeCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestTakodCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestTakodCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKOD_COMMAND_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(args) {
		os.Exit(2)
	}
	command := args[separator+1]
	commandArgs := args[separator+2:]
	entry := strings.Join(append([]string{command}, commandArgs...), " ")
	if logPath := os.Getenv("TAKO_COMMAND_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.WriteString(entry + "\n")
		_ = file.Close()
	}

	if command == "sh" {
		if os.Getenv("TAKO_FAKE_FAIL_SHELL") != "" && strings.Contains(strings.Join(commandArgs, " "), os.Getenv("TAKO_FAKE_FAIL_SHELL")) {
			_, _ = os.Stderr.WriteString("shell failed")
			os.Exit(7)
		}
		os.Exit(0)
	}

	if command != "docker" || len(commandArgs) == 0 {
		os.Exit(0)
	}
	switch commandArgs[0] {
	case "info":
		if output := os.Getenv("TAKO_FAKE_INFO_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		} else {
			_, _ = os.Stdout.WriteString(`{"OSType":"linux","Architecture":"x86_64","OperatingSystem":"Fake Linux","ServerVersion":"27.0.0"}` + "\n")
		}
		os.Exit(0)
	case "buildx":
		if len(commandArgs) > 1 && commandArgs[1] == "version" {
			if os.Getenv("TAKO_FAKE_BUILDX_UNAVAILABLE") != "" {
				os.Exit(1)
			}
			if output := os.Getenv("TAKO_FAKE_BUILDX_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			} else {
				_, _ = os.Stdout.WriteString("github.com/docker/buildx v0.18.0\n")
			}
			os.Exit(0)
		}
		os.Exit(0)
	case "images":
		if output := os.Getenv("TAKO_FAKE_IMAGES_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "rmi":
		os.Exit(0)
	case "ps":
		if output := os.Getenv("TAKO_FAKE_PS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "network":
		if len(commandArgs) > 1 && commandArgs[1] == "inspect" && os.Getenv("TAKO_FAKE_FAIL_NETWORK_INSPECT") != "" {
			os.Exit(1)
		}
		if len(commandArgs) > 1 && commandArgs[1] == "ls" {
			if output := os.Getenv("TAKO_FAKE_NETWORK_LS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("network-ok\n")
		os.Exit(0)
	case "volume":
		if len(commandArgs) > 1 && commandArgs[1] == "ls" {
			if output := os.Getenv("TAKO_FAKE_VOLUME_LS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
		}
		if len(commandArgs) > 1 && commandArgs[1] == "inspect" {
			if failVolume := os.Getenv("TAKO_FAKE_FAIL_VOLUME_INSPECT"); failVolume != "" {
				if failVolume == "1" || strings.Contains(strings.Join(commandArgs, " "), failVolume) {
					_, _ = os.Stderr.WriteString("volume not found")
					os.Exit(1)
				}
			}
			if output := os.Getenv("TAKO_FAKE_VOLUME_INSPECT_LABELS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if len(commandArgs) > 1 && commandArgs[1] == "rm" {
			failVolume := os.Getenv("TAKO_FAKE_FAIL_VOLUME_RM")
			if failVolume != "" && strings.Contains(strings.Join(commandArgs, " "), failVolume) {
				_, _ = os.Stderr.WriteString("volume is in use")
				os.Exit(1)
			}
		}
		os.Exit(0)
	case "pull":
		if required := os.Getenv("TAKO_FAKE_REQUIRE_DOCKER_CONFIG"); required != "" {
			configDir := os.Getenv("DOCKER_CONFIG")
			if configDir == "" {
				_, _ = os.Stderr.WriteString("missing DOCKER_CONFIG")
				os.Exit(9)
			}
			data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
			if err != nil || !strings.Contains(string(data), required) {
				_, _ = os.Stderr.WriteString("missing required Docker auth config")
				os.Exit(9)
			}
		}
		_, _ = os.Stdout.WriteString("pulled\n")
		os.Exit(0)
	case "run":
		if needle := os.Getenv("TAKO_FAKE_RUN_FAIL_ONCE_CONTAINS"); needle != "" && strings.Contains(strings.Join(commandArgs, " "), needle) {
			marker := os.Getenv("TAKO_FAKE_RUN_FAIL_MARKER")
			if marker == "" {
				marker = filepath.Join(os.TempDir(), "tako-fake-run-failed")
			}
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				_ = os.WriteFile(marker, []byte("failed"), 0600)
				_, _ = os.Stderr.WriteString("port is already allocated")
				os.Exit(1)
			}
		}
		_, _ = os.Stdout.WriteString("container-id\n")
		os.Exit(0)
	case "wait":
		if output := os.Getenv("TAKO_FAKE_WAIT_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		} else {
			_, _ = os.Stdout.WriteString("0\n")
		}
		os.Exit(0)
	case "rm":
		os.Exit(0)
	case "stop":
		os.Exit(0)
	case "inspect":
		if output := os.Getenv("TAKO_FAKE_INSPECT_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
			os.Exit(0)
		}
		joined := strings.Join(commandArgs, " ")
		if strings.Contains(joined, ".Args") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_ARGS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".NetworkSettings.Ports") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_NETWORK_PORTS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".State.Health.Status") {
			if output := os.Getenv("TAKO_FAKE_HEALTH_STATUS"); output != "" {
				_, _ = os.Stdout.WriteString(output + "\n")
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".HostConfig.PortBindings") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_PORT_BINDINGS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Config.Labels") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_LABELS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".State.Running") {
			_, _ = os.Stdout.WriteString("true\n")
		}
		os.Exit(0)
	case "logs":
		_, _ = os.Stdout.WriteString("logs\n")
		os.Exit(0)
	case "stats":
		if output := os.Getenv("TAKO_FAKE_STATS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

func readCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func firstEntryContaining(entries []string, needle string) int {
	return nextEntryContaining(entries, needle, 0)
}

func nextEntryContaining(entries []string, needle string, start int) int {
	for i := start; i < len(entries); i++ {
		if strings.Contains(entries[i], needle) {
			return i
		}
	}
	return -1
}
