package platform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type fakeStatusProbe struct {
	status *takodclient.AgentStatus
	err    error
}

func (p fakeStatusProbe) Status(context.Context) (*takodclient.AgentStatus, error) {
	return p.status, p.err
}

func (p fakeStatusProbe) RequestJSON(context.Context, string, string, any) (string, error) {
	return `{}`, p.err
}

func TestWorkerAttestsControllerAndWritesReadyJournal(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tako-worker-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	installation, err := nodeidentity.New("", "", "node-1", firstNodeRoles, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	document := ConfigDocument{
		State: BootstrapState{
			InventoryPath: DefaultConfigDir + "/cluster-inventory.json",
			APIVersion:    APIVersion, Kind: BootstrapKind, ClusterID: installation.ClusterID,
			NodeID: installation.NodeID, NodeName: installation.NodeName, ControllerMode: "single-writer",
			EnrollmentRoles: installation.EnrollmentRoles, IdentityPath: "/etc/tako/identity.json",
			StateDir: dir, AuditDir: dir, SocketPath: DefaultSocket, WorkerSocketPath: filepath.Join(dir, "worker.sock"),
			DockerDataRoot: "/var/lib/docker",
			SocketGroup:    DefaultSocketGroup, ServiceBinaryPath: DefaultBinaryPath,
			WorkerUID: os.Geteuid(), WorkerGID: os.Getegid(), SocketGroupGID: os.Getegid(),
			WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup, InitializedAt: time.Now(),
		},
		Policy: DefaultResourcePolicy(),
	}
	configPath := filepath.Join(dir, "platform.json")
	data, _ := json.Marshal(document)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	status := &takodclient.AgentStatus{
		Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &installation.Identity,
	}
	worker, err := NewWorker(configPath, dir, DefaultSocket, fakeStatusProbe{status: status}, fixedDiskProbe{available: document.Policy.MinimumFreeDiskBytes + 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	journalPath := filepath.Join(dir, DefaultJournalName)
	deadline := time.Now().Add(time.Second)
	for {
		data, readErr := os.ReadFile(journalPath)
		if readErr == nil && strings.Contains(string(data), `"phase":"ready"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not write ready journal: %v", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("worker exit = %v", err)
	}
}

func TestWorkerRejectsWrongLocalIdentity(t *testing.T) {
	dir := t.TempDir()
	expected, _ := nodeidentity.New("", "", "node-1", firstNodeRoles, time.Now())
	wrong, _ := nodeidentity.New("", "", "node-2", []string{nodeidentity.RoleWorker}, time.Now())
	document := ConfigDocument{
		State: BootstrapState{
			InventoryPath: DefaultConfigDir + "/cluster-inventory.json",
			APIVersion:    APIVersion, Kind: BootstrapKind, ClusterID: expected.ClusterID, NodeID: expected.NodeID,
			NodeName: expected.NodeName, ControllerMode: "single-writer", EnrollmentRoles: expected.EnrollmentRoles,
			IdentityPath: "/etc/tako/identity.json", StateDir: dir, AuditDir: dir, SocketPath: DefaultSocket, WorkerSocketPath: filepath.Join(dir, "worker.sock"),
			DockerDataRoot: "/var/lib/docker",
			SocketGroup:    DefaultSocketGroup, ServiceBinaryPath: DefaultBinaryPath,
			WorkerUID: os.Geteuid(), WorkerGID: os.Getegid(), SocketGroupGID: os.Getegid(),
			WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup, InitializedAt: time.Now(),
		}, Policy: DefaultResourcePolicy(),
	}
	data, _ := json.Marshal(document)
	configPath := filepath.Join(dir, "platform.json")
	_ = os.WriteFile(configPath, data, 0600)
	worker, _ := NewWorker(configPath, dir, DefaultSocket, fakeStatusProbe{status: &takodclient.AgentStatus{
		Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &wrong.Identity,
	}}, fixedDiskProbe{available: document.Policy.MinimumFreeDiskBytes + 1})
	if err := worker.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("worker error = %v", err)
	}
}

func TestWorkerRejectsRuntimePathMismatch(t *testing.T) {
	dir := t.TempDir()
	installation, _ := nodeidentity.New("", "", "node-1", firstNodeRoles, time.Now())
	document := ConfigDocument{State: BootstrapState{
		InventoryPath: DefaultConfigDir + "/cluster-inventory.json",
		APIVersion:    APIVersion, Kind: BootstrapKind, ClusterID: installation.ClusterID, NodeID: installation.NodeID,
		NodeName: installation.NodeName, ControllerMode: "single-writer", EnrollmentRoles: installation.EnrollmentRoles,
		IdentityPath: "/etc/tako/identity.json", StateDir: dir, AuditDir: dir, SocketPath: DefaultSocket, WorkerSocketPath: filepath.Join(dir, "worker.sock"),
		DockerDataRoot: "/var/lib/docker",
		SocketGroup:    DefaultSocketGroup, ServiceBinaryPath: DefaultBinaryPath,
		WorkerUID: os.Geteuid(), WorkerGID: os.Getegid(), SocketGroupGID: os.Getegid(),
		WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup, InitializedAt: time.Now(),
	}, Policy: DefaultResourcePolicy()}
	data, _ := json.Marshal(document)
	configPath := filepath.Join(dir, "platform.json")
	_ = os.WriteFile(configPath, data, 0600)
	status := &takodclient.AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &installation.Identity}
	for _, test := range []struct{ state, socket string }{{state: dir + "-other", socket: DefaultSocket}, {state: dir, socket: "/run/tako/other.sock"}} {
		worker, _ := NewWorker(configPath, test.state, test.socket, fakeStatusProbe{status: status}, fixedDiskProbe{available: document.Policy.MinimumFreeDiskBytes + 1})
		if err := worker.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("worker mismatch error = %v", err)
		}
	}
}
