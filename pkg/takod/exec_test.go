package takod

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestResolveExecTargetSelectsFirstHealthyContainer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_PS_OUTPUT", "web-2\nweb-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-1",
    "Name": "/web-1",
    "State": {"Running": true, "Health": {"Status": "starting"}}
  },
  {
    "Id": "container-2",
    "Name": "/web-2",
    "State": {"Running": true, "Health": {"Status": "healthy"}}
  }
]`)

	response, err := ResolveExecTarget(context.Background(), ExecTargetRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
	})
	if err != nil {
		t.Fatalf("ResolveExecTarget returned error: %v", err)
	}
	if response.Container != "web-2" || response.ContainerID != "container-2" {
		t.Fatalf("unexpected exec target: %#v", response)
	}
}

func TestExecuteServiceCommandUsesDockerExecWithSelectedReplica(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	containerName := runtimeid.ContainerName("demo", "production", "web", 2)
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-2",
    "Name": "/`+containerName+`",
    "State": {"Running": true, "Health": {"Status": "healthy"}}
  }
]`)

	var output bytes.Buffer
	exitCode, err := ExecuteServiceCommand(context.Background(), ExecStreamRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Slot:        2,
		Command:     []string{"sh", "-c", "echo ok"},
		Stdin:       true,
	}, strings.NewReader("input"), &output)
	if err != nil {
		t.Fatalf("ExecuteServiceCommand returned error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}

	entries := readCommandLog(t, logPath)
	want := "docker exec -i " + containerName + " sh -c echo ok"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing %q in %#v", want, entries)
	}
}

func TestBuildOneOffRunArgsIncludesRuntimeScope(t *testing.T) {
	args := buildOneOffRunArgs(RunOneOffRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "demo/web:1.0.0-production",
		Network:     "tako_demo_production",
		EnvFile:     "/tmp/tako-env",
		Mounts:      []string{"type=volume,source=demo_data,target=/data"},
		Command:     []string{"npm", "run", "migrate"},
		Stdin:       true,
		Remove:      true,
	})

	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"run",
		"--rm",
		"-i",
		"--network\x00tako_demo_production",
		"--label\x00tako.project=demo",
		"--label\x00tako.environment=production",
		"--label\x00tako.service=web",
		"--label\x00tako.oneOff=true",
		"--env-file\x00/tmp/tako-env",
		"--mount\x00type=volume,source=demo_data,target=/data",
		"demo/web:1.0.0-production\x00npm\x00run\x00migrate",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("one-off args missing %q in %#v", want, args)
		}
	}
}

func TestValidateCommandArgsRejectsNUL(t *testing.T) {
	err := validateCommandArgs([]string{"sh", "-c", "echo bad\x00"})
	if err == nil {
		t.Fatal("expected NUL command arg to be rejected")
	}
}
