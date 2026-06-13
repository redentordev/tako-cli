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
	invalid.EnvFile = "relative.env"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected relative env file to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected empty container name to be rejected")
	}

	invalid = valid
	invalid.EnvFile = "/tmp/demo.env"
	invalid.EnvFileContent = "TOKEN=value\n"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected envFile and envFileContent together to be rejected")
	}

	invalid = valid
	invalid.EnvFileContent = string(make([]byte, (1<<20)+1))
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized envFileContent to be rejected")
	}

	invalid = valid
	invalid.Init = []string{" "}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected empty init command to be rejected")
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

func TestRunInitCommandsStopsOnFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_FAIL_SHELL", "fail now")

	err := runInitCommands(context.Background(), []string{"fail now", "should not run"})
	if err == nil {
		t.Fatal("expected init failure")
	}
	if !strings.Contains(err.Error(), "init command failed") {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 1 || !strings.Contains(entries[0], "fail now") {
		t.Fatalf("expected only the failing init command to run, got %#v", entries)
	}
}

func TestReconcileServiceRunsInitBeforeDockerMutation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Init:        []string{"echo preparing"},
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) < 2 {
		t.Fatalf("expected shell and docker commands, got %#v", entries)
	}
	if !strings.HasPrefix(entries[0], "sh -c echo preparing") {
		t.Fatalf("expected init command first, got %#v", entries)
	}
	if !strings.HasPrefix(entries[1], "docker ps ") {
		t.Fatalf("expected first Docker mutation after init to list old containers, got %#v", entries)
	}
}

func useFakeCommands(t *testing.T, logPath string) func() {
	t.Helper()
	oldDocker := dockerCommandContext
	oldShell := shellCommandContext
	dockerCommandContext = fakeCommandContext
	shellCommandContext = fakeCommandContext
	t.Setenv("GO_WANT_TAKOD_COMMAND_HELPER", "1")
	t.Setenv("TAKO_COMMAND_LOG", logPath)
	return func() {
		dockerCommandContext = oldDocker
		shellCommandContext = oldShell
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
		if strings.Contains(joined, ".State.Running") {
			_, _ = os.Stdout.WriteString("true\n")
		}
		os.Exit(0)
	case "logs":
		_, _ = os.Stdout.WriteString("logs\n")
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
