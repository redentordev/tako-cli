package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/recovery"
)

func TestAlertManagerDeduplicatesEpisodeAndSendsSingleRecovery(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 2})); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	manager.deliver = func(_ context.Context, _, _ string, _ any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}

	observeAlertFact(t, manager, "one", JobRunStatusFailed, now.Add(time.Minute))
	status, _ := manager.Status("demo", "production")
	if status.ActiveEpisodes != 0 || status.PendingDelivery != 0 {
		t.Fatalf("first failure opened an alert: %#v", status)
	}
	observeAlertFact(t, manager, "two", JobRunStatusFailed, now.Add(2*time.Minute))
	observeAlertFact(t, manager, "three", JobRunStatusFailed, now.Add(3*time.Minute))
	status, _ = manager.Status("demo", "production")
	if status.ActiveEpisodes != 1 || status.PendingDelivery != 1 || len(status.Subjects) != 1 {
		t.Fatalf("active status = %#v", status)
	}
	episodeID := status.Subjects[0].EpisodeID
	manager.DispatchDue(context.Background())
	status, _ = manager.Status("demo", "production")
	if status.PendingDelivery != 0 || !status.Subjects[0].Active {
		t.Fatalf("opening delivery status = %#v", status)
	}

	observeAlertFact(t, manager, "four", JobRunStatusSucceeded, now.Add(4*time.Minute))
	status, _ = manager.Status("demo", "production")
	if status.ActiveEpisodes != 0 || status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" || status.Deliveries[0].EpisodeID != episodeID {
		t.Fatalf("recovery status = %#v", status)
	}
	if len(status.Deliveries[0].Reasons) != 1 || status.Deliveries[0].Reasons[0] != "consecutive_failures" {
		t.Fatalf("recovery reasons = %#v", status.Deliveries[0].Reasons)
	}
	manager.DispatchDue(context.Background())
	observeAlertFact(t, manager, "five", JobRunStatusSucceeded, now.Add(5*time.Minute))
	status, _ = manager.Status("demo", "production")
	if status.PendingDelivery != 0 || status.ActiveEpisodes != 0 || status.Subjects[0].EpisodeID != "" || !status.Subjects[0].OpenedAt.IsZero() {
		t.Fatalf("duplicate recovery queued: %#v", status)
	}
}

func TestSchedulerPreflightLocksHonorCancellation(t *testing.T) {
	backupRestoreBarrierMu.Lock()
	defer backupRestoreBarrierMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	barrierDone := make(chan error, 1)
	go func() {
		_, _, err := acquireBackupRestoreMutationBarrierContext(ctx)
		barrierDone <- err
	}()
	cancel()
	select {
	case err := <-barrierDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("barrier error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("barrier acquisition ignored cancellation")
	}
	dataDir := t.TempDir()
	releaseSnapshot, err := recovery.AcquireSnapshotLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSnapshot()
	lockCtx, cancelLock := context.WithCancel(context.Background())
	cancelLock()
	if _, err := recovery.AcquireMutationLockContext(lockCtx, dataDir); !errors.Is(err, context.Canceled) {
		t.Fatalf("mutation lock error = %v", err)
	}
}

func TestAlertDispatchAndOverlapFactsWaitForSnapshotAuthority(t *testing.T) {
	t.Run("alert dispatch", func(t *testing.T) {
		dir := t.TempDir()
		manager := NewAlertManager(dir)
		now := time.Now().UTC()
		manager.now = func() time.Time { return now }
		if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
			t.Fatal(err)
		}
		observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
		manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
			return alertDeliveryResult{Interrupted: true}
		}
		release, err := recovery.AcquireSnapshotLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { manager.DispatchDue(context.Background()); close(done) }()
		time.Sleep(100 * time.Millisecond)
		manager.mu.Lock()
		started := manager.states[alertScopeKey("demo", "production")].Deliveries[0].DispatchStarted
		manager.mu.Unlock()
		if started {
			release()
			t.Fatal("dispatch marker changed during exclusive snapshot")
		}
		release()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("dispatch did not resume after snapshot")
		}
	})

	t.Run("job overlap", func(t *testing.T) {
		dir := t.TempDir()
		scheduler := NewJobScheduler(dir)
		spec := JobSpec{Project: "demo", Environment: "production", Name: "worker"}
		key := jobKey(spec.Project, spec.Environment, spec.Name)
		scheduler.specs[key] = spec
		scheduler.running[key] = true
		release, err := recovery.AcquireSnapshotLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { scheduler.runScheduledJob(key); close(done) }()
		time.Sleep(100 * time.Millisecond)
		records, err := scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
		if err != nil || len(records) != 0 {
			release()
			t.Fatalf("overlap mutated history during snapshot: %#v, %v", records, err)
		}
		release()
		<-done
		records, err = scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
		if err != nil || len(records) != 1 || records[0].Status != JobRunStatusSkipped {
			t.Fatalf("overlap record = %#v, %v", records, err)
		}
	})

	t.Run("backup overlap", func(t *testing.T) {
		dir := t.TempDir()
		scheduler := NewBackupScheduler(dir)
		request := testBackupScheduleRequest()
		key := backupScheduleKey(request)
		scheduler.generations[key] = 1
		scheduler.running[key] = true
		release, err := recovery.AcquireSnapshotLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { scheduler.runScheduledBackup(request, 1); close(done) }()
		time.Sleep(100 * time.Millisecond)
		records, err := scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
		if err != nil || len(records) != 0 {
			release()
			t.Fatalf("overlap mutated history during snapshot: %#v, %v", records, err)
		}
		release()
		<-done
		records, err = scheduler.readBackupRunRecords(request.Project, request.Environment, request.Service)
		if err != nil || len(records) != 1 || records[0].Status != JobRunStatusSkipped {
			t.Fatalf("overlap record = %#v, %v", records, err)
		}
	})
}

