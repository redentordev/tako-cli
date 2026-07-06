package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
)

func TestAcquireRemoteOperationLeasesWithRunsConcurrently(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testLeaseServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	setDone := make(chan *RemoteLeaseSet, 1)
	errDone := make(chan error, 1)
	go func() {
		set, err := AcquireRemoteOperationLeasesWith(servers, serverNames, "deploy", func(serverName string, _ config.ServerConfig) (RemoteLease, error) {
			started <- serverName
			<-release
			return RemoteLease{
				ServerName: serverName,
				Manager:    &recordingLeaseManager{},
				Lease:      &remotestate.LeaseInfo{ID: "lease-" + serverName},
			}, nil
		})
		setDone <- set
		errDone <- err
	}()

	waitForLeaseStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("AcquireRemoteOperationLeasesWith returned error: %v", err)
	}
	set := <-setDone
	if got, want := set.Summary(), "node-a:lease-node-a, node-b:lease-node-b, node-c:lease-node-c"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestAcquireRemoteOperationLeasesWithReleasesOnFailure(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testLeaseServers(serverNames)
	manager := &recordingLeaseManager{}

	_, err := AcquireRemoteOperationLeasesWith(servers, serverNames, "deploy", func(serverName string, _ config.ServerConfig) (RemoteLease, error) {
		if serverName == "node-c" {
			return RemoteLease{}, fmt.Errorf("node-c failed")
		}
		return RemoteLease{
			ServerName: serverName,
			Manager:    manager,
			Lease:      &remotestate.LeaseInfo{ID: "lease-" + serverName},
		}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "node-c failed") {
		t.Fatalf("expected node-c failure, got %v", err)
	}

	released := manager.Released()
	if got, want := strings.Join(released, ","), "lease-node-b,lease-node-a"; got != want {
		t.Fatalf("released leases = %q, want %q", got, want)
	}
}

func TestAcquireRemoteOperationLeasesWithClassifiesLockedFailure(t *testing.T) {
	serverNames := []string{"node-a"}
	servers := testLeaseServers(serverNames)

	_, err := AcquireRemoteOperationLeasesWith(servers, serverNames, "deploy", func(serverName string, _ config.ServerConfig) (RemoteLease, error) {
		return RemoteLease{}, &LockedError{Operation: "deploy", Err: fmt.Errorf("lease held")}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if Classify(err) != ClassLocked {
		t.Fatalf("Classify(%v) = %d, want ClassLocked", err, Classify(err))
	}
}

func waitForLeaseStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for lease acquisition fanout; saw %v", seen)
		}
	}
}

func testLeaseServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name, User: "root"}
	}
	return servers
}

type recordingLeaseManager struct {
	mu       sync.Mutex
	released []string
}

func (m *recordingLeaseManager) ReleaseLease(lease *remotestate.LeaseInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, lease.ID)
	return nil
}

func (m *recordingLeaseManager) Released() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.released...)
}
