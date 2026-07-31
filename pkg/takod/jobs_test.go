package takod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func validJobSpecFixture() JobSpec {
	return JobSpec{
		Project:     "demo",
		Environment: "production",
		Name:        "report",
		Schedule:    "*/5 * * * *",
		Image:       "registry.example.com/report:abc123",
		Command:     []string{"sh", "-c", "generate-report"},
	}
}

func newTestJobScheduler(t *testing.T) *JobScheduler {
	t.Helper()
	scheduler := NewJobScheduler(t.TempDir())
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		return 0, nil
	}
	return scheduler
}

func TestJobSchedulerProjectDrainWaitsForReservedRunAndBlocksNewRuns(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	spec := validJobSpecFixture()
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	scheduler.specs[key] = spec
	started := make(chan struct{})
	releaseRun := make(chan struct{})
	var runs atomic.Int32
	scheduler.runJob = func(context.Context, JobSpec, string, io.Writer) (int, error) {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-releaseRun
		return 0, nil
	}
	go scheduler.runScheduledJob(key)
	<-started
	drainReady := make(chan func(), 1)
	go func() {
		release, err := scheduler.BeginProjectDrain(context.Background(), spec.Project, spec.Environment)
		if err != nil {
			drainReady <- nil
			return
		}
		drainReady <- release
	}()
	select {
	case <-drainReady:
		t.Fatal("project drain returned before reserved run completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRun)
	releaseDrain := <-drainReady
	if releaseDrain == nil {
		t.Fatal("BeginProjectDrain returned error")
	}
	scheduler.runScheduledJob(key)
	if runs.Load() != 1 {
		t.Fatalf("runs during drain = %d, want 1 completed run only", runs.Load())
	}
	releaseDrain()
}

func TestJobSchedulerProjectWideDrainIsExclusiveAndBlocksApply(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	wait, release, err := scheduler.StartProjectDrain("demo", "")
	if err != nil {
		t.Fatalf("StartProjectDrain returned error: %v", err)
	}
	defer release()
	if err := wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scheduler.StartProjectDrain("demo", "production"); err == nil {
		t.Fatal("overlapping project drain was accepted")
	}
	spec := validJobSpecFixture()
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{spec}}); err == nil || !strings.Contains(err.Error(), "cleanup is draining") {
		t.Fatalf("Apply error = %v, want draining rejection", err)
	}
}

func TestJobSchedulerIgnoresQueuedCallbackAfterRemoval(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	spec := validJobSpecFixture()
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: spec.Project, Environment: spec.Environment, Jobs: []JobSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	generation := scheduler.generations[key]
	var runs atomic.Int32
	scheduler.runJob = func(context.Context, JobSpec, string, io.Writer) (int, error) {
		runs.Add(1)
		return 0, nil
	}
	if _, err := scheduler.RemoveProject(spec.Project, spec.Environment); err != nil {
		t.Fatal(err)
	}
	scheduler.runScheduledJob(key, generation)
	if runs.Load() != 0 {
		t.Fatal("removed job's queued callback executed")
	}
}

func TestJobSchedulerLoaderHonorsDurableDrainMarker(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewJobScheduler(dataDir)
	spec := validJobSpecFixture()
	if err := scheduler.persistSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := writeSchedulerDrainMarker(dataDir, "jobs", spec.Project, spec.Environment); err != nil {
		t.Fatal(err)
	}
	reloaded := NewJobScheduler(dataDir)
	if err := reloaded.loadSpecs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.entries) != 0 {
		t.Fatal("loader resurrected job protected by durable drain marker")
	}
	if _, err := reloaded.Apply(context.Background(), JobsApplyRequest{Project: spec.Project, Environment: spec.Environment, Jobs: []JobSpec{spec}}); err == nil || !strings.Contains(err.Error(), "durable project cleanup") {
		t.Fatalf("durable drain admitted job mutation: %v", err)
	}
}

