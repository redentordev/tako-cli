package takod

import (
	"context"
	"strings"
	"testing"
)

func TestAcquireLeaseBlocksConcurrentOwnerAndRelease(t *testing.T) {
	dataDir := t.TempDir()
	request := LeaseRequest{
		Project:     "demo",
		Environment: "production",
		ID:          "lease_1",
		Operation:   "deploy",
		Who:         "tester",
		PID:         123,
		TTLSeconds:  60,
	}

	first, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("AcquireLease returned error: %v", err)
	}
	if !first.Acquired || first.Lease == nil || first.Lease.ID != request.ID {
		t.Fatalf("unexpected first lease response: %#v", first)
	}

	secondReq := request
	secondReq.ID = "lease_2"
	second, err := AcquireLease(context.Background(), dataDir, secondReq)
	if err != nil {
		t.Fatalf("second AcquireLease returned error: %v", err)
	}
	if second.Acquired || second.Lease == nil || second.Lease.ID != request.ID {
		t.Fatalf("unexpected second lease response: %#v", second)
	}

	if _, err := ReleaseLease(context.Background(), dataDir, LeaseRequest{
		Project:     request.Project,
		Environment: request.Environment,
		ID:          "wrong",
	}); err == nil || !strings.Contains(err.Error(), "cannot release") {
		t.Fatalf("expected wrong owner release error, got %v", err)
	}

	released, err := ReleaseLease(context.Background(), dataDir, LeaseRequest{
		Project:     request.Project,
		Environment: request.Environment,
		ID:          request.ID,
	})
	if err != nil {
		t.Fatalf("ReleaseLease returned error: %v", err)
	}
	if released.Found {
		t.Fatalf("expected released lease response to be not found: %#v", released)
	}
}
