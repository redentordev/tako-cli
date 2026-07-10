package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAcquireLeaseReplacesExpiredLease(t *testing.T) {
	dataDir := t.TempDir()
	request := LeaseRequest{
		Project:     "demo",
		Environment: "production",
		ID:          "lease_new",
		Operation:   "deploy",
		Who:         "tester",
		TTLSeconds:  60,
	}
	path, err := leasePath(dataDir, request.Project, request.Environment)
	if err != nil {
		t.Fatalf("leasePath returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create lease dir: %v", err)
	}
	expired := LeaseInfo{
		ID:          "lease_old",
		ProjectName: request.Project,
		Environment: request.Environment,
		Operation:   "deploy",
		Who:         "old-owner",
		CreatedAt:   time.Now().UTC().Add(-2 * time.Hour),
		ExpiresAt:   time.Now().UTC().Add(-time.Hour),
	}
	data, err := json.Marshal(expired)
	if err != nil {
		t.Fatalf("failed to marshal expired lease: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("failed to write expired lease: %v", err)
	}

	response, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("AcquireLease returned error: %v", err)
	}
	if !response.Acquired || response.Lease == nil || response.Lease.ID != request.ID {
		t.Fatalf("unexpected acquired lease response: %#v", response)
	}
}

func TestAcquireLeaseSameIDRenewsWithoutChangingHolderMetadata(t *testing.T) {
	dataDir := t.TempDir()
	request := LeaseRequest{
		Project:     "demo",
		Environment: "production",
		ID:          "lease_1",
		Operation:   "deploy",
		Who:         "original-owner",
		PID:         123,
		TTLSeconds:  60,
	}
	first, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("AcquireLease returned error: %v", err)
	}

	renew := request
	renew.Operation = "different-operation"
	renew.Who = "different-owner"
	renew.PID = 999
	renew.TTLSeconds = 120
	renewed, err := AcquireLease(context.Background(), dataDir, renew)
	if err != nil {
		t.Fatalf("renewing AcquireLease returned error: %v", err)
	}
	if !renewed.Acquired || renewed.Lease == nil {
		t.Fatalf("unexpected renewal response: %#v", renewed)
	}
	if renewed.Message != "lease renewed" {
		t.Fatalf("renewal message = %q, want lease renewed", renewed.Message)
	}
	if renewed.Lease.CreatedAt != first.Lease.CreatedAt {
		t.Fatalf("renewal changed createdAt: got %s want %s", renewed.Lease.CreatedAt, first.Lease.CreatedAt)
	}
	if renewed.Lease.Operation != request.Operation || renewed.Lease.Who != request.Who || renewed.Lease.PID != request.PID {
		t.Fatalf("renewal changed holder metadata: %#v", renewed.Lease)
	}
	if !renewed.Lease.ExpiresAt.After(first.Lease.ExpiresAt) {
		t.Fatalf("renewed expiry %s is not after original %s", renewed.Lease.ExpiresAt, first.Lease.ExpiresAt)
	}

	onDisk, err := ReadLease(context.Background(), dataDir, LeaseRequest{Project: request.Project, Environment: request.Environment})
	if err != nil {
		t.Fatalf("ReadLease returned error: %v", err)
	}
	if !onDisk.Found || onDisk.Lease == nil || onDisk.Lease.ExpiresAt != renewed.Lease.ExpiresAt {
		t.Fatalf("on-disk lease does not contain renewed expiry: %#v", onDisk)
	}
}

func TestExplicitLeaseRenewalNeverShortensExpiry(t *testing.T) {
	dataDir := t.TempDir()
	request := LeaseRequest{
		Project: "demo", Environment: "production", ID: "lease_1",
		Operation: "deploy", Who: "tester", TTLSeconds: 3600,
	}
	first, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("AcquireLease returned error: %v", err)
	}
	request.Renew = true
	request.TTLSeconds = 60
	renewed, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("renewing AcquireLease returned error: %v", err)
	}
	if !renewed.Acquired || renewed.Lease == nil {
		t.Fatalf("unexpected renewal response: %#v", renewed)
	}
	if renewed.Lease.ExpiresAt != first.Lease.ExpiresAt {
		t.Fatalf("short renewal changed expiry: got %s want %s", renewed.Lease.ExpiresAt, first.Lease.ExpiresAt)
	}
}

func TestExplicitLeaseRenewalRejectsExpiredHolderWithoutReacquiring(t *testing.T) {
	dataDir := t.TempDir()
	request := LeaseRequest{
		Project: "demo", Environment: "production", ID: "lease_1",
		Operation: "deploy", Who: "tester", TTLSeconds: 60, Renew: true,
	}
	path, err := leasePath(dataDir, request.Project, request.Environment)
	if err != nil {
		t.Fatalf("leasePath returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create lease dir: %v", err)
	}
	expired := LeaseInfo{
		ID: request.ID, ProjectName: request.Project, Environment: request.Environment,
		Operation: request.Operation, Who: request.Who,
		CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}
	data, err := json.Marshal(expired)
	if err != nil {
		t.Fatalf("failed to marshal lease: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("failed to write lease: %v", err)
	}

	response, err := AcquireLease(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("renewing expired lease returned error: %v", err)
	}
	if response.Acquired || !response.Found || response.Message != "lease expired" {
		t.Fatalf("unexpected expired renewal response: %#v", response)
	}
	onDisk, err := readLeaseFile(path)
	if err != nil {
		t.Fatalf("readLeaseFile returned error: %v", err)
	}
	if onDisk == nil || !onDisk.ExpiresAt.Equal(expired.ExpiresAt) {
		t.Fatalf("expired renewal changed the holder: %#v", onDisk)
	}
}

func TestValidateLeaseRequestRejectsUnsafeAcquireFields(t *testing.T) {
	base := LeaseRequest{
		Project:     "demo",
		Environment: "production",
		ID:          "lease_1",
		Operation:   "deploy",
		Who:         "tester",
		TTLSeconds:  60,
	}
	tests := []struct {
		name   string
		mutate func(*LeaseRequest)
		want   string
	}{
		{
			name: "owner control character",
			mutate: func(req *LeaseRequest) {
				req.Who = "tester\nbad"
			},
			want: "invalid lease owner",
		},
		{
			name: "negative pid",
			mutate: func(req *LeaseRequest) {
				req.PID = -1
			},
			want: "invalid lease PID",
		},
		{
			name: "oversized ttl",
			mutate: func(req *LeaseRequest) {
				req.TTLSeconds = int64(maxLeaseTTL/time.Second) + 1
			},
			want: "invalid lease TTL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			err := validateLeaseRequest(req, true)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}
