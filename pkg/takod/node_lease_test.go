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

func maintenanceRequest(id string) NodeMaintenanceLeaseRequest {
	return NodeMaintenanceLeaseRequest{ID: id, Operation: "upgrade-servers", Who: "operator@laptop", PID: 42, TTLSeconds: 900}
}

func environmentLeaseRequest(project, environment, id string) LeaseRequest {
	return LeaseRequest{Project: project, Environment: environment, ID: id, Operation: "deploy", Who: "ci@runner", PID: 7, TTLSeconds: 600}
}

func TestNodeMaintenanceLeaseBlocksEnvironmentLeases(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	acquired, err := AcquireNodeMaintenanceLease(ctx, dataDir, maintenanceRequest("maint-1"))
	if err != nil || !acquired.Acquired {
		t.Fatalf("maintenance acquire = %+v, %v", acquired, err)
	}
	response, err := AcquireLease(ctx, dataDir, environmentLeaseRequest("app", "production", "deploy-1"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Acquired {
		t.Fatal("environment lease acquired during node maintenance")
	}
	if response.Lease == nil || !strings.Contains(response.Lease.Operation, "node maintenance (upgrade-servers)") || response.Lease.Who != "operator@laptop" {
		t.Fatalf("blocked response does not name the maintenance operation: %+v", response.Lease)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "leases", "app", "production.json")); !os.IsNotExist(err) {
		t.Fatalf("blocked environment lease left a file: %v", err)
	}

	// Releasing maintenance frees environment leases again.
	if _, err := ReleaseNodeMaintenanceLease(ctx, dataDir, NodeMaintenanceLeaseRequest{ID: "maint-1"}); err != nil {
		t.Fatal(err)
	}
	response, err = AcquireLease(ctx, dataDir, environmentLeaseRequest("app", "production", "deploy-1"))
	if err != nil || !response.Acquired {
		t.Fatalf("environment lease after release = %+v, %v", response, err)
	}
}

func TestLiveEnvironmentLeaseOfAnyProjectBlocksMaintenance(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	for _, request := range []LeaseRequest{
		environmentLeaseRequest("other", "staging", "deploy-a"),
		environmentLeaseRequest("app", "preview", "deploy-b"),
	} {
		if response, err := AcquireLease(ctx, dataDir, request); err != nil || !response.Acquired {
			t.Fatalf("environment lease %s/%s = %+v, %v", request.Project, request.Environment, response, err)
		}
	}
	response, err := AcquireNodeMaintenanceLease(ctx, dataDir, maintenanceRequest("maint-1"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Acquired {
		t.Fatal("maintenance lease granted while environment leases are live")
	}
	var holders []string
	for _, holder := range response.Holders {
		holders = append(holders, holder.ProjectName+"/"+holder.Environment)
	}
	if strings.Join(holders, ",") != "app/preview,other/staging" {
		t.Fatalf("holders = %v", holders)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node-leases", "maintenance.json")); !os.IsNotExist(err) {
		t.Fatalf("refused maintenance lease left a file: %v", err)
	}
}

func TestNodeMaintenanceLeaseExpiryReleasesBothKinds(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	// An expired environment lease does not block maintenance.
	expiredEnvironment := LeaseInfo{ID: "old", ProjectName: "app", Environment: "production", Operation: "deploy", Who: "x", ExpiresAt: time.Now().Add(-time.Minute)}
	writeTestJSON(t, filepath.Join(dataDir, "leases", "app", "production.json"), expiredEnvironment)
	response, err := AcquireNodeMaintenanceLease(ctx, dataDir, maintenanceRequest("maint-1"))
	if err != nil || !response.Acquired {
		t.Fatalf("maintenance over expired environment lease = %+v, %v", response, err)
	}

	// An expired maintenance lease does not block environment leases.
	expiredMaintenance := NodeMaintenanceLease{Version: 1, ID: "maint-1", Operation: "upgrade-servers", Who: "x", ExpiresAt: time.Now().Add(-time.Second)}
	writeTestJSON(t, filepath.Join(dataDir, "node-leases", "maintenance.json"), expiredMaintenance)
	lease, err := AcquireLease(ctx, dataDir, environmentLeaseRequest("app", "production", "deploy-2"))
	if err != nil || !lease.Acquired {
		t.Fatalf("environment lease over expired maintenance = %+v, %v", lease, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node-leases", "maintenance.json")); !os.IsNotExist(err) {
		t.Fatal("expired maintenance lease was not removed")
	}
}

// The v1 file is the contract between a takod and its successor: a lease
// written before an upgrade restart is honored by the restarted agent.
func TestNodeMaintenanceLeaseWrittenBeforeRestartIsHonored(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	frozen := `{
  "version": 1,
  "id": "maint-before-restart",
  "operation": "upgrade-servers",
  "who": "operator@laptop",
  "pid": 99,
  "createdAt": "2026-01-01T00:00:00Z",
  "expiresAt": "` + time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339Nano) + `"
}
`
	path := filepath.Join(dataDir, "node-leases", "maintenance.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(frozen), 0600); err != nil {
		t.Fatal(err)
	}
	response, err := AcquireLease(ctx, dataDir, environmentLeaseRequest("app", "production", "deploy-1"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Acquired {
		t.Fatal("restarted agent ignored the maintenance lease")
	}
	read, err := ReadNodeMaintenanceLease(ctx, dataDir)
	if err != nil || !read.Found || read.Lease.ID != "maint-before-restart" || read.Lease.PID != 99 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if _, err := ReleaseNodeMaintenanceLease(ctx, dataDir, NodeMaintenanceLeaseRequest{ID: "maint-before-restart"}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeMaintenanceLeaseRejectsOtherHoldersAndUnknownFormats(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if response, err := AcquireNodeMaintenanceLease(ctx, dataDir, maintenanceRequest("maint-1")); err != nil || !response.Acquired {
		t.Fatalf("first acquire = %+v, %v", response, err)
	}
	second, err := AcquireNodeMaintenanceLease(ctx, dataDir, maintenanceRequest("maint-2"))
	if err != nil || second.Acquired {
		t.Fatalf("second holder acquire = %+v, %v", second, err)
	}
	if _, err := ReleaseNodeMaintenanceLease(ctx, dataDir, NodeMaintenanceLeaseRequest{ID: "maint-2"}); err == nil {
		t.Fatal("another holder released the maintenance lease")
	}

	writeTestJSON(t, filepath.Join(dataDir, "node-leases", "maintenance.json"), map[string]any{"version": 2, "id": "future"})
	if _, err := AcquireLease(ctx, dataDir, environmentLeaseRequest("app", "production", "deploy-1")); err == nil {
		t.Fatal("unknown maintenance format did not fail closed")
	}
}

func TestStatusAdvertisesNodeMaintenanceLease(t *testing.T) {
	server := &Server{dataDir: t.TempDir()}
	status := server.Status()
	found := false
	for _, capability := range status.Capabilities {
		found = found || capability == CapabilityNodeMaintenanceLeaseV1
	}
	if !found {
		t.Fatalf("capabilities %v lack %s", status.Capabilities, CapabilityNodeMaintenanceLeaseV1)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