func TestSchedulerStartupRecoveryWaitsForSnapshotAuthority(t *testing.T) {
	t.Run("jobs", func(t *testing.T) {
		dir := t.TempDir()
		scheduler := NewJobScheduler(dir)
		record := JobRunRecord{ID: "startup-job", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule, StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}
		record.Container = scheduledJobContainerName(record.Project, record.Environment, record.Job, record.ID)
		if err := scheduler.beginRunRecord(record); err != nil {
			t.Fatal(err)
		}
		scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return nil }
		release, err := recovery.AcquireSnapshotLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { scheduler.Run(ctx); close(done) }()
		select {
		case <-scheduler.ready:
			release()
			cancel()
			t.Fatal("job recovery crossed exclusive snapshot")
		case <-time.After(100 * time.Millisecond):
		}
		release()
		<-scheduler.ready
		cancel()
		<-done
	})

	t.Run("backups", func(t *testing.T) {
		dir := t.TempDir()
		scheduler := NewBackupScheduler(dir)
		record := BackupRunRecord{ID: "startup-backup", Project: "demo", Environment: "production", Service: "postgres", StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}
		record.Containers = []string{scheduledBackupContainerName(BackupScheduleRequest{Project: record.Project, Environment: record.Environment, Service: record.Service}, record.ID, 0)}
		if err := scheduler.beginBackupRunRecord(record); err != nil {
			t.Fatal(err)
		}
		scheduler.recoverInterrupted = func(context.Context, BackupRunRecord) error { return nil }
		release, err := recovery.AcquireSnapshotLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { scheduler.Run(ctx); close(done) }()
		select {
		case <-scheduler.ready:
			release()
			cancel()
			t.Fatal("backup recovery crossed exclusive snapshot")
		case <-time.After(100 * time.Millisecond):
		}
		release()
		<-scheduler.ready
		cancel()
		<-done
	})
}

func TestAlertManagerRetriesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	manager := NewAlertManager(dir)
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "one", JobRunStatusFailed, now)
	manager.deliver = func(_ context.Context, _, _ string, _ any) alertDeliveryResult {
		return alertDeliveryResult{Retryable: true, Error: "network"}
	}
	manager.DispatchDue(context.Background())
	status, _ := manager.Status("demo", "production")
	if status.PendingDelivery != 1 || status.Deliveries[0].Attempts != 1 || status.Deliveries[0].LastError != "network" {
		t.Fatalf("retry status = %#v", status)
	}

	restarted := NewAlertManager(dir)
	restarted.now = func() time.Time { return now.Add(2 * time.Second) }
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	restarted.deliver = func(_ context.Context, _, _ string, _ any) alertDeliveryResult {
		deliveries++
		return alertDeliveryResult{Delivered: true}
	}
	restarted.DispatchDue(context.Background())
	status, _ = restarted.Status("demo", "production")
	if deliveries != 1 || status.PendingDelivery != 0 || !status.Subjects[0].OpenedProviders["webhook"] {
		// OpenedProviders is intentionally redacted from Status; inspect state below.
		restarted.mu.Lock()
		opened := restarted.states[alertScopeKey("demo", "production")].Subjects[alertSubjectKey("job", "worker")].OpenedProviders["webhook"]
		restarted.mu.Unlock()
		if deliveries != 1 || status.PendingDelivery != 0 || !opened {
			t.Fatalf("restart delivery status = %#v, deliveries=%d, opened=%v", status, deliveries, opened)
		}
	}
}

func TestAlertManagerDoesNotConsumeAttemptWhenDeliveryIsInterrupted(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Interrupted: true, Error: "interrupted"}
	}
	manager.DispatchDue(context.Background())
	status, _ := manager.Status("demo", "production")
	if status.PendingDelivery != 1 || status.Deliveries[0].Attempts != 0 || status.Deliveries[0].Exhausted {
		t.Fatalf("interrupted delivery = %#v", status)
	}
}

func TestAlertManagerRetainsPreviousRecoveryWhenNewEpisodeOpens(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure-one", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	first, _ := manager.Status("demo", "production")
	oldEpisode := first.Deliveries[0].EpisodeID
	observeAlertFact(t, manager, "failure-two", JobRunStatusFailed, now.Add(2*time.Minute))
	status, _ := manager.Status("demo", "production")
	transitions := map[string]string{}
	for _, delivery := range status.Deliveries {
		transitions[delivery.EpisodeID] = delivery.Transition
	}
	if len(status.Deliveries) != 2 || transitions[oldEpisode] != "recovery" || status.ActiveEpisodes != 1 {
		t.Fatalf("reopened episode discarded recovery: %#v", status)
	}
}

func TestAlertManagerQueuesRecoveryWhenInflightOpeningFinishesAfterClosure(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	started := make(chan struct{})
	release := make(chan struct{})
	manager.deliver = func(_ context.Context, _ string, _ string, payload any) alertDeliveryResult {
		if payload.(map[string]any)["transition"] == "opening" {
			close(started)
			<-release
			return alertDeliveryResult{Delivered: true}
		}
		return alertDeliveryResult{Interrupted: true, Error: "interrupted"}
	}
	done := make(chan struct{})
	go func() {
		manager.DispatchDue(context.Background())
		close(done)
	}()
	<-started
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	close(release)
	<-done
	status, _ := manager.Status("demo", "production")
	if status.ActiveEpisodes != 0 || status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" {
		t.Fatalf("in-flight opening closure = %#v", status)
	}
}

