package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/robfig/cron/v3"
)

// Job persistence layout under the takod data dir: specs and run history
// live in sibling trees keyed by project/environment/name.
const (
	jobSpecDirName = "jobs"
	jobRunsDirName = "job-runs"
)

const (
	// maxJobRunRecords bounds the per-job run history kept on disk.
	maxJobRunRecords = 50
	// jobRunOutputMaxBytes bounds the captured output tail per run record.
	jobRunOutputMaxBytes = 16 * 1024
	// defaultJobTimeoutSeconds bounds a run when the spec sets no timeout.
	defaultJobTimeoutSeconds = 3600
)

// jobRoleLabel marks scheduled-run containers so orphans are identifiable
// and reaped by project cleanup.
const jobRoleLabel = "tako.role=job"

// Job run statuses.
const (
	JobRunStatusSucceeded = "succeeded"
	JobRunStatusFailed    = "failed"
	JobRunStatusTimeout   = "timeout"
	JobRunStatusSkipped   = "skipped"
)

// Job run triggers.
const (
	JobTriggerSchedule = "schedule"
	JobTriggerManual   = "manual"
)

// JobSpec declares one scheduled job on this node. The owning node receives
// the spec at deploy time via /v1/jobs/apply and fires it with its local
// cron; each run is a fresh one-off container from Image.
type JobSpec struct {
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Name        string            `json:"name"`
	Schedule    string            `json:"schedule"`
	Timezone    string            `json:"timezone,omitempty"`
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	// EnvFileContent carries the job's env/secrets; it is written to a 0600
	// temp file per run and passed via --env-file.
	EnvFileContent string   `json:"envFileContent,omitempty"`
	Env            []string `json:"env,omitempty"`
	// Network attaches run containers; default tako_<project>_<env>.
	Network            string                         `json:"network,omitempty"`
	Mounts             []string                       `json:"mounts,omitempty"`
	Files              []ServiceFileBundle            `json:"files,omitempty"`
	FileSetID          string                         `json:"fileSetId,omitempty"`
	MemoryLimit        string                         `json:"memoryLimit,omitempty"`
	CPULimit           string                         `json:"cpuLimit,omitempty"`
	User               string                         `json:"user,omitempty"`
	WorkingDir         string                         `json:"workingDir,omitempty"`
	StopTimeoutSeconds int                            `json:"stopTimeoutSeconds,omitempty"`
	Init               bool                           `json:"init,omitempty"`
	ExtraHosts         []string                       `json:"extraHosts,omitempty"`
	Ulimits            map[string]config.UlimitConfig `json:"ulimits,omitempty"`
	ShmSize            string                         `json:"shmSize,omitempty"`
	TimeoutSeconds     int                            `json:"timeoutSeconds,omitempty"`
	// ConfigHash is the deployer's fingerprint of the job's service config,
	// reported back through actual state for drift/plan comparison.
	ConfigHash string `json:"configHash,omitempty"`
}

// JobsApplyRequest declaratively replaces this node's job set for one
// project/environment: listed jobs are scheduled, absent ones unscheduled.
type JobsApplyRequest struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Jobs        []JobSpec `json:"jobs"`
}

// JobsApplyResponse reports the applied and removed job names.
type JobsApplyResponse struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Applied     []string  `json:"applied,omitempty"`
	Removed     []string  `json:"removed,omitempty"`
	Warnings    []string  `json:"warnings,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// JobTriggerRequest asks for one immediate run of a scheduled job.
type JobTriggerRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Job         string `json:"job"`
}

// JobStatus describes a scheduled job without its env-file content, which
// must never leave the node through list responses.
type JobStatus struct {
	Project        string        `json:"project"`
	Environment    string        `json:"environment"`
	Name           string        `json:"name"`
	Schedule       string        `json:"schedule"`
	Timezone       string        `json:"timezone,omitempty"`
	Image          string        `json:"image"`
	Command        []string      `json:"command"`
	TimeoutSeconds int           `json:"timeoutSeconds,omitempty"`
	ConfigHash     string        `json:"configHash,omitempty"`
	NextRun        *time.Time    `json:"nextRun,omitempty"`
	LastRun        *JobRunRecord `json:"lastRun,omitempty"`
}

// JobRunRecord is one completed (or skipped) run in a job's history.
type JobRunRecord struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Job         string    `json:"job"`
	Trigger     string    `json:"trigger"`
	Container   string    `json:"container,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	DurationMs  int64     `json:"durationMs"`
	ExitCode    int       `json:"exitCode"`
	Status      string    `json:"status"`
	// Output is the bounded tail of the run's combined output.
	Output string `json:"output,omitempty"`
}

