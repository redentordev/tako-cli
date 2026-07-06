package engine

import (
	"context"
	"errors"
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

func TestAcquireRemoteOperationLeasesWithContextCancelledBeforeFanout(t *testing.T) {
	serverNames := []string{"node-a"}
	servers := testLeaseServers(serverNames)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	_, err := AcquireRemoteOperationLeasesWithContext(ctx, servers, serverNames, "deploy", func(context.Context, string, config.ServerConfig) (RemoteLease, error) {
		called = true
		return RemoteLease{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if Classify(err) != ClassCancelled {
		t.Fatalf("Classify(%v) = %d, want ClassCancelled", err, Classify(err))
	}
	if called {
		t.Fatal("acquire function was called after context cancellation")
	}
}

func TestAcquireRemoteOperationLeasesWithContextCancelledDuringFanoutReleasesLeases(t *testing.T) {
	serverNames := []string{"node-a", "node-b"}
	servers := testLeaseServers(serverNames)
	manager := &recordingLeaseManager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	acquiredA := make(chan struct{})
	startedB := make(chan struct{})
	releaseB := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := AcquireRemoteOperationLeasesWithContext(ctx, servers, serverNames, "deploy", func(_ context.Context, serverName string, _ config.ServerConfig) (RemoteLease, error) {
			if serverName == "node-b" {
				close(startedB)
				<-releaseB
			} else {
				close(acquiredA)
			}
			return RemoteLease{
				ServerName: serverName,
				Manager:    manager,
				Lease:      &remotestate.LeaseInfo{ID: "lease-" + serverName},
			}, nil
		})
		errCh <- err
	}()

	waitForClosed(t, acquiredA, "node-a acquisition")
	waitForClosed(t, startedB, "node-b acquisition start")
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled lease acquisition to return")
	}

	waitForReleased(t, manager, "lease-node-a")
	close(releaseB)
	waitForReleased(t, manager, "lease-node-b")
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

func waitForClosed(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForReleased(t *testing.T, manager *recordingLeaseManager, leaseID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		for _, released := range manager.Released() {
			if released == leaseID {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for release of %s; released %v", leaseID, manager.Released())
		case <-time.After(10 * time.Millisecond):
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