func TestAlertManagerRetiresActiveSubjectAfterRecovery(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	if err := manager.RemoveSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	if len(status.Subjects) != 1 || !status.Subjects[0].Retiring || status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" {
		t.Fatalf("retiring subject = %#v", status)
	}
	manager.DispatchDue(context.Background())
	status, _ = manager.Status("demo", "production")
	if len(status.Subjects) != 0 || status.PendingDelivery != 0 {
		t.Fatalf("retired subject remains = %#v", status)
	}
}

func TestAlertManagerDisableRetainsCredentialsUntilRecovery(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	if err := manager.RemoveConfig("demo", "production"); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	if status.Configured || status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" {
		t.Fatalf("disabled scope = %#v", status)
	}
	if _, err := os.Stat(manager.credentialsDir("demo", "production")); err != nil {
		t.Fatalf("credentials removed before recovery: %v", err)
	}
	manager.DispatchDue(context.Background())
	if _, err := os.Stat(manager.credentialsDir("demo", "production")); !os.IsNotExist(err) {
		t.Fatalf("credentials remain after recovery: %v", err)
	}
}

func TestAlertManagerRemovalDuringInflightOpeningQueuesRecovery(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	started := make(chan struct{})
	release := make(chan struct{})
	manager.deliver = func(_ context.Context, _ string, _ string, payload any) alertDeliveryResult {
		if payload.(map[string]any)["transition"] == "opening" {
			close(started)
			<-release
			return alertDeliveryResult{Delivered: true}
		}
		return alertDeliveryResult{Interrupted: true}
	}
	done := make(chan struct{})
	go func() { manager.DispatchDue(context.Background()); close(done) }()
	<-started
	if err := manager.RemoveSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	status, _ := manager.Status("demo", "production")
	if len(status.Subjects) != 1 || status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" {
		t.Fatalf("in-flight retirement = %#v", status)
	}
}

func TestAlertManagerRejectsSymlinkedStorageParent(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, schedulerAlertsDirName)); err != nil {
		t.Fatal(err)
	}
	manager := NewAlertManager(dataDir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err == nil || !strings.Contains(err.Error(), "path component") {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write escaped through symlink: %#v", entries)
	}
}

func TestAlertManagerExhaustsBoundedRetriesAndDoesNotSendUnopenedRecovery(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	attempts := 0
	manager.deliver = func(_ context.Context, _, _ string, _ any) alertDeliveryResult {
		attempts++
		return alertDeliveryResult{Retryable: true, Error: "network"}
	}
	for range alertDeliveryMaxAttempts {
		manager.DispatchDue(context.Background())
		now = now.Add(time.Minute)
	}
	status, _ := manager.Status("demo", "production")
	if attempts != alertDeliveryMaxAttempts || status.Exhausted != 1 || status.Deliveries[0].Attempts != alertDeliveryMaxAttempts {
		t.Fatalf("exhausted status = %#v, attempts=%d", status, attempts)
	}
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	status, _ = manager.Status("demo", "production")
	if status.ActiveEpisodes != 0 || status.PendingDelivery != 0 || status.Exhausted != 0 {
		t.Fatalf("recovery queued for unopened provider: %#v", status)
	}
}

func TestAlertManagerReopensActiveEpisodeAfterTargetRotation(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(_ context.Context, _, _ string, _ any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	status, _ := manager.Status("demo", "production")
	if status.PendingDelivery != 0 || status.ActiveEpisodes != 1 {
		t.Fatalf("opening status = %#v", status)
	}
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status("demo", "production")
	if status.PendingDelivery != 0 {
		t.Fatalf("unchanged target duplicated opening: %#v", status)
	}
	config.Targets.Webhook = "https://alerts.example/new-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status("demo", "production")
	transitions := map[string]bool{}
	for _, delivery := range status.Deliveries {
		transitions[delivery.Transition] = true
	}
	if status.PendingDelivery != 2 || !transitions["opening"] || !transitions["recovery"] {
		t.Fatalf("rotated target did not receive opening: %#v", status)
	}
	manager.DispatchDue(context.Background())
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	status, _ = manager.Status("demo", "production")
	if status.PendingDelivery != 1 || status.Deliveries[0].Transition != "recovery" {
		t.Fatalf("rotated target recovery = %#v", status)
	}
}

func TestAlertManagerUsesDeliveryCredentialGenerationAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	config.Targets.Webhook = "https://alerts.example/old-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	oldGeneration := manager.configGenerations[alertScopeKey("demo", "production")]
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	config.Targets.Webhook = "https://alerts.example/new-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); err != nil {
		t.Fatalf("old credentials removed before recovery: %v", err)
	}
	var firstEndpoint string
	manager.deliver = func(_ context.Context, _ string, endpoint string, _ any) alertDeliveryResult {
		firstEndpoint = endpoint
		return alertDeliveryResult{Interrupted: true}
	}
	manager.DispatchDue(context.Background())
	if firstEndpoint != "https://alerts.example/old-token" {
		t.Fatalf("old-generation recovery endpoint = %q", firstEndpoint)
	}
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); !os.IsNotExist(err) {
		t.Fatalf("old credentials remain after recovery: %v", err)
	}
}

