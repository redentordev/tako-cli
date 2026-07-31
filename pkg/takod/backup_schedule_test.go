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

func TestBackupSchedulerPersistsFailedAttemptForAlertReplay(t *testing.T) {
	dataDir := t.TempDir()
	manager := NewAlertManager(dataDir)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{BackupMaxAgeSeconds: 60})); err != nil {
		t.Fatal(err)
	}
	scheduler := NewBackupScheduler(dataDir)
	scheduler.alerts = manager
	request := testBackupScheduleRequest()
	if err := manager.RegisterSubject(request.Project, request.Environment, "backup", request.Service); err != nil {
		t.Fatal(err)
	}
	scheduler.admit = func(...string) error { return fmt.Errorf("low disk") }
	scheduler.runScheduledBackup(request)
	records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != JobRunStatusFailed || records[0].ID == "" {
		t.Fatalf("records = %#v", records)
	}
	now = now.Add(2 * time.Minute)
	if err := manager.Evaluate(); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status(request.Project, request.Environment)
	if status.ActiveEpisodes != 1 || status.Subjects[0].Reasons[0] != "backup_stale" {
		t.Fatalf("alert status = %#v", status)
	}
}

func TestBackupSchedulerRecoversDurableRunningAttempt(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{BackupMaxAgeSeconds: 60})); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "backup", "postgres"); err != nil {
		t.Fatal(err)
	}
	scheduler := NewBackupScheduler(dir)
	scheduler.alerts = manager
	record := BackupRunRecord{ID: "running-backup", Project: "demo", Environment: "production", Service: "postgres", BackupID: "backup-1", Containers: []string{"tako_demo_production_postgres_backup_running_1"}, StartedAt: time.Now().Add(-time.Minute).UTC(), Status: JobRunStatusRunning}
	record.Containers = []string{scheduledBackupContainerName(BackupScheduleRequest{Project: record.Project, Environment: record.Environment, Service: record.Service}, record.ID, 0)}
	if err := scheduler.beginBackupRunRecord(record); err != nil {
		t.Fatal(err)
	}
	recovered := 0
	scheduler.recoverInterrupted = func(_ context.Context, got BackupRunRecord) error {
		recovered++
		if got.Containers[0] != record.Containers[0] {
			t.Fatalf("recovered record = %#v", got)
		}
		return nil
	}
	if err := scheduler.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := scheduler.readBackupRunRecords("demo", "production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || len(records) != 1 || records[0].ID != record.ID || records[0].Status != JobRunStatusFailed || records[0].FinishedAt.IsZero() {
		t.Fatalf("records = %#v", records)
	}
}

