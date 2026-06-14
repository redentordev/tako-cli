package takod

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAllocatePortPersistsAndReusesAllocation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	dataDir := t.TempDir()
	req := testPortAllocationRequest()

	first, err := AllocatePort(context.Background(), dataDir, req)
	if err != nil {
		t.Fatalf("AllocatePort returned error: %v", err)
	}
	if first.HostPort != req.PreferredPort {
		t.Fatalf("host port = %d, want preferred %d", first.HostPort, req.PreferredPort)
	}

	second, err := AllocatePort(context.Background(), dataDir, req)
	if err != nil {
		t.Fatalf("second AllocatePort returned error: %v", err)
	}
	if second.HostPort != first.HostPort || second.Key != first.Key {
		t.Fatalf("allocation was not reused: first=%#v second=%#v", first, second)
	}

	if _, err := os.Stat(portAllocationRegistryPath(dataDir)); err != nil {
		t.Fatalf("expected registry file to exist: %v", err)
	}
}

func TestAllocatePortAvoidsDockerOccupiedPort(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "container-a\n")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", `{"80/tcp":[{"HostIp":"10.210.0.1","HostPort":"31001"}]}`)

	req := testPortAllocationRequest()
	req.PreferredPort = 31001
	req.MinPort = 31001
	req.MaxPort = 31003

	allocation, err := AllocatePort(context.Background(), t.TempDir(), req)
	if err != nil {
		t.Fatalf("AllocatePort returned error: %v", err)
	}
	if allocation.HostPort != 31002 {
		t.Fatalf("host port = %d, want first free port 31002", allocation.HostPort)
	}
}

func TestAllocatePortReusesSameServiceDockerPort(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "container-a\n")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", `{"3000/tcp":[{"HostIp":"10.210.0.1","HostPort":"31001"}]}`)
	t.Setenv("TAKO_FAKE_INSPECT_LABELS", `{"tako.project":"demo","tako.environment":"production","tako.service":"web"}`)

	req := testPortAllocationRequest()
	req.PreferredPort = 31001
	req.MinPort = 31001
	req.MaxPort = 31003

	allocation, err := AllocatePort(context.Background(), t.TempDir(), req)
	if err != nil {
		t.Fatalf("AllocatePort returned error: %v", err)
	}
	if allocation.HostPort != 31001 {
		t.Fatalf("host port = %d, want same-service port 31001", allocation.HostPort)
	}
}

func TestReleaseServicePortAllocations(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	dataDir := t.TempDir()
	if _, err := AllocatePort(context.Background(), dataDir, testPortAllocationRequest()); err != nil {
		t.Fatalf("AllocatePort returned error: %v", err)
	}
	if err := ReleaseServicePortAllocations(context.Background(), dataDir, "demo", "production", "web"); err != nil {
		t.Fatalf("ReleaseServicePortAllocations returned error: %v", err)
	}

	registry, err := readPortAllocationRegistry(portAllocationRegistryPath(dataDir))
	if err != nil {
		t.Fatalf("readPortAllocationRegistry returned error: %v", err)
	}
	if len(registry.Allocations) != 0 {
		t.Fatalf("allocations after release = %#v, want empty", registry.Allocations)
	}
}

func TestReleaseProjectPortAllocationsKeepsOtherProjects(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	dataDir := t.TempDir()
	if _, err := AllocatePort(context.Background(), dataDir, testPortAllocationRequest()); err != nil {
		t.Fatalf("AllocatePort returned error: %v", err)
	}
	other := testPortAllocationRequest()
	other.Project = "other"
	other.PreferredPort = 31002
	if _, err := AllocatePort(context.Background(), dataDir, other); err != nil {
		t.Fatalf("other AllocatePort returned error: %v", err)
	}
	if err := ReleaseProjectPortAllocations(context.Background(), dataDir, "demo", "production"); err != nil {
		t.Fatalf("ReleaseProjectPortAllocations returned error: %v", err)
	}

	registry, err := readPortAllocationRegistry(portAllocationRegistryPath(dataDir))
	if err != nil {
		t.Fatalf("readPortAllocationRegistry returned error: %v", err)
	}
	if len(registry.Allocations) != 1 {
		t.Fatalf("allocations after release = %#v, want one remaining", registry.Allocations)
	}
	for _, allocation := range registry.Allocations {
		if allocation.Project != "other" {
			t.Fatalf("remaining allocation project = %q, want other", allocation.Project)
		}
	}
}

func TestValidatePortAllocationRequestRejectsInvalidInput(t *testing.T) {
	req := testPortAllocationRequest()
	req.Project = "../demo"
	if err := validatePortAllocationRequest(req); err == nil {
		t.Fatal("expected invalid project to be rejected")
	}

	req = testPortAllocationRequest()
	req.PreferredPort = 20000
	req.MinPort = 30000
	req.MaxPort = 30010
	if err := validatePortAllocationRequest(req); err == nil {
		t.Fatal("expected preferred port outside range to be rejected")
	}
}

func TestParseDockerHostPorts(t *testing.T) {
	got := parseDockerHostPorts(`{"80/tcp":[{"HostIp":"","HostPort":"80"}],"443/tcp":[{"HostIp":"0.0.0.0","HostPort":"443"}]}`)
	if !got[80] || !got[443] || len(got) != 2 {
		t.Fatalf("parseDockerHostPorts = %#v, want 80 and 443", got)
	}
}

func testPortAllocationRequest() PortAllocationRequest {
	return PortAllocationRequest{
		Kind:          PortAllocationKindMeshUpstream,
		Project:       "demo",
		Environment:   "production",
		Service:       "web",
		Slot:          1,
		HostIP:        "10.210.0.1",
		ContainerPort: 3000,
		PreferredPort: 31001,
		MinPort:       30000,
		MaxPort:       32000,
	}
}
