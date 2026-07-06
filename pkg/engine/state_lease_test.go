package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
)

type recordingStateLeaseManager struct {
	mu       sync.Mutex
	released []string
}

func (m *recordingStateLeaseManager) ReleaseLease(lease *remotestate.LeaseInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, lease.ID)
	return nil
}

func (m *recordingStateLeaseManager) Released() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.released...)
}

func TestReleaseStateLeaseByIDRequiresForceForActiveLease(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	manager := &recordingStateLeaseManager{}
	nodes := []StateLeaseNodeResult{
		{
			Name:    "node-a",
			Manager: manager,
			Lease: &remotestate.LeaseInfo{
				ID:        "lease-active",
				ExpiresAt: now.Add(time.Minute),
			},
		},
	}

	if _, err := ReleaseStateLeaseByID(nodes, "lease-active", false, now); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected active lease to require --force, got %v", err)
	}
	if released := manager.Released(); len(released) != 0 {
		t.Fatalf("released leases = %#v, want none", released)
	}

	released, err := ReleaseStateLeaseByID(nodes, "lease-active", true, now)
	if err != nil {
		t.Fatalf("ReleaseStateLeaseByID with force returned error: %v", err)
	}
	if strings.Join(released, ",") != "node-a" {
		t.Fatalf("released nodes = %#v, want node-a", released)
	}
	if got := strings.Join(manager.Released(), ","); got != "lease-active" {
		t.Fatalf("released leases = %q, want lease-active", got)
	}
}

func TestReleaseStateLeaseByIDAllowsExpiredLeaseWithoutForce(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	manager := &recordingStateLeaseManager{}
	nodes := []StateLeaseNodeResult{
		{
			Name:    "node-a",
			Manager: manager,
			Lease: &remotestate.LeaseInfo{
				ID:        "lease-expired",
				ExpiresAt: now.Add(-time.Second),
			},
		},
	}

	released, err := ReleaseStateLeaseByID(nodes, "lease-expired", false, now)
	if err != nil {
		t.Fatalf("ReleaseStateLeaseByID returned error: %v", err)
	}
	if strings.Join(released, ",") != "node-a" {
		t.Fatalf("released nodes = %#v, want node-a", released)
	}
}

func TestReleaseStateLeaseByIDReportsMissingReachableLeaseWithUnreachableNodes(t *testing.T) {
	nodes := []StateLeaseNodeResult{
		{Name: "node-a"},
		{Name: "node-b", Err: errors.New("connection refused"), Error: "connection refused"},
	}

	_, err := ReleaseStateLeaseByID(nodes, "missing", true, time.Now())
	if err == nil {
		t.Fatal("ReleaseStateLeaseByID should report missing lease")
	}
	for _, want := range []string{"missing", "not found", "node-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}