func TestJobSchedulerLoaderRetriesTransientMutationContention(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewJobScheduler(dataDir)
	spec := validJobSpecFixture()
	if err := scheduler.persistSpec(spec); err != nil {
		t.Fatal(err)
	}
	endMutation, err := scheduler.beginProjectMutation(spec.Project, spec.Environment)
	if err != nil {
		t.Fatal(err)
	}
	loaded := make(chan error, 1)
	go func() { loaded <- scheduler.loadSpecs(context.Background()) }()
	select {
	case <-loaded:
		endMutation()
		t.Fatal("loader skipped schedule during temporary mutation contention")
	case <-time.After(30 * time.Millisecond):
	}
	endMutation()
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	if len(scheduler.entries) != 1 {
		t.Fatalf("loaded entries = %d, want 1", len(scheduler.entries))
	}
}

func TestJobSchedulerLoaderRetriesFreshCleanupDrain(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewJobScheduler(dataDir)
	spec := validJobSpecFixture()
	if err := scheduler.persistSpec(spec); err != nil {
		t.Fatal(err)
	}
	_, release, err := scheduler.StartProjectDrain(spec.Project, spec.Environment)
	if err != nil {
		t.Fatal(err)
	}
	loaded := make(chan error, 1)
	go func() { loaded <- scheduler.loadSpecs(context.Background()) }()
	select {
	case <-loaded:
		release()
		t.Fatal("loader skipped persisted job during a fresh cleanup drain")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	if len(scheduler.entries) != 1 {
		t.Fatalf("loaded entries = %d, want 1", len(scheduler.entries))
	}
}

func TestJobSchedulerRetainedDrainCanBeRetried(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	_, _, err := scheduler.StartProjectDrain("demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	scheduler.MarkProjectDrainRecoverable("demo", "production")
	wait, release, err := scheduler.StartProjectDrain("demo", "production")
	if err != nil {
		t.Fatalf("recoverable drain retry failed: %v", err)
	}
	if err := wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scheduler.StartProjectDrain("demo", "production"); err == nil {
		t.Fatal("retained drain recovery token was claimed more than once")
	}
	release()
	if _, ok := scheduler.draining[schedulerDrainScope("demo", "production")]; ok {
		t.Fatal("successful retry did not clear retained drain")
	}
}

func TestJobSchedulerMarkerRemovalFailureRetainsInMemoryBarrier(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	_, release, err := scheduler.StartProjectDrain("demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	marker := schedulerDrainMarkerPath(scheduler.dataDir, "jobs", "demo", "production")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, "block"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	release()
	if !scheduler.draining[schedulerDrainScope("demo", "production")] || !scheduler.recoverable[schedulerDrainScope("demo", "production")] {
		t.Fatal("marker removal failure cleared fail-closed scheduler barrier")
	}
}

func TestValidateJobSpecAcceptsFullSpec(t *testing.T) {
	spec := validJobSpecFixture()
	spec.Timezone = "America/New_York"
	spec.Env = []string{"REPORT_KIND=daily"}
	spec.Network = "tako_demo_production"
	spec.Mounts = []string{"type=volume,source=tako_demo_production_data,target=/data"}
	spec.TimeoutSeconds = 300
	if err := validateJobSpec(&spec); err != nil {
		t.Fatalf("valid job spec rejected: %v", err)
	}
}

func TestValidateJobSpecRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*JobSpec)
	}{
		{"bad project", func(s *JobSpec) { s.Project = "Demo!" }},
		{"bad environment", func(s *JobSpec) { s.Environment = "prod$" }},
		{"bad name", func(s *JobSpec) { s.Name = "Report" }},
		{"empty schedule", func(s *JobSpec) { s.Schedule = " " }},
		{"bad schedule", func(s *JobSpec) { s.Schedule = "every day" }},
		{"bad timezone", func(s *JobSpec) { s.Timezone = "Mars/Olympus" }},
		{"timezone with spaces", func(s *JobSpec) { s.Timezone = "America/New York" }},
		{"empty command", func(s *JobSpec) { s.Command = nil }},
		{"blank command", func(s *JobSpec) { s.Command = []string{" "} }},
		{"missing image", func(s *JobSpec) { s.Image = "" }},
		{"env without equals", func(s *JobSpec) { s.Env = []string{"NOVALUE"} }},
		{"bad network", func(s *JobSpec) { s.Network = "net work" }},
		{"blank mount", func(s *JobSpec) { s.Mounts = []string{" "} }},
		{"negative timeout", func(s *JobSpec) { s.TimeoutSeconds = -1 }},
		{"excessive timeout", func(s *JobSpec) { s.TimeoutSeconds = maxExecTimeoutSeconds + 1 }},
		{"unsafe memory limit", func(s *JobSpec) { s.MemoryLimit = "512m --privileged" }},
		{"unsafe cpu limit", func(s *JobSpec) { s.CPULimit = "1.5 --privileged" }},
		{"reserved label", func(s *JobSpec) { s.Labels = map[string]string{"tako.project": "other"} }},
	}
	for _, tc := range cases {
		spec := validJobSpecFixture()
		tc.mutate(&spec)
		if err := validateJobSpec(&spec); err == nil {
			t.Fatalf("%s: spec accepted", tc.name)
		}
	}
}

