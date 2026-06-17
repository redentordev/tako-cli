package provisioner

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDockerRuntimeInfoDetectsRootfulDocker(t *testing.T) {
	info, err := parseDockerRuntimeInfo(`["name=seccomp,profile=builtin","name=cgroupns"]
/var/lib/docker
29.1.3
`)
	if err != nil {
		t.Fatalf("parseDockerRuntimeInfo returned error: %v", err)
	}
	if info.Rootless {
		t.Fatal("rootful security options should not be marked rootless")
	}
	if info.RootDir != "/var/lib/docker" || info.ServerVersion != "29.1.3" {
		t.Fatalf("info = %#v", info)
	}
}

func TestParseDockerRuntimeInfoDetectsRootlessDocker(t *testing.T) {
	info, err := parseDockerRuntimeInfo(`["name=rootless","name=seccomp,profile=builtin"]
/home/deploy/.local/share/docker
29.1.3
`)
	if err != nil {
		t.Fatalf("parseDockerRuntimeInfo returned error: %v", err)
	}
	if !info.Rootless {
		t.Fatalf("rootless security option should be detected: %#v", info)
	}
}

func TestParseDockerRuntimeInfoRejectsUnexpectedOutput(t *testing.T) {
	if _, err := parseDockerRuntimeInfo("not enough lines"); err == nil {
		t.Fatal("expected malformed docker info output to fail")
	}
}

func TestDetectDockerRuntimeUsesSudoDockerInfo(t *testing.T) {
	client := &recordingDockerRuntimeClient{
		output: `[]
/var/lib/docker
29.1.3
`,
	}

	info, err := DetectDockerRuntime(client)
	if err != nil {
		t.Fatalf("DetectDockerRuntime returned error: %v", err)
	}
	if info.Rootless {
		t.Fatal("expected rootful runtime")
	}
	if !strings.Contains(client.command, "sudo docker info --format") {
		t.Fatalf("command = %q, want sudo docker info", client.command)
	}
}

func TestDetectDockerRuntimeWrapsCommandFailure(t *testing.T) {
	client := &recordingDockerRuntimeClient{err: errors.New("permission denied")}

	_, err := DetectDockerRuntime(client)
	if err == nil {
		t.Fatal("expected command failure")
	}
	if !strings.Contains(err.Error(), "rootful Docker daemon is not reachable through sudo") {
		t.Fatalf("error = %q", err)
	}
}

type recordingDockerRuntimeClient struct {
	command string
	output  string
	err     error
}

func (r *recordingDockerRuntimeClient) Execute(command string) (string, error) {
	r.command = command
	return r.output, r.err
}