func TestBackupRunningAttemptSurvivesBoundedOverlapHistory(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	started := time.Now().Add(-time.Hour).UTC()
	running := BackupRunRecord{ID: "running-backup", Project: "demo", Environment: "production", Service: "postgres", BackupID: "backup-1", Containers: []string{"tako_demo_production_postgres_backup_running_1"}, StartedAt: started, Status: JobRunStatusRunning}
	running.Containers = []string{scheduledBackupContainerName(BackupScheduleRequest{Project: running.Project, Environment: running.Environment, Service: running.Service}, running.ID, 0)}
	if err := scheduler.beginBackupRunRecord(running); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxBackupRunRecords+5; index++ {
		finished := started.Add(time.Duration(index+1) * time.Second)
		if err := scheduler.appendBackupRunRecord(BackupRunRecord{ID: fmt.Sprintf("skip-%d", index), Project: "demo", Environment: "production", Service: "postgres", StartedAt: finished, FinishedAt: finished, Status: JobRunStatusSkipped}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := scheduler.readBackupRunRecords("demo", "production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		found = found || record.ID == running.ID
	}
	if !found || len(records) != maxBackupRunRecords {
		t.Fatalf("running attempt was evicted: len=%d records=%#v", len(records), records)
	}
	scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { return nil }
	if err := scheduler.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ = scheduler.readBackupRunRecords("demo", "production", "postgres")
	for _, record := range records {
		if record.ID == running.ID && record.Status != JobRunStatusFailed {
			t.Fatalf("running attempt was not recovered: %#v", record)
		}
	}
}

func TestBackupSchedulerReportsRecoveryStartupFailure(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	record := BackupRunRecord{ID: "running-backup", Project: "demo", Environment: "production", Service: "postgres", Containers: []string{"tako_demo_production_postgres_backup_running_1"}, StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}
	record.Containers = []string{scheduledBackupContainerName(BackupScheduleRequest{Project: record.Project, Environment: record.Environment, Service: record.Service}, record.ID, 0)}
	if err := scheduler.beginBackupRunRecord(record); err != nil {
		t.Fatal(err)
	}
	scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { return fmt.Errorf("docker unavailable") }
	scheduler.Run(context.Background())
	if err := scheduler.StartupError(); err == nil || !strings.Contains(err.Error(), "docker unavailable") {
		t.Fatalf("StartupError() = %v", err)
	}
}

func TestBackupSchedulerRejectsMismatchedRecoveryIdentity(t *testing.T) {
	dir := t.TempDir()
	scheduler := NewBackupScheduler(dir)
	record := BackupRunRecord{ID: "running-backup", Project: "other", Environment: "production", Service: "postgres", Containers: []string{"tako_other_production_postgres_backup_running_1"}, StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}
	record.Containers = []string{scheduledBackupContainerName(BackupScheduleRequest{Project: record.Project, Environment: record.Environment, Service: record.Service}, record.ID, 0)}
	path := backupRunsPath(dir, "demo", "production", "postgres")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]BackupRunRecord{record})
	if err := writeFileAtomic(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { called = true; return nil }
	if err := scheduler.recoverInterruptedRuns(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid backup run history") {
		t.Fatalf("recoverInterruptedRuns() error = %v", err)
	}
	if called {
		t.Fatal("recovery acted on mismatched record")
	}
	if _, err := os.Stat(backupRunsPath(dir, "other", "production", "postgres")); !os.IsNotExist(err) {
		t.Fatalf("mismatched history path was created: %v", err)
	}
}

func TestRecoverInterruptedBackupRemovesNamedContainer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	record := BackupRunRecord{ID: "running-backup", Project: "demo", Environment: "production", Service: "postgres"}
	container := scheduledBackupContainerName(BackupScheduleRequest{Project: record.Project, Environment: record.Environment, Service: record.Service}, record.ID, 0)
	record.Containers = []string{container}
	t.Setenv("TAKO_FAKE_PS_OUTPUT", container+"\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS", `{"tako.project":"demo","tako.environment":"production","tako.service":"postgres","tako.runtime":"takod","tako.role":"scheduled-backup","tako.attempt":"running-backup"}`)
	if err := recoverInterruptedBackupContainers(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "ps -a --filter name=^"+container+"$") || !strings.Contains(commands, "rm -f "+container) {
		t.Fatalf("recovery commands:\n%s", commands)
	}
}

func TestBackupSchedulerPersistsRunningAttemptBeforeBackup(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	request := testBackupScheduleRequest()
	request.Storage = nil
	scheduler.createBackup = func(_ context.Context, backupRequest BackupRequest) (*BackupInfo, error) {
		records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Status != JobRunStatusRunning || records[0].BackupID == "" || backupRequest.ContainerName == "" || records[0].Containers[0] != backupRequest.ContainerName {
			t.Fatalf("records during backup = %#v", records)
		}
		return &BackupInfo{}, nil
	}
	scheduler.cleanupBackups = func(context.Context, BackupRequest) (*BackupCleanupResponse, error) {
		return &BackupCleanupResponse{}, nil
	}
	scheduler.runScheduledBackup(request)
	records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != JobRunStatusSucceeded || len(records[0].Containers) != 0 {
		t.Fatalf("records = %#v", records)
	}
}

func TestBackupSchedulerCancellationStopsAndWaitsForCronRun(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	request := testBackupScheduleRequest()
	request.Schedule = "@every 10ms"
	request.Storage = nil
	if _, err := scheduler.Upsert(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	scheduler.createBackup = func(ctx context.Context, _ BackupRequest) (*BackupInfo, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return &BackupInfo{}, nil
	}
	scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scheduler.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled backup did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not wait for canceled backup")
	}
	select {
	case <-finished:
	default:
		t.Fatal("scheduler returned before backup finished")
	}
	records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil || len(records) != 1 || records[0].Status != JobRunStatusFailed {
		t.Fatalf("canceled backup records = %#v, err=%v", records, err)
	}
}