func TestBuildJobRunArgsUsesEntrypointExecCommandAndCustomLabels(t *testing.T) {
	spec := validJobSpecFixture()
	spec.Command = []string{"report", "--format", "json"}
	spec.Entrypoint = []string{"/usr/bin/env", "python3"}
	spec.Labels = map[string]string{"com.example.role": "report"}
	got := buildJobRunArgs(spec, "tako_demo_production_report_job_1", "")
	wantTail := []string{spec.Image, "python3", "report", "--format", "json"}
	if len(got) < len(wantTail) || strings.Join(got[len(got)-len(wantTail):], "|") != strings.Join(wantTail, "|") {
		t.Fatalf("args tail = %#v, want %#v; all args %#v", got, wantTail, got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--entrypoint /usr/bin/env") || !strings.Contains(joined, "--label com.example.role=report") {
		t.Fatalf("args missing entrypoint or custom label: %s", joined)
	}
}

func TestJobCronSpecCarriesTimezone(t *testing.T) {
	spec := validJobSpecFixture()
	if got := jobCronSpec(spec); got != "CRON_TZ=UTC */5 * * * *" {
		t.Fatalf("spec without timezone = %q, want UTC default", got)
	}
	spec.Timezone = "Europe/Berlin"
	if got := jobCronSpec(spec); got != "CRON_TZ=Europe/Berlin */5 * * * *" {
		t.Fatalf("spec with timezone = %q", got)
	}
}

func TestBuildJobRunArgsLabelsAndDefaults(t *testing.T) {
	spec := validJobSpecFixture()
	spec.Env = []string{"REPORT_KIND=daily"}
	spec.Mounts = []string{"type=volume,source=data,target=/data"}
	spec.MemoryLimit = "512m"
	spec.CPULimit = "0.5"

	got := buildJobRunArgs(spec, "tako_demo_production_report_job_1", "/tmp/envfile")
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"run --rm",
		"--network tako_demo_production",
		"--label tako.project=demo",
		"--label tako.service=report",
		"--label " + jobRoleLabel,
		"--env-file /tmp/envfile",
		"-e REPORT_KIND=daily",
		"--mount type=volume,source=data,target=/data",
		"--memory 512m",
		"--cpus 0.5",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
	if got[len(got)-4] != spec.Image {
		t.Fatalf("image not before command: %s", joined)
	}
}

func TestBuildJobRunArgsUsesRuntimeControls(t *testing.T) {
	spec := validJobSpecFixture()
	spec.User = "1000:1000"
	spec.WorkingDir = "/work"
	spec.StopTimeoutSeconds = 60
	spec.Init = true
	spec.ExtraHosts = []string{"database:10.0.0.2"}
	spec.Ulimits = map[string]config.UlimitConfig{"nofile": {Soft: 262144, Hard: 262144}}
	spec.ShmSize = "256m"
	got := buildJobRunArgs(spec, "tako_demo_production_report_job_1", "")
	want := []string{
		"run", "--rm", "--name", "tako_demo_production_report_job_1", "--network", "tako_demo_production",
		"--label", "tako.project=demo", "--label", "tako.environment=production",
		"--label", "tako.service=report", "--label", "tako.runtime=takod", "--label", jobRoleLabel,
		"--user", "1000:1000", "--workdir", "/work", "--stop-timeout", "60", "--init",
		"--add-host", "database:10.0.0.2", "--ulimit", "nofile=262144:262144", "--shm-size", "256m",
		spec.Image, "sh", "-c", "generate-report",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("job args = %#v, want %#v", got, want)
	}
}

func TestJobsApplySchedulesAndRemovesStaleJobs(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	first := validJobSpecFixture()
	second := validJobSpecFixture()
	second.Name = "cleanup"
	second.Schedule = "0 3 * * *"

	response, err := scheduler.Apply(context.Background(), JobsApplyRequest{
		Project:     "demo",
		Environment: "production",
		Jobs:        []JobSpec{first, second},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(response.Applied) != 2 || len(response.Removed) != 0 {
		t.Fatalf("response = %+v", response)
	}
	if _, err := os.Stat(jobSpecPath(scheduler.dataDir, "demo", "production", "cleanup")); err != nil {
		t.Fatalf("spec not persisted: %v", err)
	}

	listed := scheduler.List("demo", "production")
	if len(listed) != 2 || listed[0].Name != "cleanup" || listed[1].Name != "report" {
		t.Fatalf("list = %+v", listed)
	}

	response, err = scheduler.Apply(context.Background(), JobsApplyRequest{
		Project:     "demo",
		Environment: "production",
		Jobs:        []JobSpec{first},
	})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(response.Removed) != 1 || response.Removed[0] != "cleanup" {
		t.Fatalf("removed = %v", response.Removed)
	}
	if _, err := os.Stat(jobSpecPath(scheduler.dataDir, "demo", "production", "cleanup")); !os.IsNotExist(err) {
		t.Fatalf("stale spec still on disk: %v", err)
	}
	if listed := scheduler.List("demo", "production"); len(listed) != 1 {
		t.Fatalf("list after removal = %+v", listed)
	}
}

func TestJobsApplyRejectsInvalidAndDuplicateJobs(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	bad := validJobSpecFixture()
	bad.Schedule = "nope"
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{bad}}); err == nil {
		t.Fatal("invalid schedule accepted")
	}
	dup := validJobSpecFixture()
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{dup, dup}}); err == nil {
		t.Fatal("duplicate job accepted")
	}
}