func TestAlertManagerDoesNotRepeatRecoveryWhenInactiveTargetRotates(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	oldGeneration := manager.configGenerations[alertScopeKey("demo", "production")]
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	manager.DispatchDue(context.Background())
	config.Targets.Webhook = "https://alerts.example/new-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	if status.ActiveEpisodes != 0 || status.PendingDelivery != 0 {
		t.Fatalf("inactive rotation queued duplicate recovery: %#v", status)
	}
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); !os.IsNotExist(err) {
		t.Fatalf("inactive generation remains after rotation: %v", err)
	}
}

func TestAlertManagerKeepsOldEpisodeOpenWhenRotationRecoveryOutboxIsFull(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	manager.DispatchDue(context.Background())
	manager.mu.Lock()
	state := manager.states[alertScopeKey("demo", "production")]
	oldGeneration := manager.configGenerations[alertScopeKey("demo", "production")]
	for index := 0; index < maxAlertDeliveries; index++ {
		state.Deliveries = append(state.Deliveries, AlertDeliveryStatus{
			ID: fmt.Sprintf("pending-%d", index), EpisodeID: fmt.Sprintf("older-%d", index), Kind: "job", Service: "worker",
			Transition: "recovery", Provider: "webhook", CredentialGeneration: oldGeneration, NextAttemptAt: now.Add(time.Hour),
		})
	}
	if err := manager.persistStateLocked(state); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	config.Targets.Webhook = "https://alerts.example/new-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "success", JobRunStatusSucceeded, now.Add(time.Minute))
	manager.mu.Lock()
	subject := manager.states[alertScopeKey("demo", "production")].Subjects[alertSubjectKey("job", "worker")]
	active, episodeID, generation := subject.Active, subject.EpisodeID, subject.OpenedGeneration
	manager.mu.Unlock()
	if !active || episodeID == "" || generation != oldGeneration {
		t.Fatalf("full outbox discarded old recovery state: active=%v episode=%q generation=%q", active, episodeID, generation)
	}
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); err != nil {
		t.Fatalf("old credentials removed while recovery was blocked: %v", err)
	}
}

func TestAlertConfigDurationsRequireAtLeastOneMinute(t *testing.T) {
	for _, seconds := range []int64{1, 59} {
		request := testAlertConfig(AlertPolicy{JobNoSuccessSeconds: seconds})
		if err := validateAlertConfigRequest(request); err == nil {
			t.Fatalf("duration %d was accepted", seconds)
		}
	}
	for _, seconds := range []int64{60, int64((365 * 24 * time.Hour) / time.Second)} {
		request := testAlertConfig(AlertPolicy{JobNoSuccessSeconds: seconds})
		if err := validateAlertConfigRequest(request); err != nil {
			t.Fatalf("duration %d rejected: %v", seconds, err)
		}
	}
}

func TestAlertManagerRestartPreservesDispatchedOpeningDuringDisable(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "failure", JobRunStatusFailed, now)
	manager.mu.Lock()
	state := manager.states[alertScopeKey("demo", "production")]
	state.Deliveries[0].DispatchStarted = true
	if err := manager.persistStateLocked(state); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	if err := manager.RemoveConfig("demo", "production"); err != nil {
		t.Fatal(err)
	}
	restarted := NewAlertManager(dir)
	restarted.now = func() time.Time { return now.Add(time.Second) }
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	var transitions []string
	restarted.deliver = func(_ context.Context, _ string, _ string, payload any) alertDeliveryResult {
		transitions = append(transitions, payload.(map[string]any)["transition"].(string))
		return alertDeliveryResult{Delivered: true}
	}
	restarted.DispatchDue(context.Background())
	if strings.Join(transitions, ",") != "opening,recovery" {
		t.Fatalf("restart transitions = %v", transitions)
	}
	if _, err := os.Stat(restarted.credentialsDir("demo", "production")); !os.IsNotExist(err) {
		t.Fatalf("retired credentials remain: %v", err)
	}
}

func TestSchedulersRejectSymlinkedPersistenceRoots(t *testing.T) {
	for _, test := range []struct {
		name string
		root string
		run  func(string) error
	}{
		{name: "jobs", root: jobRunsDirName, run: func(dir string) error { return NewJobScheduler(dir).recoverInterruptedRuns(context.Background()) }},
		{name: "backups", root: backupRunsDirName, run: func(dir string) error { return NewBackupScheduler(dir).recoverInterruptedRuns(context.Background()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.Symlink(t.TempDir(), filepath.Join(dataDir, test.root)); err != nil {
				t.Fatal(err)
			}
			if err := test.run(dataDir); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("recovery error = %v", err)
			}
		})
	}
}

