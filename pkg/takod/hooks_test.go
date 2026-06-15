package takod

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunHookCommandRemovesSuccessfulContainer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	response, err := RunHookCommand(context.Background(), RunHookRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Hook:           HookPreDeploy,
		Image:          "demo/web:1",
		Network:        "tako_demo_production",
		EnvFileContent: "HOOK_MODE=pre\n",
		Mounts:         []string{"type=volume,source=demo_data,target=/data"},
		Command:        []string{"sh", "-c", "echo migrate"},
		User:           "1000:1000",
		WorkingDir:     "/app",
		Timeout:        "5m",
	})
	if err != nil {
		t.Fatalf("RunHookCommand returned error: %v", err)
	}
	if response.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", response.ExitCode)
	}

	entries := readCommandLog(t, logPath)
	joined := strings.Join(entries, "\n")
	for _, want := range []string{
		"docker run -d",
		"--label tako.hook=true",
		"--label tako.hook.phase=preDeploy",
		"--user 1000:1000",
		"--workdir /app",
		"--env-file ",
		"--mount type=volume,source=demo_data,target=/data",
		"demo/web:1 sh -c echo migrate",
		"docker wait " + response.Container,
		"docker rm -f " + response.Container,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
}

func TestRunHookCommandKeepsFailedContainer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_WAIT_OUTPUT", "7\n")

	response, err := RunHookCommand(context.Background(), RunHookRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Hook:        HookPostDeploy,
		Image:       "demo/web:1",
		Network:     "tako_demo_production",
		Command:     []string{"sh", "-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("RunHookCommand returned error: %v", err)
	}
	if response.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", response.ExitCode)
	}
	if !strings.Contains(response.Error, "failed") {
		t.Fatalf("response error = %q, want failed message", response.Error)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if entry == "docker rm -f "+response.Container {
			t.Fatalf("failed hook container should be kept, log = %#v", entries)
		}
	}
	if !slices.Contains(entries, "docker wait "+response.Container) {
		t.Fatalf("docker log missing wait for failed container: %#v", entries)
	}
}

func TestValidateRunHookRequestRejectsUnsafeInput(t *testing.T) {
	valid := RunHookRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Hook:        HookPreDeploy,
		Image:       "demo/web:1",
		Network:     "tako_demo_production",
		Command:     []string{"echo", "ok"},
	}
	if err := validateRunHookRequest(valid); err != nil {
		t.Fatalf("valid request returned error: %v", err)
	}

	invalid := valid
	invalid.Hook = "bad"
	if err := validateRunHookRequest(invalid); err == nil {
		t.Fatal("expected invalid hook to be rejected")
	}

	invalid = valid
	invalid.Command = nil
	if err := validateRunHookRequest(invalid); err == nil {
		t.Fatal("expected missing command to be rejected")
	}

	invalid = valid
	invalid.User = "bad\nuser"
	if err := validateRunHookRequest(invalid); err == nil {
		t.Fatal("expected invalid user to be rejected")
	}
}
