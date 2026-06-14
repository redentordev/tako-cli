package takod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestBuildServiceContainerArgs(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:      "demo",
		Environment:  "production",
		Service:      "web",
		Image:        "registry.example.com/demo/web:abc",
		Restart:      "unless-stopped",
		Network:      "tako_demo_production",
		NetworkAlias: "web",
		EnvFile:      "/tmp/web.env",
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
	case "ps":
		if output := os.Getenv("TAKO_FAKE_PS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "network":
		_, _ = os.Stdout.WriteString("network-ok\n")
		os.Exit(0)
	case "pull":
		_, _ = os.Stdout.WriteString("pulled\n")
		os.Exit(0)
	case "run":
		_, _ = os.Stdout.WriteString("container-id\n")
		os.Exit(0)
	case "inspect":
		joined := strings.Join(commandArgs, " ")
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