func TestAlertManagerTracksTimeoutAndSkippedIndependently(t *testing.T) {
	tests := []struct {
		name   string
		policy AlertPolicy
		status string
		reason string
	}{
		{name: "timeout", policy: AlertPolicy{JobConsecutiveTimeouts: 1}, status: JobRunStatusTimeout, reason: "consecutive_timeouts"},
		{name: "skipped", policy: AlertPolicy{JobConsecutiveSkipped: 1}, status: JobRunStatusSkipped, reason: "consecutive_skipped"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewAlertManager(t.TempDir())
			if err := manager.ApplyConfig(testAlertConfig(test.policy)); err != nil {
				t.Fatal(err)
			}
			observeAlertFact(t, manager, test.name, test.status, time.Now().UTC())
			status, _ := manager.Status("demo", "production")
			if status.ActiveEpisodes != 1 || len(status.Subjects) != 1 || len(status.Subjects[0].Reasons) != 1 || status.Subjects[0].Reasons[0] != test.reason {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestAlertManagerEvaluatesNoSuccessAndBackupAge(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	config := testAlertConfig(AlertPolicy{JobNoSuccessSeconds: 60, BackupMaxAgeSeconds: 120})
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "backup", "database"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := manager.Evaluate(); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	if status.ActiveEpisodes != 2 {
		t.Fatalf("active episodes = %d, want 2: %#v", status.ActiveEpisodes, status.Subjects)
	}
	if strings.Join(status.Subjects[0].Reasons, ",")+strings.Join(status.Subjects[1].Reasons, ",") != "backup_staleno_success" {
		t.Fatalf("reasons = %#v", status.Subjects)
	}
}

func TestAlertManagerSeparatesCredentialsFromStateAndStatus(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	config.Targets.Webhook = "https://alerts.example/secret-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	observeAlertFact(t, manager, "one", JobRunStatusFailed, time.Now().UTC())
	stateData, err := os.ReadFile(manager.statePath("demo", "production"))
	if err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	statusData, _ := json.Marshal(status)
	configData, err := os.ReadFile(manager.configPath("demo", "production"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"policy": configData, "state": stateData, "status": statusData} {
		if strings.Contains(string(data), "secret-token") || strings.Contains(string(data), "alerts.example") {
			t.Fatalf("%s disclosed target credential: %s", name, data)
		}
	}
	info, err := os.Stat(manager.configPath("demo", "production"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	generation := manager.configGenerations[alertScopeKey("demo", "production")]
	credentialsPath := manager.credentialsPath("demo", "production", generation)
	credentialsData, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentialsData), "secret-token") {
		t.Fatal("credential document did not preserve target")
	}
	info, err = os.Stat(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials mode = %o, want 600", info.Mode().Perm())
	}
	oldGeneration := generation
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	if manager.configGenerations[alertScopeKey("demo", "production")] != oldGeneration {
		t.Fatal("unchanged target replaced its credential generation")
	}
	config.Targets.Webhook = "https://alerts.example/new-secret-token"
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	if manager.configGenerations[alertScopeKey("demo", "production")] == oldGeneration {
		t.Fatal("changed target retained its credential generation")
	}
	if _, err := os.Stat(credentialsPath); !os.IsNotExist(err) {
		t.Fatalf("old credential generation remains: %v", err)
	}
}

func TestAlertManagerRejectsInsecureCredentialFileOnRestart(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	generation := manager.configGenerations[alertScopeKey("demo", "production")]
	if err := os.Chmod(manager.credentialsPath("demo", "production", generation), 0644); err != nil {
		t.Fatal(err)
	}
	restarted := NewAlertManager(dir)
	if err := restarted.Load(); err == nil || !strings.Contains(err.Error(), "regular 0600") {
		t.Fatalf("Load() error = %v, want insecure credential rejection", err)
	}
}

func TestAlertManagerCleansCredentialsAndStateWithoutCommittedPolicy(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manager.configPath("demo", "production")); err != nil {
		t.Fatal(err)
	}
	restarted := NewAlertManager(dir)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(restarted.credentialsDir("demo", "production")); !os.IsNotExist(err) {
		t.Fatalf("orphan credentials remain: %v", err)
	}
	if _, err := os.Stat(restarted.statePath("demo", "production")); !os.IsNotExist(err) {
		t.Fatalf("orphan state remains: %v", err)
	}
}

func TestAlertManagerIgnoresDuplicateAndOlderFacts(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 2})); err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	observeAlertFact(t, manager, "new", JobRunStatusFailed, finished)
	observeAlertFact(t, manager, "new", JobRunStatusFailed, finished)
	observeAlertFact(t, manager, "old", JobRunStatusFailed, finished.Add(-time.Second))
	status, _ := manager.Status("demo", "production")
	if status.Subjects[0].ConsecutiveFailures != 1 || status.ActiveEpisodes != 0 {
		t.Fatalf("duplicate facts changed counters: %#v", status.Subjects[0])
	}
}

func TestAlertManagerIgnoresFactsAfterSubjectRemoval(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Observe(AlertAttemptFact{ID: "late", Project: "demo", Environment: "production", Kind: "job", Service: "worker", Status: JobRunStatusFailed, StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status("demo", "production")
	if len(status.Subjects) != 0 || status.ActiveEpisodes != 0 {
		t.Fatalf("removed subject was resurrected: %#v", status)
	}
}

func TestAlertManagerReplaysPersistedScheduledJobFacts(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	scheduler := NewJobScheduler(dir)
	finished := time.Now().UTC()
	scheduler.appendRunRecord(JobRunRecord{
		ID: "scheduled-failure", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule,
		StartedAt: finished.Add(-time.Second), FinishedAt: finished, Status: JobRunStatusFailed,
	})
	scheduler.appendRunRecord(JobRunRecord{
		ID: "manual-success", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerManual,
		StartedAt: finished, FinishedAt: finished.Add(time.Second), Status: JobRunStatusSucceeded,
	})

	restartedManager := NewAlertManager(dir)
	if err := restartedManager.Load(); err != nil {
		t.Fatal(err)
	}
	restartedScheduler := NewJobScheduler(dir)
	restartedScheduler.alerts = restartedManager
	if err := restartedScheduler.syncAlertSubject(JobSpec{Project: "demo", Environment: "production", Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	status, _ := restartedManager.Status("demo", "production")
	if status.ActiveEpisodes != 1 || len(status.Subjects) != 1 || status.Subjects[0].LastStatus != JobRunStatusFailed {
		t.Fatalf("replayed status = %#v", status)
	}
}

func TestScheduledJobOverlapPersistsSkippedAlertFact(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveSkipped: 1})); err != nil {
		t.Fatal(err)
	}
	scheduler := NewJobScheduler(dir)
	scheduler.alerts = manager
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker"}
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	scheduler.specs[key] = spec
	scheduler.generations[key] = 7
	scheduler.running[key] = true
	if err := manager.RegisterSubject(spec.Project, spec.Environment, "job", spec.Name); err != nil {
		t.Fatal(err)
	}
	scheduler.runScheduledJob(key, 7)
	if len(scheduler.mutating) != 0 {
		t.Fatalf("scheduled overlap left mutation activity: %#v", scheduler.mutating)
	}
	runs, err := scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Trigger != JobTriggerSchedule || runs[0].Status != JobRunStatusSkipped {
		t.Fatalf("runs = %#v", runs)
	}
	status, _ := manager.Status(spec.Project, spec.Environment)
	if status.ActiveEpisodes != 1 || status.Subjects[0].Reasons[0] != "consecutive_skipped" {
		t.Fatalf("status = %#v", status)
	}
}

func TestManualJobOverlapBalancesMutationActivity(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker"}
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	scheduler.specs[key] = spec
	scheduler.running[key] = true
	if err := scheduler.Trigger(context.Background(), spec.Project, spec.Environment, spec.Name, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Trigger() error = %v", err)
	}
	if len(scheduler.mutating) != 0 {
		t.Fatalf("manual overlap left mutation activity: %#v", scheduler.mutating)
	}
}

func TestJobSchedulerRecoversDurableRunningAttempt(t *testing.T) {
	dir := t.TempDir()
	manager := NewAlertManager(dir)
	if err := manager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	scheduler := NewJobScheduler(dir)
	scheduler.alerts = manager
	started := time.Now().Add(-time.Minute).UTC()
	record := JobRunRecord{ID: "running-job", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule, Container: "tako_demo_production_worker_job_1", StartedAt: started, ExitCode: -1, Status: JobRunStatusRunning}
	record.Container = scheduledJobContainerName(record.Project, record.Environment, record.Job, record.ID)
	if err := scheduler.beginRunRecord(record); err != nil {
		t.Fatal(err)
	}
	recovered := 0
	scheduler.recoverInterrupted = func(_ context.Context, got JobRunRecord) error {
		recovered++
		if got.ID != record.ID {
			t.Fatalf("recovered record = %#v", got)
		}
		return nil
	}
	if err := scheduler.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := scheduler.readRunRecords("demo", "production", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || len(records) != 1 || records[0].ID != record.ID || records[0].Status != JobRunStatusFailed || records[0].FinishedAt.IsZero() {
		t.Fatalf("recovered=%d records=%#v", recovered, records)
	}
	status, _ := manager.Status("demo", "production")
	if status.ActiveEpisodes != 1 || status.Subjects[0].LastStatus != JobRunStatusFailed {
		t.Fatalf("alert status = %#v", status)
	}
}

func TestJobRunningAttemptSurvivesBoundedOverlapHistory(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	started := time.Now().Add(-time.Hour).UTC()
	running := JobRunRecord{ID: "running-job", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule, Container: "tako_demo_production_worker_job_1", StartedAt: started, ExitCode: -1, Status: JobRunStatusRunning}
	running.Container = scheduledJobContainerName(running.Project, running.Environment, running.Job, running.ID)
	if err := scheduler.beginRunRecord(running); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxJobRunRecords+5; index++ {
		finished := started.Add(time.Duration(index+1) * time.Second)
		scheduler.appendRunRecord(JobRunRecord{ID: fmt.Sprintf("skip-%d", index), Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule, StartedAt: finished, FinishedAt: finished, ExitCode: -1, Status: JobRunStatusSkipped})
	}
	records, err := scheduler.readRunRecords("demo", "production", "worker")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		found = found || record.ID == running.ID
	}
	if !found || len(records) != maxJobRunRecords {
		t.Fatalf("running attempt was evicted: len=%d records=%#v", len(records), records)
	}
	scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return nil }
	if err := scheduler.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ = scheduler.readRunRecords("demo", "production", "worker")
	for _, record := range records {
		if record.ID == running.ID && record.Status != JobRunStatusFailed {
			t.Fatalf("running attempt was not recovered: %#v", record)
		}
	}
}

func TestJobSchedulerReportsRecoveryStartupFailure(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	record := JobRunRecord{ID: "running-job", Project: "demo", Environment: "production", Job: "worker", Trigger: JobTriggerSchedule, Container: "tako_demo_production_worker_job_1", StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}
	record.Container = scheduledJobContainerName(record.Project, record.Environment, record.Job, record.ID)
	if err := scheduler.beginRunRecord(record); err != nil {
		t.Fatal(err)
	}
	scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return fmt.Errorf("docker unavailable") }
	scheduler.Run(context.Background())
	if err := scheduler.StartupError(); err == nil || !strings.Contains(err.Error(), "docker unavailable") {
		t.Fatalf("StartupError() = %v", err)
	}
}