// JobScheduler fires job specs with a local cron, mirroring BackupScheduler:
// specs persist as JSON under the data dir and are reloaded on start.
type JobScheduler struct {
	dataDir string
	parser  cron.Parser
	cron    *cron.Cron
	// runJob is the container-execution seam; tests stub it.
	runJob func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error)
	admit  func(...string) error

	mu      sync.Mutex
	entries map[string]cron.EntryID
	specs   map[string]JobSpec
	running map[string]bool

	// runsMu serializes run-history read-modify-write cycles: a skipped-run
	// record can land while the blocking run still owns the running flag.
	runsMu sync.Mutex
}

func NewJobScheduler(dataDir string) *JobScheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &JobScheduler{
		dataDir: dataDir,
		parser:  parser,
		cron:    cron.New(cron.WithParser(parser), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		runJob:  runJobDocker,
		entries: map[string]cron.EntryID{},
		specs:   map[string]JobSpec{},
		running: map[string]bool{},
	}
}

// Run loads persisted specs, starts the cron, and blocks until ctx ends.
func (s *JobScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	if err := s.loadSpecs(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to load jobs: %v\n", err)
	}
	s.cron.Start()
	<-ctx.Done()
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(30 * time.Second):
	}
}

// Apply reconciles this node's job set for one project/environment.
func (s *JobScheduler) Apply(ctx context.Context, request JobsApplyRequest) (*JobsApplyResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("job scheduler is not initialized")
	}
	if !isSafeProjectName(request.Project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(request.Environment) {
		return nil, fmt.Errorf("invalid environment name")
	}
	desired := map[string]JobSpec{}
	var warnings []string
	for i := range request.Jobs {
		spec := request.Jobs[i]
		spec.Project = request.Project
		spec.Environment = request.Environment
		if spec.Image == "" {
			// An unchanged job's deploy sends no image; keep the one the
			// last image-bearing apply left on this node. A job this node
			// has never seen cannot be scheduled yet, but must not fail
			// the apply (a service-scoped deploy still reconciles jobs).
			s.mu.Lock()
			existing, ok := s.specs[jobKey(spec.Project, spec.Environment, spec.Name)]
			s.mu.Unlock()
			if !ok || existing.Image == "" {
				warnings = append(warnings, fmt.Sprintf("job %s skipped: no image on this node yet; deploy the job to schedule it", spec.Name))
				continue
			}
			spec.Image = existing.Image
		}
		if err := validateJobSpec(&spec); err != nil {
			return nil, fmt.Errorf("job %s: %w", spec.Name, err)
		}
		if _, ok := desired[spec.Name]; ok {
			return nil, fmt.Errorf("duplicate job %s", spec.Name)
		}
		desired[spec.Name] = spec
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	existing, err := s.persistedJobNames(request.Project, request.Environment)
	if err != nil {
		return nil, err
	}

	var applied []string
	for name := range desired {
		applied = append(applied, name)
	}
	sort.Strings(applied)
	for _, name := range applied {
		spec := desired[name]
		if spec.FileSetID != "" {
			if err := ensureServiceFileSet(spec.Project, spec.Environment, spec.Name, spec.FileSetID); err != nil {
				return nil, fmt.Errorf("job %s files: %w", spec.Name, err)
			}
		}
		spec.Files = nil
		desired[name] = spec
	}
	for _, name := range applied {
		spec := desired[name]
		if err := s.persistSpec(spec); err != nil {
			return nil, err
		}
		s.mu.Lock()
		err := s.scheduleLocked(spec)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		running := s.running[jobKey(spec.Project, spec.Environment, spec.Name)]
		s.mu.Unlock()
		if !running {
			if err := cleanupServiceFileVersions(spec.Project, spec.Environment, spec.Name, spec.FileSetID); err != nil {
				return nil, fmt.Errorf("failed to clean old job files: %w", err)
			}
		}
	}

	var removed []string
	for _, name := range existing {
		if _, ok := desired[name]; ok {
			continue
		}
		if err := s.removeJob(request.Project, request.Environment, name); err != nil {
			return nil, err
		}
		removed = append(removed, name)
	}
	sort.Strings(removed)

	return &JobsApplyResponse{
		Project:     request.Project,
		Environment: request.Environment,
		Applied:     applied,
		Removed:     removed,
		Warnings:    warnings,
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

// RemoveProject unschedules every job for a project (one environment, or all
// when environment is empty) and deletes its specs and run history.
func (s *JobScheduler) RemoveProject(project string, environment string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	if !isSafeProjectName(project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if environment != "" && !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid environment name")
	}

	s.mu.Lock()
	var targets []JobSpec
	for _, spec := range s.specs {
		if spec.Project != project {
			continue
		}
		if environment != "" && spec.Environment != environment {
			continue
		}
		targets = append(targets, spec)
	}
	s.mu.Unlock()

	var removed []string
	for _, spec := range targets {
		if err := s.removeJob(spec.Project, spec.Environment, spec.Name); err != nil {
			return removed, err
		}
		removed = append(removed, spec.Environment+"/"+spec.Name)
	}
	sort.Strings(removed)

	for _, dir := range []string{jobSpecDirName, jobRunsDirName} {
		path := filepath.Join(s.dataDir, dir, project)
		if environment != "" {
			path = filepath.Join(path, environment)
		}
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("failed to remove job state: %w", err)
		}
		if environment != "" {
			// Prune the project dir when this was its last environment;
			// os.Remove refuses non-empty dirs, which is exactly right.
			_ = os.Remove(filepath.Join(s.dataDir, dir, project))
		}
	}
	return removed, nil
}

// List reports scheduled jobs, optionally filtered by project/environment.
func (s *JobScheduler) List(project string, environment string) []JobStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	type listed struct {
		spec  JobSpec
		entry cron.EntryID
		has   bool
	}
	var snapshot []listed
	for key, spec := range s.specs {
		if project != "" && spec.Project != project {
			continue
		}
		if environment != "" && spec.Environment != environment {
			continue
		}
		entryID, ok := s.entries[key]
		snapshot = append(snapshot, listed{spec: spec, entry: entryID, has: ok})
	}
	s.mu.Unlock()

	statuses := make([]JobStatus, 0, len(snapshot))
	for _, item := range snapshot {
		spec := item.spec
		status := JobStatus{
			Project:        spec.Project,
			Environment:    spec.Environment,
			Name:           spec.Name,
			Schedule:       spec.Schedule,
			Timezone:       spec.Timezone,
			Image:          spec.Image,
			Command:        append([]string(nil), spec.Command...),
			TimeoutSeconds: spec.TimeoutSeconds,
			ConfigHash:     spec.ConfigHash,
		}
		if item.has {
			if next := s.cron.Entry(item.entry).Next; !next.IsZero() {
				nextRun := next.UTC()
				status.NextRun = &nextRun
			}
		}
		if last := s.lastRunRecord(spec.Project, spec.Environment, spec.Name); last != nil {
			last.Output = ""
			status.LastRun = last
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Project != statuses[j].Project {
			return statuses[i].Project < statuses[j].Project
		}
		if statuses[i].Environment != statuses[j].Environment {
			return statuses[i].Environment < statuses[j].Environment
		}
		return statuses[i].Name < statuses[j].Name
	})
	return statuses
}

// Runs returns run history for one job, or every job in the environment
// when job is empty, newest first.
func (s *JobScheduler) Runs(project string, environment string, job string) ([]JobRunRecord, error) {
	if s == nil {
		return nil, fmt.Errorf("job scheduler is not initialized")
	}
	if !isSafeProjectName(project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid environment name")
	}
	names := []string{job}
	if job == "" {
		persisted, err := s.persistedJobRunNames(project, environment)
		if err != nil {
			return nil, err
		}
		names = persisted
	} else if !isSafeServiceName(job) {
		return nil, fmt.Errorf("invalid job name")
	}

	var records []JobRunRecord
	for _, name := range names {
		runs, err := s.readRunRecords(project, environment, name)
		if err != nil {
			return nil, err
		}
		records = append(records, runs...)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})
	return records, nil
}

