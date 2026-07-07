package takod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	}
	for _, tc := range cases {
		spec := validJobSpecFixture()
		tc.mutate(&spec)
		if err := validateJobSpec(&spec); err == nil {
			t.Fatalf("%s: spec accepted", tc.name)
		}
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

func TestJobRunHistoryIsBounded(t *testing.T) {
	scheduler := newTestJobScheduler(t)
	spec := validJobSpecFixture()
	for i := 0; i < maxJobRunRecords+5; i++ {
		scheduler.appendRunRecord(JobRunRecord{
			Project:     spec.Project,
			Environment: spec.Environment,
			Job:         spec.Name,
			Trigger:     JobTriggerSchedule,
			StartedAt:   time.Now().UTC(),
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
	scheduler.appendRunRecord(JobRunRecord{Project: "demo", Environment: "production", Job: "report", Status: JobRunStatusSucceeded, StartedAt: time.Now().UTC()})

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