func TestJobSchedulerLoadsPersistedSpecsOnRun(t *testing.T) {
	dataDir := t.TempDir()
	scheduler := NewJobScheduler(dataDir)
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		return 0, nil
	}
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{validJobSpecFixture()}}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	reloaded := NewJobScheduler(dataDir)
	reloaded.runJob = scheduler.runJob
	if err := reloaded.loadSpecs(context.Background()); err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	if listed := reloaded.List("demo", "production"); len(listed) != 1 || listed[0].Name != "report" {
		t.Fatalf("reloaded list = %+v", listed)
	}
}

func TestExecuteJobRecordsSuccessWithOutputTail(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		fmt.Fprintf(output, "report generated\n")
		return 0, nil
	}
	spec := validJobSpecFixture()

	record := scheduler.executeJob(context.Background(), spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusSucceeded || record.ExitCode != 0 {
		t.Fatalf("record = %+v", record)
	}
	if record.Output != "report generated\n" {
		t.Fatalf("output = %q", record.Output)
	}
	if record.Trigger != JobTriggerSchedule {
		t.Fatalf("trigger = %q", record.Trigger)
	}

	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
	status := scheduler.List("demo", "production")
	if len(status) != 0 {
		// executeJob alone does not schedule; List only reports scheduled jobs.
		t.Fatalf("unexpected scheduled jobs: %+v", status)
	}
}

func TestExecuteJobRecordsFailureExitCode(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		fmt.Fprintf(output, "boom")
		return 3, nil
	}
	record := scheduler.executeJob(context.Background(), validJobSpecFixture(), JobTriggerSchedule, nil)
	if record.Status != JobRunStatusFailed || record.ExitCode != 3 {
		t.Fatalf("record = %+v", record)
	}
}