// Trigger runs a scheduled job immediately, streaming raw output framed by
// the exec markers to stream. An overlapping run surfaces as an error before
// any bytes are streamed.
func (s *JobScheduler) Trigger(ctx context.Context, project string, environment string, job string, stream io.Writer) error {
	if s == nil {
		return fmt.Errorf("job scheduler is not initialized")
	}
	if !isSafeProjectName(project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(job) {
		return fmt.Errorf("invalid job name")
	}
	s.mu.Lock()
	key := jobKey(project, environment, job)
	spec, ok := s.specs[key]
	reserved := ok && !s.running[key]
	if reserved {
		s.running[key] = true
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("job %s is not scheduled for %s/%s on this node", job, project, environment)
	}
	var record JobRunRecord
	if reserved {
		record = s.executeReservedJob(ctx, spec, JobTriggerManual, stream)
	} else {
		record = s.recordSkippedJob(spec, JobTriggerManual)
	}
	if record.Status == JobRunStatusSkipped {
		return fmt.Errorf("job %s is already running; try again after it finishes", job)
	}
	return nil
}

// executeJob runs one job to completion and records the run. A nil stream
// captures output only; a non-nil stream additionally receives raw output
// framed by ExecContainerMarker/ExecExitMarker lines. Overlapping runs are
// skipped and recorded as such without touching the stream.
func (s *JobScheduler) executeJob(ctx context.Context, spec JobSpec, trigger string, stream io.Writer) JobRunRecord {
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	s.mu.Lock()
	if s.running[key] {
		s.mu.Unlock()
		return s.recordSkippedJob(spec, trigger)
	}
	s.running[key] = true
	s.mu.Unlock()
	return s.executeReservedJob(ctx, spec, trigger, stream)
}

func (s *JobScheduler) recordSkippedJob(spec JobSpec, trigger string) JobRunRecord {
	started := time.Now().UTC()
	record := JobRunRecord{
		Project:     spec.Project,
		Environment: spec.Environment,
		Job:         spec.Name,
		Trigger:     trigger,
		StartedAt:   started,
		FinishedAt:  started,
		ExitCode:    -1,
		Status:      JobRunStatusSkipped,
		Output:      "skipped: previous run still in progress",
	}
	s.appendRunRecord(record)
	return record
}

// executeReservedJob runs a job whose running reference was acquired while
// the scheduler spec was still protected by s.mu.
func (s *JobScheduler) executeReservedJob(ctx context.Context, spec JobSpec, trigger string, stream io.Writer) JobRunRecord {
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	started := time.Now().UTC()
	defer func() {
		s.mu.Lock()
		delete(s.running, key)
		current, exists := s.specs[key]
		s.mu.Unlock()
		keep := ""
		if exists {
			keep = current.FileSetID
		}
		_ = cleanupServiceFileVersions(spec.Project, spec.Environment, spec.Name, keep)
	}()
	if s.admit != nil {
		if err := s.admit(s.dataDir); err != nil {
			finished := time.Now().UTC()
			record := JobRunRecord{Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger, StartedAt: started, FinishedAt: finished, DurationMs: finished.Sub(started).Milliseconds(), ExitCode: -1, Status: JobRunStatusFailed, Output: "job denied by resource admission: " + err.Error()}
			s.appendRunRecord(record)
			return record
		}
	}

	timeoutSeconds := spec.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultJobTimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	container := fmt.Sprintf("tako_%s_%s_%s_job_%d", spec.Project, spec.Environment, spec.Name, time.Now().UnixNano())

	capped := newCappedOutputBuffer(jobRunOutputMaxBytes)
	target := io.Writer(capped)
	if stream != nil {
		fmt.Fprintf(stream, "%s%s\n", ExecContainerMarker, container)
		target = io.MultiWriter(stream, capped)
	}
	out := &lineStartWriter{writer: target}

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	exitCode, runErr := s.runJob(runCtx, spec, container, out)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	cancel()
	if timedOut {
		removeExecContainer(container)
	}

	if runErr != nil {
		out.ensureLineStart()
		fmt.Fprintf(out, "job run failed: %v\n", runErr)
	} else if timedOut {
		out.ensureLineStart()
		fmt.Fprintf(out, "job run timed out after %s\n", timeout)
	}
	out.ensureLineStart()
	if stream != nil {
		fmt.Fprintf(stream, "%s%d\n", ExecExitMarker, exitCode)
	}

	status := JobRunStatusSucceeded
	switch {
	case timedOut:
		status = JobRunStatusTimeout
	case runErr != nil || exitCode != 0:
		status = JobRunStatusFailed
	}
	finished := time.Now().UTC()
	record := JobRunRecord{
		Project:     spec.Project,
		Environment: spec.Environment,
		Job:         spec.Name,
		Trigger:     trigger,
		Container:   container,
		StartedAt:   started,
		FinishedAt:  finished,
		DurationMs:  finished.Sub(started).Milliseconds(),
		ExitCode:    exitCode,
		Status:      status,
		Output:      capped.String(),
	}
	s.appendRunRecord(record)
	return record
}

// runScheduledJob is the cron entry point: it resolves the current spec so
// a re-applied job fires with its latest definition.
func (s *JobScheduler) runScheduledJob(key string) {
	s.mu.Lock()
	spec, ok := s.specs[key]
	reserved := ok && !s.running[key]
	if reserved {
		s.running[key] = true
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	var record JobRunRecord
	if reserved {
		record = s.executeReservedJob(context.Background(), spec, JobTriggerSchedule, nil)
	} else {
		record = s.recordSkippedJob(spec, JobTriggerSchedule)
	}
	if record.Status != JobRunStatusSucceeded {
		fmt.Fprintf(os.Stderr, "takod scheduled job %s finished %s (exit %d)\n", key, record.Status, record.ExitCode)
	}
}

// runJobDocker is the production execution seam: a one-off --rm container
// from the job's image, mirroring exec's oneoff mode with the job role label.
func runJobDocker(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error) {
	envFile, cleanup, err := writeTempEnvFile(spec.EnvFileContent, spec.Project, spec.Environment, spec.Name)
	if err != nil {
		return -1, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	return runExecDocker(ctx, output, buildJobRunArgs(spec, container, envFile))
}

func buildJobRunArgs(spec JobSpec, container string, envFile string) []string {
	network := spec.Network
	if network == "" {
		network = fmt.Sprintf("tako_%s_%s", spec.Project, spec.Environment)
	}
	args := []string{
		"run", "--rm",
		"--name", container,
		"--network", network,
		"--label", "tako.project=" + spec.Project,
		"--label", "tako.environment=" + spec.Environment,
		"--label", "tako.service=" + spec.Name,
		"--label", "tako.runtime=takod",
		"--label", jobRoleLabel,
	}
	labelKeys := make([]string, 0, len(spec.Labels))
	for key := range spec.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		args = append(args, "--label", key+"="+spec.Labels[key])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, entry := range spec.Env {
		args = append(args, "-e", entry)
	}
	for _, mount := range spec.Mounts {
		args = append(args, "--mount", mount)
	}
	if spec.MemoryLimit != "" {
		args = append(args, "--memory", spec.MemoryLimit)
	}
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	args = appendContainerRuntimeArgs(args, spec.User, spec.WorkingDir, spec.StopTimeoutSeconds, spec.Init, spec.ExtraHosts, spec.Ulimits, spec.ShmSize)
	if len(spec.Entrypoint) > 0 {
		args = append(args, "--entrypoint", spec.Entrypoint[0])
	}
	args = append(args, spec.Image)
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Command...)
	return args
}

func validateJobSpec(spec *JobSpec) error {
	if !isSafeProjectName(spec.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(spec.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(spec.Name) {
		return fmt.Errorf("invalid job name")
	}
	if strings.TrimSpace(spec.Schedule) == "" {
		return fmt.Errorf("schedule is required")
	}
	if spec.Timezone != "" {
		if hasControlChars(spec.Timezone) || strings.ContainsAny(spec.Timezone, " \t") {
			return fmt.Errorf("invalid timezone")
		}
		if _, err := time.LoadLocation(spec.Timezone); err != nil {
			return fmt.Errorf("invalid timezone: %w", err)
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(jobCronSpec(*spec)); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	if err := validateContainerArgv("command", spec.Command); err != nil {
		return err
	}
	if len(spec.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	if err := validateContainerArgv("entrypoint", spec.Entrypoint); err != nil {
		return err
	}
	if err := validateDockerLabels(spec.Labels); err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}
	for key := range spec.Labels {
		if strings.HasPrefix(key, "tako.") {
			return fmt.Errorf("invalid label: %q uses reserved tako. prefix", key)
		}
	}
	if spec.Image == "" {
		return fmt.Errorf("image is required")
	}
	if err := validateImageName(spec.Image); err != nil {
		return err
	}
	for _, entry := range spec.Env {
		if !strings.Contains(entry, "=") || hasControlChars(entry) {
			return fmt.Errorf("env entries must be KEY=VALUE")
		}
	}
	if spec.Network != "" && !isSafeRuntimeName(spec.Network) {
		return fmt.Errorf("invalid network name")
	}
	for _, mount := range spec.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	if err := validateServiceFileBundles(spec.Files); err != nil {
		return err
	}
	if spec.FileSetID != "" {
		if err := validateServiceFileSetID(spec.FileSetID); err != nil {
			return err
		}
	} else if len(spec.Files) > 0 {
		return fmt.Errorf("fileSetId is required when job files are present")
	}
	if spec.TimeoutSeconds < 0 || spec.TimeoutSeconds > maxExecTimeoutSeconds {
		return fmt.Errorf("timeoutSeconds must be between 0 and %d", maxExecTimeoutSeconds)
	}
	if len(spec.ConfigHash) > 128 || hasControlChars(spec.ConfigHash) {
		return fmt.Errorf("invalid configHash")
	}
	if spec.MemoryLimit != "" && !isSafeDockerMemoryLimit(spec.MemoryLimit) {
		return fmt.Errorf("invalid memory limit")
	}
	if spec.CPULimit != "" && !isSafeDockerCPULimit(spec.CPULimit) {
		return fmt.Errorf("invalid cpu limit")
	}
	if err := validateContainerRuntimeControls(spec.User, spec.WorkingDir, spec.StopTimeoutSeconds, spec.ExtraHosts, spec.Ulimits, spec.ShmSize); err != nil {
		return err
	}
	return nil
}

// jobCronSpec renders the cron spec, carrying the job's timezone via the
// CRON_TZ= prefix robfig/cron parses natively. Jobs default to UTC so a
// schedule means the same thing on every node, unlike server-local time.
func jobCronSpec(spec JobSpec) string {
	timezone := spec.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	return "CRON_TZ=" + timezone + " " + spec.Schedule
}

func (s *JobScheduler) loadSpecs(ctx context.Context) error {
	root := filepath.Join(s.dataDir, jobSpecDirName)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var spec JobSpec
		if err := json.Unmarshal(data, &spec); err != nil {
			return fmt.Errorf("failed to parse job spec %s: %w", path, err)
		}
		if err := validateJobSpec(&spec); err != nil {
			return fmt.Errorf("invalid job spec %s: %w", path, err)
		}
		s.mu.Lock()
		err = s.scheduleLocked(spec)
		s.mu.Unlock()
		return err
	})
}

func (s *JobScheduler) persistSpec(spec JobSpec) error {
	path := jobSpecPath(s.dataDir, spec.Project, spec.Environment, spec.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create job spec directory: %w", err)
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode job spec: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write job spec: %w", err)
	}
	return nil
}

func (s *JobScheduler) scheduleLocked(spec JobSpec) error {
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	if entryID, ok := s.entries[key]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, key)
	}
	entryID, err := s.cron.AddFunc(jobCronSpec(spec), func() {
		s.runScheduledJob(key)
	})
	if err != nil {
		return fmt.Errorf("failed to schedule job: %w", err)
	}
	s.entries[key] = entryID
	s.specs[key] = spec
	return nil
}

func (s *JobScheduler) removeJob(project string, environment string, name string) error {
	key := jobKey(project, environment, name)
	s.mu.Lock()
	if entryID, ok := s.entries[key]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, key)
	}
	delete(s.specs, key)
	running := s.running[key]
	s.mu.Unlock()
	if err := os.Remove(jobSpecPath(s.dataDir, project, environment, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove job spec: %w", err)
	}
	if err := os.Remove(jobRunsPath(s.dataDir, project, environment, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove job run history: %w", err)
	}
	if !running {
		if err := removeServiceFiles(project, environment, name); err != nil {
			return fmt.Errorf("failed to remove job files: %w", err)
		}
	}
	return nil
}

// persistedJobNames lists spec files on disk for one project/environment.
func (s *JobScheduler) persistedJobNames(project string, environment string) ([]string, error) {
	return jobJSONNames(filepath.Join(s.dataDir, jobSpecDirName, project, environment))
}

// persistedJobRunNames lists run-history files for one project/environment.
func (s *JobScheduler) persistedJobRunNames(project string, environment string) ([]string, error) {
	return jobJSONNames(filepath.Join(s.dataDir, jobRunsDirName, project, environment))
}

func jobJSONNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list job state: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

func (s *JobScheduler) appendRunRecord(record JobRunRecord) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	records, err := s.readRunRecords(record.Project, record.Environment, record.Job)
	if err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to read run history: %v\n", err)
		records = nil
	}
	records = append([]JobRunRecord{record}, records...)
	if len(records) > maxJobRunRecords {
		records = records[:maxJobRunRecords]
	}
	path := jobRunsPath(s.dataDir, record.Project, record.Environment, record.Job)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to create run history directory: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to encode run history: %v\n", err)
		return
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to write run history: %v\n", err)
	}
}

func (s *JobScheduler) readRunRecords(project string, environment string, name string) ([]JobRunRecord, error) {
	data, err := os.ReadFile(jobRunsPath(s.dataDir, project, environment, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read job run history: %w", err)
	}
	var records []JobRunRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("failed to parse job run history: %w", err)
	}
	return records, nil
}

func (s *JobScheduler) lastRunRecord(project string, environment string, name string) *JobRunRecord {
	records, err := s.readRunRecords(project, environment, name)
	if err != nil || len(records) == 0 {
		return nil
	}
	record := records[0]
	return &record
}

func jobSpecPath(dataDir string, project string, environment string, name string) string {
	return filepath.Join(dataDir, jobSpecDirName, project, environment, name+".json")
}

func jobRunsPath(dataDir string, project string, environment string, name string) string {
	return filepath.Join(dataDir, jobRunsDirName, project, environment, name+".json")
}

func jobKey(project string, environment string, name string) string {
	return project + "/" + environment + "/" + name
}
