package takod

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOnDemandBackupRetentionPrunesLocalArchives(t *testing.T) {
	var got []BackupRequest
	cleanup := func(_ context.Context, req BackupRequest) (*BackupCleanupResponse, error) {
		got = append(got, req)
		return &BackupCleanupResponse{Deleted: 3}, nil
	}
	info := &BackupInfo{ID: "20261010-104940-abc"}
	applyOnDemandBackupRetention(context.Background(), BackupRequest{
		Project:       "demo",
		Environment:   "production",
		Volume:        "mongo_data",
		DockerVolume:  "demo_production_mongo_data",
		BackupID:      "20261010-104940-abc",
		RetentionDays: 14,
	}, info, cleanup)

	if len(got) != 1 {
		t.Fatalf("cleanup calls = %d, want 1", len(got))
	}
	want := BackupRequest{Project: "demo", Environment: "production", Volume: "mongo_data", RetentionDays: 14}
	if got[0] != want {
		t.Fatalf("cleanup request = %#v, want %#v", got[0], want)
	}
	if len(info.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", info.Warnings)
	}
}

func TestOnDemandBackupRetentionSkipsWithoutRetention(t *testing.T) {
	called := false
	cleanup := func(context.Context, BackupRequest) (*BackupCleanupResponse, error) {
		called = true
		return &BackupCleanupResponse{}, nil
	}
	applyOnDemandBackupRetention(context.Background(), BackupRequest{Project: "demo", Environment: "production", Volume: "data"}, &BackupInfo{}, cleanup)
	applyOnDemandBackupRetention(context.Background(), BackupRequest{Project: "demo", Environment: "production", Volume: "data", RetentionDays: 7}, nil, cleanup)
	if called {
		t.Fatal("retention cleanup ran without a retention policy or a created backup")
	}
}

func TestOnDemandBackupRetentionFailureIsAWarning(t *testing.T) {
	cleanup := func(context.Context, BackupRequest) (*BackupCleanupResponse, error) {
		return nil, errors.New("directory locked")
	}
	info := &BackupInfo{Warnings: []string{"existing"}}
	applyOnDemandBackupRetention(context.Background(), BackupRequest{Project: "demo", Environment: "production", Volume: "data", RetentionDays: 7}, info, cleanup)
	if len(info.Warnings) != 2 || !strings.Contains(info.Warnings[1], "local backup retention cleanup failed: directory locked") {
		t.Fatalf("warnings = %v", info.Warnings)
	}
}