func TestExecuteJobRecordsTimeout(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	scheduler.recoverInterrupted = func(context.Context, JobRunRecord) error { return nil }
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		<-ctx.Done()
		return -1, nil
	}
	spec := validJobSpecFixture()
	spec.TimeoutSeconds = 1

	record := scheduler.executeJob(context.Background(), spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusTimeout || record.ExitCode != -1 {
		t.Fatalf("record = %+v", record)
	}
	if !strings.Contains(record.Output, "timed out") {
		t.Fatalf("output = %q", record.Output)
	}
}

func TestExecuteJobSkipsOverlappingRun(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	spec := validJobSpecFixture()
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	scheduler.mu.Lock()
	scheduler.running[key] = true
	scheduler.mu.Unlock()

	var stream bytes.Buffer
	record := scheduler.executeJob(context.Background(), spec, JobTriggerSchedule, &stream)
	if record.Status != JobRunStatusSkipped {
		t.Fatalf("record = %+v", record)
	}
	if stream.Len() != 0 {
		t.Fatalf("skipped run wrote to stream: %q", stream.String())
	}
	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil || len(runs) != 1 || runs[0].Status != JobRunStatusSkipped {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
}

func TestScheduledReservedJobIsBlockedByPendingRestore(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	dir := filepath.Join(backupRootDir, "demo", "production")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	journal := backupRestoreJournal{
		SchemaVersion: 1, Phase: "mutating", TargetVolume: "demo-production-data", StageVolume: "tako-restore-stage-0123456789abcdef",
		BackupFile: "data_20240101-120000.tar.gz", RollbackFile: ".restore-rollback-0123456789abcdef.tar.gz",
	}
	if err := writeBackupRestoreJournal(filepath.Join(dir, ".restore-transaction-pending.json"), journal); err != nil {
		t.Fatal(err)
	}
	scheduler := NewJobScheduler(t.TempDir())
	spec := validJobSpecFixture()
	record := scheduler.executeReservedJob(context.Background(), spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusFailed || !strings.Contains(record.Output, "pending volume restore") {
		t.Fatalf("scheduled restore barrier record = %#v", record)
	}
}

func TestExecuteReservedJobInitializesManagedVolumeBeforeRun(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", filepath.Join(t.TempDir(), "volume-state"))
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE", filepath.Join(t.TempDir(), "marker"))
	scheduler := NewJobScheduler(t.TempDir())
	runCalled := false
	scheduler.runJob = func(context.Context, JobSpec, string, io.Writer) (int, error) {
		runCalled = true
		return 0, nil
	}
	spec := validJobSpecFixture()
	spec.Mounts = []string{"type=volume,source=managed-reports,target=/reports"}
	spec.VolumeOwnership = []ManagedVolumeOwnership{{Name: "managed-reports", UID: 1000, GID: 1001}}
	record := scheduler.executeReservedJob(context.Background(), spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusSucceeded || !runCalled {
		t.Fatalf("job record = %#v, runCalled=%v", record, runCalled)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Count(commands, "volume create") != 1 || strings.Count(commands, "chown 1000:1001 /volume") != 1 {
		t.Fatalf("job ownership initialization commands:\n%s", commands)
	}
}

func TestJobRunHistoryIsBounded(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	spec := validJobSpecFixture()
	for i := 0; i < maxJobRunRecords+5; i++ {
		finished := time.Now().UTC()
		scheduler.appendRunRecord(JobRunRecord{
			Project:     spec.Project,
			Environment: spec.Environment,
			Job:         spec.Name,
			Trigger:     JobTriggerSchedule,
			StartedAt:   finished,
			FinishedAt:  finished,
			Status:      JobRunStatusSucceeded,
		})
	}
	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != maxJobRunRecords {
		t.Fatalf("history length = %d, want %d", len(runs), maxJobRunRecords)
	}
}

func TestTriggerStreamsMarkersAndRecordsManualRun(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		fmt.Fprintf(output, "hello from job\n")
		return 0, nil
	}
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{validJobSpecFixture()}}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var stream bytes.Buffer
	if err := scheduler.Trigger(context.Background(), "demo", "production", "report", &stream); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	out := stream.String()
	if !strings.Contains(out, ExecContainerMarker+"tako_demo_production_report_job_") {
		t.Fatalf("missing container marker: %q", out)
	}
	if !strings.Contains(out, "hello from job\n") {
		t.Fatalf("missing output: %q", out)
	}
	if !strings.HasSuffix(out, ExecExitMarker+"0\n") {
		t.Fatalf("missing exit marker: %q", out)
	}

	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
	if runs[0].Trigger != JobTriggerManual || runs[0].Output != "hello from job\n" {
		t.Fatalf("run = %+v", runs[0])
	}
}

func TestTriggerUnknownJobFailsBeforeStreaming(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	var stream bytes.Buffer
	err := scheduler.Trigger(context.Background(), "demo", "production", "ghost", &stream)
	if err == nil || stream.Len() != 0 {
		t.Fatalf("err = %v, stream = %q", err, stream.String())
	}
}

func TestTriggerReservesFileSetBeforeApplyCanPruneIt(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	scheduler := newTestJobScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		close(started)
		<-release
		return 0, nil
	}

	oldFiles := []ServiceFileBundle{{Name: "file-000", Target: "/run/config", UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: []byte("old"), Mode: 0600}}}}
	oldSetID := testServiceFileSetID(t, oldFiles)
	if err := PublishServiceFiles(context.Background(), ServiceFilesRequest{Project: "demo", Environment: "production", Service: "report", FileSetID: oldSetID, Files: oldFiles}); err != nil {
		t.Fatal(err)
	}
	oldSpec := validJobSpecFixture()
	oldSpec.FileSetID = oldSetID
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{oldSpec}}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- scheduler.Trigger(context.Background(), "demo", "production", "report", nil) }()
	<-started

	newFiles := []ServiceFileBundle{{Name: "file-000", Target: "/run/config", UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: []byte("new"), Mode: 0600}}}}
	newSetID := testServiceFileSetID(t, newFiles)
	if err := PublishServiceFiles(context.Background(), ServiceFilesRequest{Project: "demo", Environment: "production", Service: "report", FileSetID: newSetID, Files: newFiles}); err != nil {
		t.Fatal(err)
	}
	newSpec := validJobSpecFixture()
	newSpec.FileSetID = newSetID
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{newSpec}}); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(serviceFilesRoot, "demo", "production", "report", oldSetID)
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("running job's file set was pruned: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old file set remained after run handoff: %v", err)
	}
}

