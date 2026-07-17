//go:build !windows

package takod

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
	"golang.org/x/sys/unix"
)

func TestSignedInventoryPublicationRejectsActiveNodeUpgradeLease(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	current, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	advanced := *current
	advanced.Generation++
	advanced.UpdatedAt = time.Now().UTC()
	advanced.Nodes[0].UpdatedAt = advanced.UpdatedAt
	snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: advanced, IssuedAt: time.Now().UTC()}
	if err := nodeidentity.SignInventorySnapshot(&snapshot, server.installation); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	writeActiveNodeUpgradeLease(t, server.dataDir)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/platform/inventory", bytes.NewReader(body))
	server.enrolledLifecycleHandler(http.HandlerFunc(server.handleInventoryAuthority)).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("signed inventory during active upgrade status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	after, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != current.Generation {
		t.Fatalf("signed inventory advanced during node upgrade: generation=%d want=%d", after.Generation, current.Generation)
	}
}

func TestAllocationAuthorizationRejectsActiveNodeUpgradeLease(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	writeActiveNodeUpgradeLease(t, server.dataDir)
	calls := 0
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/platform/allocations/authorize", bytes.NewReader([]byte(`{"phase":"prepare"}`)))
	server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("allocation authorization crossed active node lease: status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
}

func TestLifecycleRequestTakesUpgradeGuardBeforeSnapshotBarrier(t *testing.T) {
	for _, route := range []struct {
		method string
		path   string
	}{{http.MethodGet, "/v1/platform/inventory"}, {http.MethodPost, "/v1/platform/allocations/authorize"}, {http.MethodPost, "/v1/platform/membership/reconcile"}} {
		t.Run(route.path, func(t *testing.T) {
			server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
			releaseSnapshot, err := recovery.AcquireSnapshotLock(server.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			requestDone := make(chan struct{})
			go func() {
				defer close(requestDone)
				request := httptest.NewRequest(route.method, route.path, nil)
				server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request)
			}()

			guardPath := filepath.Join(server.dataDir, "node-upgrade", "locks", ".guard")
			waitForGuardLock(t, guardPath)
			contenderDone := make(chan error, 1)
			go func() {
				release, lockErr := recovery.AcquireNodeUpgradeLifecycleExclusionForDataDir(server.dataDir)
				if lockErr == nil {
					release()
				}
				contenderDone <- lockErr
			}()
			select {
			case lockErr := <-contenderDone:
				t.Fatalf("lifecycle contender bypassed request guard before snapshot release: %v", lockErr)
			case <-time.After(50 * time.Millisecond):
			}

			releaseSnapshot()
			select {
			case <-requestDone:
			case <-time.After(time.Second):
				t.Fatal("guarded lifecycle request did not proceed after snapshot release")
			}
			select {
			case lockErr := <-contenderDone:
				if lockErr != nil {
					t.Fatalf("lifecycle contender failed after ordered request released: %v", lockErr)
				}
			case <-time.After(time.Second):
				t.Fatal("lifecycle contender remained blocked after ordered request released")
			}
		})
	}
}

func writeActiveNodeUpgradeLease(t *testing.T, dataDir string) {
	t.Helper()
	nodeLock := filepath.Join(dataDir, "node-upgrade", "locks", "node")
	if err := os.MkdirAll(nodeLock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeLock, "expiry"), []byte(strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func waitForGuardLock(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == nil {
			lockErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
			if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
				_ = unix.Close(fd)
				return
			}
			if lockErr == nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
			}
			_ = unix.Close(fd)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("lifecycle request did not acquire node upgrade guard")
}
