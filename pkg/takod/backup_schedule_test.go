package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackupSchedulerUpsertPersistsSecureSchedule(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewBackupScheduler(dataDir)
	request := testBackupScheduleRequest()

	response, err := scheduler.Upsert(context.Background(), request)
	if err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	if !response.Scheduled {
		t.Fatalf("response = %#v, want scheduled", response)
	}

	path := backupSchedulePath(dataDir, request)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("schedule file missing: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("schedule file mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read schedule: %v", err)
	}
	var stored BackupScheduleRequest
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("stored schedule is invalid JSON: %v", err)
	}
	if stored.Storage == nil || stored.Storage.SecretAccessKey != "secret" {
		t.Fatalf("stored schedule did not preserve storage credentials")
	}
}

func TestBackupSchedulerDeleteRemovesSchedule(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewBackupScheduler(dataDir)
	request := testBackupScheduleRequest()
	if _, err := scheduler.Upsert(context.Background(), request); err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}

	response, err := scheduler.Delete(context.Background(), request.Project, request.Environment, request.Service)
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if response.Scheduled {
		t.Fatalf("response = %#v, want unscheduled", response)
	}
	if _, err := os.Stat(backupSchedulePath(dataDir, request)); !os.IsNotExist(err) {
		t.Fatalf("schedule file should be removed, stat err=%v", err)
	}
}

func TestBackupSchedulerRemoveProjectUnschedulesScope(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewBackupScheduler(dataDir)
	production := testBackupScheduleRequest()
	staging := production
	staging.Environment = "staging"
	if _, err := scheduler.Upsert(context.Background(), production); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Upsert(context.Background(), staging); err != nil {
		t.Fatal(err)
	}

	removed, err := scheduler.RemoveProject("demo", "production")
	if err != nil {
		t.Fatalf("RemoveProject returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed schedules = %d, want 1", removed)
	}
	if _, ok := scheduler.entries[backupScheduleKey(production)]; ok {
		t.Fatal("production schedule remains registered")
	}
	if _, ok := scheduler.entries[backupScheduleKey(staging)]; !ok {
		t.Fatal("staging schedule was removed")
	}
	if _, err := os.Stat(backupSchedulePath(dataDir, production)); !os.IsNotExist(err) {
		t.Fatalf("production schedule file remains: %v", err)
	}
}

func TestBackupSchedulerProjectDrainWaitsForRunningBackup(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	key := "demo/production/postgres"
	scheduler.mu.Lock()
	scheduler.running[key] = true
	scheduler.mu.Unlock()
	drainReady := make(chan func(), 1)
	go func() {
		release, err := scheduler.BeginProjectDrain(context.Background(), "demo", "production")
		if err != nil {
			drainReady <- nil
			return
		}
		drainReady <- release
	}()
	select {
	case <-drainReady:
		t.Fatal("project drain returned before running backup completed")
	case <-time.After(50 * time.Millisecond):
	}
	scheduler.mu.Lock()
	delete(scheduler.running, key)
	scheduler.notifyRunningChangedLocked()
	scheduler.mu.Unlock()
	releaseDrain := <-drainReady
	if releaseDrain == nil {
		t.Fatal("BeginProjectDrain returned error")
	}
	releaseDrain()
}

func TestBackupSchedulerProjectWideDrainIsExclusiveAndBlocksUpsert(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	wait, release, err := scheduler.StartProjectDrain("demo", "")
	if err != nil {
		t.Fatalf("StartProjectDrain returned error: %v", err)
	}
	defer release()
	if err := wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scheduler.StartProjectDrain("demo", "production"); err == nil {
		t.Fatal("overlapping backup drain was accepted")
	}
	if _, err := scheduler.Upsert(context.Background(), testBackupScheduleRequest()); err == nil || !strings.Contains(err.Error(), "cleanup is draining") {
		t.Fatalf("Upsert error = %v, want draining rejection", err)
	}
}

func TestBackupSchedulerIgnoresQueuedCallbackAfterRemoval(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewBackupScheduler(dataDir)
	request := testBackupScheduleRequest()
	if _, err := scheduler.Upsert(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	key := backupScheduleKey(request)
	generation := scheduler.generations[key]
	var attempts atomic.Int32
	scheduler.admit = func(...string) error {
		attempts.Add(1)
		return fmt.Errorf("should not run")
	}
	if _, err := scheduler.RemoveProject(request.Project, request.Environment); err != nil {
		t.Fatal(err)
	}
	scheduler.runScheduledBackup(request, generation)
	if attempts.Load() != 0 {
		t.Fatal("removed backup's queued callback executed")
	}
}

func TestBackupSchedulerLoaderHonorsDurableDrainMarker(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewBackupScheduler(dataDir)
	request := testBackupScheduleRequest()
	if err := scheduler.persistSchedule(request); err != nil {
		t.Fatal(err)
	}
	if err := writeSchedulerDrainMarker(dataDir, "backups", request.Project, request.Environment); err != nil {
		t.Fatal(err)
	}
	reloaded := NewBackupScheduler(dataDir)
	if err := reloaded.loadSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.entries) != 0 {
		t.Fatal("loader resurrected backup protected by durable drain marker")
	}
	if _, err := reloaded.Upsert(context.Background(), request); err == nil || !strings.Contains(err.Error(), "durable project cleanup") {
		t.Fatalf("durable drain admitted backup mutation: %v", err)
	}
}

func TestHandleBackupScheduleRejectsInvalidRequest(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/backup-schedule", bytes.NewBufferString(`{"project":"demo"}`))
	recorder := httptest.NewRecorder()

	server.handleBackupSchedule(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "invalid environment name") {
		t.Fatalf("unexpected response: %q", recorder.Body.String())
	}
}

func TestHandleBackupScheduleUpsertAndDelete(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer("/tmp/takod-test.sock", dataDir, "test")
	body, err := json.Marshal(testBackupScheduleRequest())
	if err != nil {
		t.Fatalf("failed to encode request: %v", err)
	}

	upsertReq := httptest.NewRequest(http.MethodPut, "/v1/backup-schedule", bytes.NewReader(body))
	upsertRecorder := httptest.NewRecorder()
	server.handleBackupSchedule(upsertRecorder, upsertReq)
	if upsertRecorder.Code != http.StatusOK {
		t.Fatalf("expected upsert 200, got %d: %s", upsertRecorder.Code, upsertRecorder.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/backup-schedule?project=demo&environment=production&service=postgres", nil)
	deleteRecorder := httptest.NewRecorder()
	server.handleBackupSchedule(deleteRecorder, deleteReq)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("expected delete 200, got %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, backupScheduleDirName, "demo", "production", "postgres.json")); !os.IsNotExist(err) {
		t.Fatalf("schedule file should be removed, stat err=%v", err)
	}
}

func testBackupScheduleRequest() BackupScheduleRequest {
	return BackupScheduleRequest{
		Project:       "demo",
		Environment:   "production",
		Service:       "postgres",
		Schedule:      "@daily",
		RetentionDays: 14,
		Volumes: []BackupScheduleVolume{{
			Volume:       "pgdata",
			DockerVolume: "tako_demo_production_pgdata_123",
		}},
		Storage: &BackupStorageConfig{
			Provider:        BackupStorageProviderS3,
			Bucket:          "backups",
			Region:          "us-east-1",
			AccessKeyID:     "access",
			SecretAccessKey: "secret",
		},
	}
}