func TestServerRejectsSchedulerStartupFailure(t *testing.T) {
	server := NewServer("/tmp/takod-alert-startup.sock", t.TempDir(), "test")
	server.backupScheduler.startupErr = fmt.Errorf("recovery failed")
	server.backupScheduler.readyOnce.Do(func() { close(server.backupScheduler.ready) })
	server.jobScheduler.readyOnce.Do(func() { close(server.jobScheduler.ready) })
	if err := server.waitForSchedulerStartup(); err == nil || !strings.Contains(err.Error(), "recovery failed") {
		t.Fatalf("waitForSchedulerStartup() error = %v", err)
	}
}

func TestSchedulerContainerNamesAreBoundedForMaximumIdentities(t *testing.T) {
	project := strings.Repeat("p", 63)
	environment := strings.Repeat("e", 63)
	service := strings.Repeat("s", 63)
	attemptID := strings.Repeat("a", 128)
	jobContainer := scheduledJobContainerName(project, environment, service, attemptID)
	backupContainer := scheduledBackupContainerName(BackupScheduleRequest{Project: project, Environment: environment, Service: service}, attemptID, 99)
	for kind, container := range map[string]string{"job": jobContainer, "backup": backupContainer} {
		if len(container) > 128 || !isSafeContainerName(container) {
			t.Fatalf("%s container name len=%d value=%q", kind, len(container), container)
		}
	}
	jobScheduler := NewJobScheduler(t.TempDir())
	if err := jobScheduler.beginRunRecord(JobRunRecord{ID: attemptID, Project: project, Environment: environment, Job: service, Trigger: JobTriggerSchedule, Container: jobContainer, StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}); err != nil {
		t.Fatalf("persist maximum job identity: %v", err)
	}
	backupScheduler := NewBackupScheduler(t.TempDir())
	if err := backupScheduler.beginBackupRunRecord(BackupRunRecord{ID: attemptID, Project: project, Environment: environment, Service: service, Containers: []string{scheduledBackupContainerName(BackupScheduleRequest{Project: project, Environment: environment, Service: service}, attemptID, 0)}, StartedAt: time.Now().UTC(), Status: JobRunStatusRunning}); err != nil {
		t.Fatalf("persist maximum backup identity: %v", err)
	}
}