func TestScheduledJobFires(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	var fired atomic.Int32
	scheduler.runJob = func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
		fired.Add(1)
		return 0, nil
	}
	spec := validJobSpecFixture()
	spec.Schedule = "@every 1s"
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{spec}}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for fired.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("scheduled job never fired")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done

	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil || len(runs) == 0 {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
	if runs[0].Trigger != JobTriggerSchedule {
		t.Fatalf("trigger = %q", runs[0].Trigger)
	}
}

func TestRemoveProjectUnschedulesAndDeletesState(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	if _, err := scheduler.Apply(context.Background(), JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{validJobSpecFixture()}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	finished := time.Now().UTC()
	scheduler.appendRunRecord(JobRunRecord{Project: "demo", Environment: "production", Job: "report", Trigger: JobTriggerSchedule, Status: JobRunStatusSucceeded, StartedAt: finished, FinishedAt: finished})

	removed, err := scheduler.RemoveProject("demo", "production")
	if err != nil {
		t.Fatalf("remove project: %v", err)
	}
	if len(removed) != 1 || removed[0] != "production/report" {
		t.Fatalf("removed = %v", removed)
	}
	if listed := scheduler.List("", ""); len(listed) != 0 {
		t.Fatalf("jobs still scheduled: %+v", listed)
	}
	runs, err := scheduler.Runs("demo", "production", "report")
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs after removal = %+v, err %v", runs, err)
	}
}

// runJobDocker must tolerate the nil cleanup writeTempEnvFile returns for
// env-less jobs; a bare defer panicked here after the container finished.
func TestRunJobDockerWithoutEnvFileDoesNotPanic(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	spec := JobSpec{
		Project:     "demo",
		Environment: "production",
		Name:        "tick",
		Image:       "busybox:1.36",
		Command:     []string{"echo", "ok"},
	}
	var out bytes.Buffer
	exitCode, err := runJobDocker(context.Background(), spec, "tako_demo_production_tick_job_1", &out)
	if err != nil {
		t.Fatalf("runJobDocker: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
}