func TestBackupCancellationKeepsRunningRecordWhenHelperCleanupFails(t *testing.T) {
	scheduler := NewBackupScheduler(t.TempDir())
	request := testBackupScheduleRequest()
	request.Storage = nil
	started := make(chan struct{})
	scheduler.createBackup = func(ctx context.Context, _ BackupRequest) (*BackupInfo, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { return fmt.Errorf("docker unavailable") }
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.runContext = ctx
	done := make(chan struct{})
	go func() { scheduler.runScheduledBackup(request); close(done) }()
	<-started
	cancel()
	<-done
	records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil || len(records) != 1 || records[0].Status != JobRunStatusRunning || len(records[0].Containers) == 0 {
		t.Fatalf("durable records = %#v, err=%v", records, err)
	}
}

func TestBackupSchedulerDoesNotMarkUnverifiedRemoteBackupSuccessful(t *testing.T) {
	dir := t.TempDir()
	scheduler := NewBackupScheduler(dir)
	scheduler.createBackup = func(context.Context, BackupRequest) (*BackupInfo, error) {
		return &BackupInfo{Warnings: []string{"local backup created but upload failed"}}, nil
	}
	scheduler.cleanupBackups = func(context.Context, BackupRequest) (*BackupCleanupResponse, error) {
		return &BackupCleanupResponse{}, nil
	}
	request := testBackupScheduleRequest()
	scheduler.runScheduledBackup(request)
	records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != JobRunStatusFailed {
		t.Fatalf("records = %#v", records)
	}
}

func TestSchedulerDeletionRejectsRunningAttempts(t *testing.T) {
	jobScheduler := NewJobScheduler(t.TempDir())
	jobSpec := validJobSpecFixture()
	jobKey := jobKey(jobSpec.Project, jobSpec.Environment, jobSpec.Name)
	jobScheduler.specs[jobKey] = jobSpec
	jobScheduler.running[jobKey] = true
	if err := jobScheduler.removeJob(jobSpec.Project, jobSpec.Environment, jobSpec.Name); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("removeJob() error = %v", err)
	}
	if _, exists := jobScheduler.specs[jobKey]; !exists {
		t.Fatal("running job was removed")
	}
	delete(jobScheduler.running, jobKey)
	finished := time.Now().UTC()
	jobScheduler.appendRunRecord(JobRunRecord{Project: jobSpec.Project, Environment: jobSpec.Environment, Job: jobSpec.Name, Trigger: JobTriggerSchedule, StartedAt: finished, FinishedAt: finished, Status: JobRunStatusFailed})
	if err := jobScheduler.removeJob(jobSpec.Project, jobSpec.Environment, jobSpec.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := jobScheduler.Apply(context.Background(), JobsApplyRequest{Project: jobSpec.Project, Environment: jobSpec.Environment, Jobs: []JobSpec{jobSpec}}); err != nil {
		t.Fatal(err)
	}
	jobRuns, err := jobScheduler.Runs(jobSpec.Project, jobSpec.Environment, jobSpec.Name)
	if err != nil || len(jobRuns) != 0 {
		t.Fatalf("re-added job replayed removed runs: %#v, %v", jobRuns, err)
	}

	backupScheduler := NewBackupScheduler(t.TempDir())
	request := testBackupScheduleRequest()
	if _, err := backupScheduler.Upsert(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	backupScheduler.running[backupScheduleKey(request)] = true
	if _, err := backupScheduler.Delete(context.Background(), request.Project, request.Environment, request.Service); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, exists := backupScheduler.schedules[backupScheduleKey(request)]; !exists {
		t.Fatal("running backup schedule was removed")
	}
	delete(backupScheduler.running, backupScheduleKey(request))
	finished = time.Now().UTC()
	if err := backupScheduler.appendBackupRunRecord(BackupRunRecord{ID: "old-backup", Project: request.Project, Environment: request.Environment, Service: request.Service, StartedAt: finished, FinishedAt: finished, Status: JobRunStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err := backupScheduler.Delete(context.Background(), request.Project, request.Environment, request.Service); err != nil {
		t.Fatal(err)
	}
	if _, err := backupScheduler.Upsert(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	backupRuns, err := backupScheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil || len(backupRuns) != 0 {
		t.Fatalf("re-added backup replayed removed runs: %#v, %v", backupRuns, err)
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