func TestJobSchedulerCancellationStopsAndWaitsForCronRun(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker", Schedule: "@every 10ms", Image: "busybox:latest", Command: []string{"true"}}
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: spec.Project, Environment: spec.Environment, Jobs: []JobSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	scheduler.runJob = func(ctx context.Context, _ JobSpec, _ string, _ io.Writer) (int, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return 0, nil
	}
	scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scheduler.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled job did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not wait for canceled cron run")
	}
	select {
	case <-finished:
	default:
		t.Fatal("scheduler returned before cron run finished")
	}
	records, err := scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
	if err != nil || len(records) != 1 || records[0].Status != JobRunStatusFailed {
		t.Fatalf("canceled job records = %#v, err=%v", records, err)
	}
}

func TestJobCancellationKeepsRunningRecordWhenHelperCleanupFails(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker", Image: "busybox:latest", Command: []string{"true"}}
	started := make(chan struct{})
	scheduler.runJob = func(ctx context.Context, _ JobSpec, _ string, _ io.Writer) (int, error) {
		close(started)
		<-ctx.Done()
		return -1, ctx.Err()
	}
	scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return fmt.Errorf("docker unavailable") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan JobRunRecord, 1)
	go func() { done <- scheduler.executeJob(ctx, spec, JobTriggerSchedule, nil) }()
	<-started
	cancel()
	record := <-done
	if record.Status != JobRunStatusRunning || !record.FinishedAt.IsZero() {
		t.Fatalf("record = %#v", record)
	}
	records, err := scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
	if err != nil || len(records) != 1 || records[0].Status != JobRunStatusRunning {
		t.Fatalf("durable records = %#v, err=%v", records, err)
	}
}

func TestRecoverInterruptedJobRemovesOnlyMatchingContainer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	record := JobRunRecord{ID: "running-job", Project: "demo", Environment: "production", Job: "worker"}
	record.Container = scheduledJobContainerName(record.Project, record.Environment, record.Job, record.ID)
	t.Setenv("TAKO_FAKE_PS_OUTPUT", record.Container+"\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS", `{"tako.project":"demo","tako.environment":"production","tako.service":"worker","tako.runtime":"takod","tako.role":"job","tako.attempt":"running-job"}`)
	if err := recoverInterruptedJobContainer(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "inspect "+record.Container) || !strings.Contains(commands, "rm -f "+record.Container) {
		t.Fatalf("recovery commands:\n%s", commands)
	}
}

func TestJobSchedulerPersistsRunningAttemptBeforeExecution(t *testing.T) {
	scheduler := NewJobScheduler(t.TempDir())
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker", Image: "example/worker:latest", Command: []string{"true"}}
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	scheduler.specs[key] = spec
	scheduler.running[key] = true
	scheduler.runJob = func(_ context.Context, _ JobSpec, _ string, _ io.Writer) (int, error) {
		records, err := scheduler.readRunRecords(spec.Project, spec.Environment, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Status != JobRunStatusRunning || records[0].Container == "" {
			t.Fatalf("records during execution = %#v", records)
		}
		return 0, nil
	}
	record := scheduler.executeReservedJob(context.Background(), spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusSucceeded {
		t.Fatalf("record = %#v", record)
	}
}

func TestAlertCredentialCleanupFailureIsRetried(t *testing.T) {
	manager := NewAlertManager(t.TempDir())
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatal(err)
	}
	oldGeneration := manager.configGenerations[alertScopeKey("demo", "production")]
	config.Targets.Webhook = "https://alerts.example/rotated-token"
	manager.cleanupCredentials = func(string, string, string) error { return fmt.Errorf("injected cleanup failure") }
	if err := manager.ApplyConfig(config); err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); err != nil {
		t.Fatalf("old generation should remain after injected failure: %v", err)
	}
	manager.cleanupCredentials = manager.removeStaleCredentials
	if err := manager.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.credentialsPath("demo", "production", oldGeneration)); !os.IsNotExist(err) {
		t.Fatalf("old generation was not removed on retry: %v", err)
	}
}

func TestHandleAlertsAppliesReadsAndDisablesPolicyWithoutDisclosingTarget(t *testing.T) {
	server := NewServer("/tmp/takod-alert-test.sock", t.TempDir(), "test")
	request := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	body, _ := json.Marshal(request)
	recorder := httptest.NewRecorder()
	server.handleAlerts(recorder, httptest.NewRequest(http.MethodPut, "/v1/alerts", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	server.handleAlerts(recorder, httptest.NewRequest(http.MethodGet, "/v1/alerts?project=demo&environment=production", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"configured":true`) || strings.Contains(recorder.Body.String(), "alerts.example") {
		t.Fatalf("GET response = %d: %s", recorder.Code, recorder.Body.String())
	}

	disable := AlertConfigRequest{Project: "demo", Environment: "production"}
	body, _ = json.Marshal(disable)
	recorder = httptest.NewRecorder()
	server.handleAlerts(recorder, httptest.NewRequest(http.MethodPut, "/v1/alerts", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(server.alertManager.configPath("demo", "production")); !os.IsNotExist(err) {
		t.Fatalf("disabled config remains: %v", err)
	}
}

func TestHandleAlertsRejectsNegativePolicyWithoutDisabling(t *testing.T) {
	server := NewServer("/tmp/takod-alert-negative.sock", t.TempDir(), "test")
	if err := server.alertManager.ApplyConfig(testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"project":"demo","environment":"production","policy":{"jobConsecutiveFailures":-1}}`)
	recorder := httptest.NewRecorder()
	server.handleAlerts(recorder, httptest.NewRequest(http.MethodPut, "/v1/alerts", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	status, err := server.alertManager.Status("demo", "production")
	if err != nil || !status.Configured {
		t.Fatalf("active policy was removed: status=%#v err=%v", status, err)
	}
}

func TestAlertTargetValidationAndPublicIPPolicy(t *testing.T) {
	config := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	config.Targets = AlertTargets{Slack: "https://alerts.example/token"}
	if err := validateAlertConfigRequest(config); err == nil {
		t.Fatal("unofficial Slack target accepted")
	}
	config.Targets = AlertTargets{Webhook: "https://169.254.169.254/token"}
	if err := validateAlertConfigRequest(config); err == nil {
		t.Fatal("private literal webhook target accepted")
	}
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1", "::1", "fc00::1", "2001:db8::1", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1"} {
		if isPublicAlertIP(net.ParseIP(value)) {
			t.Fatalf("non-public address %s accepted", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicAlertIP(net.ParseIP(value)) {
			t.Fatalf("public address %s rejected", value)
		}
	}
}

func TestAlertHTTPTransportRejectsMixedResolutionAndDialsValidatedIP(t *testing.T) {
	dialed := ""
	transport := newAlertHTTPTransport(
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			left, right := net.Pipe()
			right.Close()
			return left, nil
		},
	)
	if _, err := transport.DialContext(context.Background(), "tcp", "alerts.example:443"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("DialContext() error = %v", err)
	}
	if dialed != "" {
		t.Fatalf("dialed mixed resolution at %q", dialed)
	}

	transport = newAlertHTTPTransport(
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		},
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			left, right := net.Pipe()
			right.Close()
			return left, nil
		},
	)
	conn, err := transport.DialContext(context.Background(), "tcp", "alerts.example:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if dialed != "1.1.1.1:443" {
		t.Fatalf("dialed address = %q", dialed)
	}
	if err := newAlertHTTPClient().CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestAlertHTTPDeliveryClassifiesResponsesAndEmitsGenericIdentifiers(t *testing.T) {
	tests := []struct {
		status    int
		delivered bool
		retryable bool
	}{
		{status: 204, delivered: true},
		{status: 400},
		{status: 408, retryable: true},
		{status: 425, retryable: true},
		{status: 429, retryable: true},
		{status: 503, retryable: true},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("status-%d", test.status), func(t *testing.T) {
			var body []byte
			manager := NewAlertManager(t.TempDir())
			manager.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var err error
				body, err = io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})}
			payload := map[string]any{"eventId": "event-1", "episodeId": "episode-1", "message": "alert"}
			result := manager.deliverHTTP(context.Background(), "webhook", "https://alerts.example/token", payload)
			if result.Delivered != test.delivered || result.Retryable != test.retryable {
				t.Fatalf("result = %#v", result)
			}
			if !bytes.Contains(body, []byte(`"eventId":"event-1"`)) || !bytes.Contains(body, []byte(`"episodeId":"episode-1"`)) {
				t.Fatalf("payload = %s", body)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testAlertConfig(policy AlertPolicy) AlertConfigRequest {
	return AlertConfigRequest{
		Project: "demo", Environment: "production", Policy: policy,
		Targets: AlertTargets{Webhook: "https://alerts.example/token"},
	}
}

func observeAlertFact(t *testing.T, manager *AlertManager, id, status string, finished time.Time) {
	t.Helper()
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Observe(AlertAttemptFact{
		ID: id, Project: "demo", Environment: "production", Kind: "job", Service: "worker",
		Status: status, StartedAt: finished.Add(-time.Second), FinishedAt: finished,
	}); err != nil {
		t.Fatal(err)
	}
}
