package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/redentordev/tako-cli/pkg/recovery"
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
	JobRunStatusRunning   = "running"
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
	ExternalVolumes    []string                       `json:"externalVolumes,omitempty"`
	VolumeOwnership    []ManagedVolumeOwnership       `json:"volumeOwnership,omitempty"`
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
	AttemptID  string `json:"-"`
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
	ID          string    `json:"id,omitempty"`
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
	runJob             func(ctx context.Context, spec JobSpec, container string, output io.Writer) (int, error)
	admit              func(...string) error
	alerts             *AlertManager
	recoverInterrupted func(context.Context, JobRunRecord) error

	mu             sync.Mutex
	entries        map[string]cron.EntryID
	specs          map[string]JobSpec
	generations    map[string]uint64
	nextGeneration uint64
	running        map[string]bool
	draining       map[string]bool
	recoverable    map[string]bool
	mutating       map[string]int
	runningChanged chan struct{}
	runContext     context.Context
	ready          chan struct{}
	readyOnce      sync.Once
	startupErr     error

	// runsMu serializes run-history read-modify-write cycles: a skipped-run
	// record can land while the blocking run still owns the running flag.
	runsMu sync.Mutex
}

func NewJobScheduler(dataDir string) *JobScheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &JobScheduler{
		dataDir:            dataDir,
		parser:             parser,
		cron:               cron.New(cron.WithParser(parser), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		runJob:             runJobDocker,
		recoverInterrupted: recoverInterruptedJobContainer,
		entries:            map[string]cron.EntryID{},
		specs:              map[string]JobSpec{},
		generations:        map[string]uint64{},
		running:            map[string]bool{},
		draining:           map[string]bool{},
		recoverable:        map[string]bool{},
		mutating:           map[string]int{},
		runningChanged:     make(chan struct{}),
		ready:              make(chan struct{}),
	}
}

func (s *JobScheduler) BeginProjectDrain(ctx context.Context, project string, environment string) (func(), error) {
	wait, release, err := s.StartProjectDrain(project, environment)
	if err != nil {
		return nil, err
	}
	if err := wait(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (s *JobScheduler) StartProjectDrain(project string, environment string) (func(context.Context) error, func(), error) {
	wait, release, _, err := s.startProjectDrain(project, environment)
	return wait, release, err
}

func (s *JobScheduler) startProjectDrain(project string, environment string) (func(context.Context) error, func(), bool, error) {
	if s == nil {
		return func(context.Context) error { return nil }, func() {}, false, nil
	}
	if !isSafeProjectName(project) || (environment != "" && !isSafeRuntimeName(environment)) {
		return nil, nil, false, fmt.Errorf("invalid job drain scope")
	}
	scope := schedulerDrainScope(project, environment)
	s.mu.Lock()
	for existing := range s.draining {
		if strings.HasPrefix(existing, scope) || strings.HasPrefix(scope, existing) {
			if existing == scope && s.recoverable[scope] {
				s.recoverable[scope] = false
				s.mu.Unlock()
				wait, release := s.projectDrainHandles(scope, project, environment)
				return wait, release, true, nil
			}
			s.mu.Unlock()
			return nil, nil, false, fmt.Errorf("job drain already active for overlapping scope")
		}
	}
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "jobs", project, environment)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, false, err
	}
	if !markerExists {
		err = writeSchedulerDrainMarker(s.dataDir, "jobs", project, environment)
	}
	if err != nil {
		s.mu.Unlock()
		return nil, nil, false, err
	}
	recovered := markerExists
	if markerExists {
		s.recoverable[scope] = false
	}
	s.draining[scope] = true
	s.mu.Unlock()
	wait, release := s.projectDrainHandles(scope, project, environment)
	return wait, release, recovered, nil
}

func (s *JobScheduler) projectDrainHandles(scope string, project string, environment string) (func(context.Context) error, func()) {
	wait := func(ctx context.Context) error {
		if ctx == nil {
			ctx = context.Background()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for s.hasActivityScopeLocked(scope) {
			changed := s.runningChanged
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				s.mu.Lock()
				return ctx.Err()
			case <-changed:
			}
			s.mu.Lock()
		}
		return nil
	}
	release := func() {
		if err := removeSchedulerDrainMarker(s.dataDir, "jobs", project, environment); err != nil {
			s.mu.Lock()
			s.recoverable[scope] = true
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "takod failed to clear job drain marker: %v\n", err)
			return
		}
		s.mu.Lock()
		delete(s.draining, scope)
		delete(s.recoverable, scope)
		s.mu.Unlock()
	}
	return wait, release
}

func (s *JobScheduler) MarkProjectDrainRecoverable(project string, environment string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.recoverable[schedulerDrainScope(project, environment)] = true
	s.mu.Unlock()
}

func (s *JobScheduler) ProjectDrainRecoverable(project string, environment string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverable[schedulerDrainScope(project, environment)]
}

func (s *JobScheduler) hasActivityScopeLocked(scope string) bool {
	for key, running := range s.running {
		if running && strings.HasPrefix(key, scope) {
			return true
		}
	}
	for key, count := range s.mutating {
		if count > 0 && strings.HasPrefix(key, scope) {
			return true
		}
	}
	return false
}

func (s *JobScheduler) beginProjectMutation(project string, environment string) (func(), error) {
	scope := schedulerDrainScope(project, environment)
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "jobs", project, environment)
	if err != nil {
		return nil, fmt.Errorf("inspect job drain marker: %w", err)
	}
	if markerExists {
		return nil, fmt.Errorf("%w: job schedules cannot change while durable project cleanup is draining", errSchedulerDraining)
	}
	s.mu.Lock()
	if s.scopeDrainingLocked(scope) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: job schedules cannot change while project cleanup is draining", errSchedulerDraining)
	}
	for active, count := range s.mutating {
		if count > 0 && (strings.HasPrefix(active, scope) || strings.HasPrefix(scope, active)) {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: job schedule mutation already in progress for overlapping scope", errSchedulerMutationInProgress)
		}
	}
	s.mutating[scope] = 1
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.mutating[scope]--
		if s.mutating[scope] == 0 {
			delete(s.mutating, scope)
		}
		s.notifyRunningChangedLocked()
		s.mu.Unlock()
	}, nil
}

func (s *JobScheduler) beginLoaderMutation(ctx context.Context, project string, environment string) (func(), bool, error) {
	for {
		endMutation, err := s.beginProjectMutation(project, environment)
		if err == nil {
			return endMutation, true, nil
		}
		if errors.Is(err, errSchedulerDraining) {
			if s.loaderShouldSuppressForDrain(project, environment) {
				return nil, false, nil
			}
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		if !errors.Is(err, errSchedulerMutationInProgress) {
			return nil, false, err
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *JobScheduler) loaderShouldSuppressForDrain(project string, environment string) bool {
	scope := schedulerDrainScope(project, environment)
	s.mu.Lock()
	hasLiveDrain := false
	for drain := range s.draining {
		if strings.HasPrefix(scope, drain) {
			hasLiveDrain = true
			if s.recoverable[drain] {
				s.mu.Unlock()
				return true
			}
		}
	}
	s.mu.Unlock()
	if hasLiveDrain {
		return false
	}
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "jobs", project, environment)
	return err == nil && markerExists
}

func (s *JobScheduler) scopeDrainingLocked(scope string) bool {
	for drain := range s.draining {
		if strings.HasPrefix(scope, drain) {
			return true
		}
	}
	return false
}

func schedulerDrainScope(project string, environment string) string {
	if environment == "" {
		return project + "/"
	}
	return project + "/" + environment + "/"
}

var (
	errSchedulerDraining           = errors.New("scheduler scope is draining")
	errSchedulerMutationInProgress = errors.New("scheduler mutation is in progress")
)

func schedulerDrainMarkerPath(dataDir string, kind string, project string, environment string) string {
	name := environment
	if name == "" {
		name = "_all"
	}
	return filepath.Join(dataDir, "scheduler-drains", kind, project, name+".draining")
}

func writeSchedulerDrainMarker(dataDir string, kind string, project string, environment string) error {
	path := schedulerDrainMarkerPath(dataDir, kind, project, environment)
	if err := validatePathParentsNoSymlink(dataDir, path, "scheduler drain path"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create scheduler drain marker directory: %w", err)
	}
	if err := writeFileAtomic(path, []byte("draining\n"), 0600); err != nil {
		return fmt.Errorf("write scheduler drain marker: %w", err)
	}
	return nil
}

func removeSchedulerDrainMarker(dataDir string, kind string, project string, environment string) error {
	path := schedulerDrainMarkerPath(dataDir, kind, project, environment)
	if err := validatePathParentsNoSymlink(dataDir, path, "scheduler drain path"); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if _, statErr := os.Stat(filepath.Dir(path)); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}
	}
	return syncDirectory(filepath.Dir(path))
}

func schedulerDrainMarkerExists(dataDir string, kind string, project string, environment string) (bool, error) {
	for _, candidate := range []string{environment, ""} {
		path := schedulerDrainMarkerPath(dataDir, kind, project, candidate)
		if err := validatePathParentsNoSymlink(dataDir, path, "scheduler drain path"); err != nil {
			return false, err
		}
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func (s *JobScheduler) notifyRunningChangedLocked() {
	close(s.runningChanged)
	s.runningChanged = make(chan struct{})
}

// Run loads persisted specs, starts the cron, and blocks until ctx ends.
func (s *JobScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	startupCtx, releaseBarrier, err := acquireBackupRestoreMutationBarrierContext(ctx)
	if err != nil {
		s.startupErr = fmt.Errorf("acquire startup restore barrier: %w", err)
		s.readyOnce.Do(func() { close(s.ready) })
		return
	}
	unlockMutation, err := recovery.AcquireMutationLockContext(startupCtx, s.dataDir)
	if err != nil {
		releaseBarrier()
		s.startupErr = fmt.Errorf("acquire startup snapshot lock: %w", err)
		s.readyOnce.Do(func() { close(s.ready) })
		return
	}
	if err := s.recoverInterruptedRuns(startupCtx); err != nil {
		unlockMutation()
		releaseBarrier()
		s.startupErr = fmt.Errorf("recover interrupted jobs: %w", err)
		s.readyOnce.Do(func() { close(s.ready) })
		return
	}
	if err := s.loadSpecs(startupCtx); err != nil {
		unlockMutation()
		releaseBarrier()
		s.startupErr = fmt.Errorf("load jobs: %w", err)
		s.readyOnce.Do(func() { close(s.ready) })
		return
	}
	unlockMutation()
	releaseBarrier()
	runContext, cancelRuns := context.WithCancel(ctx)
	s.mu.Lock()
	s.runContext = runContext
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
	s.cron.Start()
	<-ctx.Done()
	cancelRuns()
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
	s.mu.Lock()
	s.runContext = nil
	s.mu.Unlock()
}

func (s *JobScheduler) StartupError() error {
	if s == nil {
		return nil
	}
	return s.startupErr
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endMutation, err := s.beginProjectMutation(request.Project, request.Environment)
	if err != nil {
		return nil, err
	}
	defer endMutation()
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
		if err := s.syncAlertSubject(spec); err != nil {
			return nil, fmt.Errorf("job %s alerts: %w", name, err)
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
	scope := schedulerDrainScope(project, environment)
	for key, running := range s.running {
		if running && strings.HasPrefix(key, scope) {
			s.mu.Unlock()
			return nil, fmt.Errorf("cannot remove job schedules while a run is in progress")
		}
	}
	var targets []JobSpec
	for key, spec := range s.specs {
		if spec.Project != project {
			continue
		}
		if environment != "" && spec.Environment != environment {
			continue
		}
		targets = append(targets, spec)
		if entryID, ok := s.entries[key]; ok {
			s.cron.Remove(entryID)
			delete(s.entries, key)
		}
		delete(s.specs, key)
		delete(s.generations, key)
	}
	s.mu.Unlock()

	var removed []string
	for _, spec := range targets {
		removed = append(removed, spec.Environment+"/"+spec.Name)
	}
	sort.Strings(removed)

	var cleanupErr error
	for _, dir := range []string{jobSpecDirName, jobRunsDirName} {
		path := filepath.Join(s.dataDir, dir, project)
		if environment != "" {
			path = filepath.Join(path, environment)
		}
		if err := validatePathParentsNoSymlink(s.dataDir, filepath.Join(path, "state.json"), "job state path"); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to remove job state: %w", err))
		}
		if environment != "" {
			// Prune the project dir when this was its last environment;
			// os.Remove refuses non-empty dirs, which is exactly right.
			_ = os.Remove(filepath.Join(s.dataDir, dir, project))
		}
	}
	for _, spec := range targets {
		if err := removeServiceFiles(spec.Project, spec.Environment, spec.Name); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to remove job files for %s: %w", spec.Name, err))
		}
		if s.alerts != nil {
			if err := s.alerts.RemoveSubject(spec.Project, spec.Environment, "job", spec.Name); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to remove job alert state for %s: %w", spec.Name, err))
			}
		}
	}
	return removed, cleanupErr
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

func (s *JobScheduler) alertSpecs(project, environment string) []JobSpec {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var specs []JobSpec
	for _, spec := range s.specs {
		if spec.Project == project && spec.Environment == environment {
			specs = append(specs, spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
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
	draining := s.scopeDrainingLocked(schedulerDrainScope(project, environment))
	reserved := ok && !draining && !s.running[key]
	if reserved {
		s.running[key] = true
	} else if ok && !draining {
		s.mutating[schedulerDrainScope(spec.Project, spec.Environment)]++
	}
	s.mu.Unlock()
	if draining {
		return fmt.Errorf("job %s cannot start while project cleanup is draining", job)
	}
	if !ok {
		return fmt.Errorf("job %s is not scheduled for %s/%s on this node", job, project, environment)
	}
	var record JobRunRecord
	if reserved {
		record = s.executeReservedJob(ctx, spec, JobTriggerManual, stream)
	} else {
		defer s.endScopeMutation(schedulerDrainScope(spec.Project, spec.Environment))
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
	ctx, releaseBarrier := acquireBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		started := time.Now().UTC()
		record := JobRunRecord{
			ID:      newAlertID(),
			Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger,
			StartedAt: started, FinishedAt: started, ExitCode: -1, Status: JobRunStatusFailed,
			Output: "job blocked by pending volume restore recovery: " + err.Error(),
		}
		s.appendRunRecord(record)
		return record
	}
	key := jobKey(spec.Project, spec.Environment, spec.Name)
	s.mu.Lock()
	if s.running[key] || s.scopeDrainingLocked(schedulerDrainScope(spec.Project, spec.Environment)) {
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
		ID:          newAlertID(),
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
		s.mu.Lock()
		s.notifyRunningChangedLocked()
		s.mu.Unlock()
	}()
	ctx, releaseBarrier := acquireBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		finished := time.Now().UTC()
		record := JobRunRecord{
			ID:      newAlertID(),
			Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger,
			StartedAt: started, FinishedAt: finished, DurationMs: finished.Sub(started).Milliseconds(),
			ExitCode: -1, Status: JobRunStatusFailed, Output: "job blocked by pending volume restore recovery: " + err.Error(),
		}
		s.appendRunRecord(record)
		return record
	}
	if s.admit != nil {
		if err := s.admit(s.dataDir); err != nil {
			finished := time.Now().UTC()
			record := JobRunRecord{Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger, StartedAt: started, FinishedAt: finished, DurationMs: finished.Sub(started).Milliseconds(), ExitCode: -1, Status: JobRunStatusFailed, Output: "job denied by resource admission: " + err.Error()}
			record.ID = newAlertID()
			s.appendRunRecord(record)
			return record
		}
	}

	timeoutSeconds := spec.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultJobTimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	attemptID := newAlertID()
	container := scheduledJobContainerName(spec.Project, spec.Environment, spec.Name, attemptID)
	attempt := JobRunRecord{
		ID: attemptID, Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger,
		Container: container, StartedAt: started, ExitCode: -1, Status: JobRunStatusRunning,
	}
	if err := s.beginRunRecord(attempt); err != nil {
		finished := time.Now().UTC()
		fmt.Fprintf(os.Stderr, "takod job scheduler refused to start untracked job %s: %v\n", key, err)
		return JobRunRecord{
			ID: attemptID, Project: spec.Project, Environment: spec.Environment, Job: spec.Name, Trigger: trigger,
			Container: container, StartedAt: started, FinishedAt: finished, DurationMs: finished.Sub(started).Milliseconds(),
			ExitCode: -1, Status: JobRunStatusFailed, Output: "job did not start because its durable attempt record could not be written",
		}
	}

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
	exitCode := -1
	dockerAttempted := false
	runErr := ensureServiceVolumes(runCtx, ReconcileServiceRequest{
		Project: spec.Project, Environment: spec.Environment, Service: spec.Name,
		Mounts: spec.Mounts, ExternalVolumes: spec.ExternalVolumes, VolumeOwnership: spec.VolumeOwnership, DataDir: s.dataDir,
	})
	if runErr == nil {
		dockerAttempted = true
		runSpec := spec
		runSpec.AttemptID = attemptID
		exitCode, runErr = s.runJob(runCtx, runSpec, container, out)
	}
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	interrupted := runCtx.Err() != nil
	cancel()
	if dockerAttempted && (runErr != nil || interrupted) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := s.recoverInterrupted(cleanupCtx, attempt)
		cleanupCancel()
		if cleanupErr != nil {
			out.ensureLineStart()
			fmt.Fprintf(out, "job helper cleanup deferred to takod restart: %v\n", cleanupErr)
			if stream != nil {
				fmt.Fprintf(stream, "%s%d\n", ExecExitMarker, -1)
			}
			attempt.Output = capped.String()
			return attempt
		}
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
	case interrupted:
		status = JobRunStatusFailed
	case runErr != nil || exitCode != 0:
		status = JobRunStatusFailed
	}
	finished := time.Now().UTC()
	record := JobRunRecord{
		ID:          attemptID,
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
	if err := s.completeRunRecord(record); err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to complete durable run history: %v\n", err)
	}
	return record
}

// runScheduledJob is the cron entry point: it resolves the current spec so
// a re-applied job fires with its latest definition.
func (s *JobScheduler) runScheduledJob(key string, expectedGeneration ...uint64) {
	s.mu.Lock()
	spec, ok := s.specs[key]
	if ok && len(expectedGeneration) > 0 && s.generations[key] != expectedGeneration[0] {
		ok = false
	}
	draining := false
	if ok {
		draining = s.scopeDrainingLocked(schedulerDrainScope(spec.Project, spec.Environment))
	}
	reserved := ok && !draining && !s.running[key]
	runContext := s.runContext
	if runContext == nil {
		runContext = context.Background()
	}
	if reserved {
		s.running[key] = true
	} else if ok && !draining {
		s.mutating[schedulerDrainScope(spec.Project, spec.Environment)]++
	}
	s.mu.Unlock()
	if !ok || draining {
		return
	}
	if !reserved {
		defer s.endScopeMutation(schedulerDrainScope(spec.Project, spec.Environment))
	}
	barrierCtx, releaseBarrier, barrierErr := acquireBackupRestoreMutationBarrierContext(runContext)
	if barrierErr != nil {
		if reserved {
			s.mu.Lock()
			delete(s.running, key)
			s.notifyRunningChangedLocked()
			s.mu.Unlock()
		}
		return
	}
	defer releaseBarrier()
	unlock, err := recovery.AcquireMutationLockContext(barrierCtx, s.dataDir)
	if err != nil {
		if !reserved {
			return
		}
		if barrierCtx.Err() != nil {
			s.mu.Lock()
			delete(s.running, key)
			s.notifyRunningChangedLocked()
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		delete(s.running, key)
		s.notifyRunningChangedLocked()
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "takod scheduled job %s snapshot lock failed: %v\n", key, err)
		return
	}
	defer unlock()
	if !reserved {
		record := s.recordSkippedJob(spec, JobTriggerSchedule)
		fmt.Fprintf(os.Stderr, "takod scheduled job %s finished %s (exit %d)\n", key, record.Status, record.ExitCode)
		return
	}
	record := s.executeReservedJob(barrierCtx, spec, JobTriggerSchedule, nil)
	if record.Status != JobRunStatusSucceeded {
		fmt.Fprintf(os.Stderr, "takod scheduled job %s finished %s (exit %d)\n", key, record.Status, record.ExitCode)
	}
}

func (s *JobScheduler) endScopeMutation(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutating[scope] <= 1 {
		delete(s.mutating, scope)
	} else {
		s.mutating[scope]--
	}
	s.notifyRunningChangedLocked()
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
	if spec.AttemptID != "" {
		args = append(args, "--label", "tako.attempt="+spec.AttemptID)
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
	if err := validateManagedVolumeOwnershipPolicies(spec.Mounts, spec.ExternalVolumes, spec.VolumeOwnership); err != nil {
		return err
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
	if err := validateStorageTreeNoSymlink(root, "job spec storage"); err != nil {
		return err
	}
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
		if !entry.Type().IsRegular() {
			return fmt.Errorf("job spec %s must be a regular file", path)
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
		if filepath.Clean(path) != filepath.Clean(jobSpecPath(s.dataDir, spec.Project, spec.Environment, spec.Name)) {
			return fmt.Errorf("job spec identity does not match path %s", path)
		}
		endMutation, admitted, err := s.beginLoaderMutation(ctx, spec.Project, spec.Environment)
		if err != nil {
			return err
		}
		if !admitted {
			return nil
		}
		freshData, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			endMutation()
			return nil
		}
		if err != nil {
			endMutation()
			return err
		}
		if err := json.Unmarshal(freshData, &spec); err != nil {
			endMutation()
			return fmt.Errorf("failed to reparse job spec %s: %w", path, err)
		}
		if err := validateJobSpec(&spec); err != nil {
			endMutation()
			return fmt.Errorf("invalid refreshed job spec %s: %w", path, err)
		}
		if filepath.Clean(path) != filepath.Clean(jobSpecPath(s.dataDir, spec.Project, spec.Environment, spec.Name)) {
			endMutation()
			return fmt.Errorf("refreshed job spec identity does not match path %s", path)
		}
		s.mu.Lock()
		err = s.scheduleLocked(spec)
		s.mu.Unlock()
		endMutation()
		if err != nil {
			return err
		}
		return s.syncAlertSubject(spec)
	})
}

func (s *JobScheduler) persistSpec(spec JobSpec) error {
	path := jobSpecPath(s.dataDir, spec.Project, spec.Environment, spec.Name)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "job spec path"); err != nil {
		return err
	}
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
	s.nextGeneration++
	generation := s.nextGeneration
	entryID, err := s.cron.AddFunc(jobCronSpec(spec), func() {
		s.runScheduledJob(key, generation)
	})
	if err != nil {
		return fmt.Errorf("failed to schedule job: %w", err)
	}
	s.entries[key] = entryID
	s.specs[key] = spec
	s.generations[key] = generation
	return nil
}

func (s *JobScheduler) removeJob(project string, environment string, name string) error {
	key := jobKey(project, environment, name)
	s.mu.Lock()
	if s.running[key] {
		s.mu.Unlock()
		return fmt.Errorf("job %s cannot be removed while a run is in progress", name)
	}
	if entryID, ok := s.entries[key]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, key)
	}
	delete(s.specs, key)
	delete(s.generations, key)
	s.mu.Unlock()
	specPath := jobSpecPath(s.dataDir, project, environment, name)
	runsPath := jobRunsPath(s.dataDir, project, environment, name)
	if err := validatePathParentsNoSymlink(s.dataDir, specPath, "job spec path"); err != nil {
		return err
	}
	if err := validatePathParentsNoSymlink(s.dataDir, runsPath, "job run path"); err != nil {
		return err
	}
	if err := os.Remove(specPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove job spec: %w", err)
	}
	if err := os.Remove(runsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove job run history: %w", err)
	}
	if err := removeServiceFiles(project, environment, name); err != nil {
		return fmt.Errorf("failed to remove job files: %w", err)
	}
	if s.alerts != nil {
		if err := s.alerts.RemoveSubject(project, environment, "job", name); err != nil {
			return fmt.Errorf("failed to remove job alert state: %w", err)
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
	if record.ID == "" {
		record.ID = jobRunAlertID(record)
	}
	if err := s.persistRunRecord(record, false); err != nil {
		fmt.Fprintf(os.Stderr, "takod job scheduler failed to persist run history: %v\n", err)
	}
}

func (s *JobScheduler) beginRunRecord(record JobRunRecord) error {
	if record.ID == "" {
		return fmt.Errorf("job run ID is required")
	}
	if record.Status != JobRunStatusRunning || record.FinishedAt.IsZero() == false {
		return fmt.Errorf("invalid running job record")
	}
	if err := validateJobRunRecord(&record); err != nil {
		return err
	}
	return s.persistRunRecord(record, false)
}

func (s *JobScheduler) completeRunRecord(record JobRunRecord) error {
	if record.ID == "" || record.Status == JobRunStatusRunning || !validAlertAttemptStatus(record.Status) || record.FinishedAt.IsZero() {
		return fmt.Errorf("invalid completed job record")
	}
	return s.persistRunRecord(record, true)
}

func (s *JobScheduler) persistRunRecord(record JobRunRecord, replace bool) error {
	s.runsMu.Lock()
	records, err := s.readRunRecords(record.Project, record.Environment, record.Job)
	if err != nil {
		s.runsMu.Unlock()
		return err
	}
	replaced := false
	if record.Status == JobRunStatusRunning {
		for _, existing := range records {
			if existing.Status == JobRunStatusRunning && existing.ID != record.ID {
				s.runsMu.Unlock()
				return fmt.Errorf("job already has a durable running attempt")
			}
		}
	}
	if replace {
		for index := range records {
			if records[index].ID == record.ID {
				records[index] = record
				replaced = true
				break
			}
		}
	}
	if !replaced {
		records = append([]JobRunRecord{record}, records...)
	}
	records = boundJobRunRecords(records)
	path := jobRunsPath(s.dataDir, record.Project, record.Environment, record.Job)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "job run path"); err != nil {
		s.runsMu.Unlock()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		s.runsMu.Unlock()
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		s.runsMu.Unlock()
		return err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		s.runsMu.Unlock()
		return err
	}
	s.runsMu.Unlock()
	if s.alerts != nil && record.Trigger == JobTriggerSchedule && record.Status != JobRunStatusRunning {
		if err := s.alerts.Observe(jobRunAlertFact(record)); err != nil {
			return fmt.Errorf("persist job alert fact: %w", err)
		}
	}
	return nil
}

func boundJobRunRecords(records []JobRunRecord) []JobRunRecord {
	running := 0
	for _, record := range records {
		if record.Status == JobRunStatusRunning {
			running++
		}
	}
	terminalBudget := maxJobRunRecords - running
	if terminalBudget < 0 {
		terminalBudget = 0
	}
	bounded := make([]JobRunRecord, 0, min(len(records), maxJobRunRecords))
	for _, record := range records {
		if record.Status == JobRunStatusRunning {
			bounded = append(bounded, record)
		} else if terminalBudget > 0 {
			bounded = append(bounded, record)
			terminalBudget--
		}
	}
	return bounded
}

func (s *JobScheduler) recoverInterruptedRuns(ctx context.Context) error {
	root := filepath.Join(s.dataDir, jobRunsDirName)
	if err := validateStorageTreeNoSymlink(root, "job run storage"); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("job run history %s must be a regular file", path)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var records []JobRunRecord
		if err := json.Unmarshal(data, &records); err != nil {
			return fmt.Errorf("parse job run history %s: %w", path, err)
		}
		for index := range records {
			record := records[index]
			if err := validateJobRunRecord(&record); err != nil || filepath.Clean(path) != filepath.Clean(jobRunsPath(s.dataDir, record.Project, record.Environment, record.Job)) {
				return fmt.Errorf("invalid job run history %s", path)
			}
			if record.Status != JobRunStatusRunning {
				continue
			}
			if s.recoverInterrupted != nil {
				if err := s.recoverInterrupted(ctx, record); err != nil {
					return fmt.Errorf("recover interrupted job %s/%s/%s: %w", record.Project, record.Environment, record.Job, err)
				}
			}
			record.Status = JobRunStatusFailed
			record.FinishedAt = time.Now().UTC()
			record.DurationMs = record.FinishedAt.Sub(record.StartedAt).Milliseconds()
			record.ExitCode = -1
			record.Output = "job interrupted by takod restart"
			if err := s.completeRunRecord(record); err != nil {
				return err
			}
		}
		return nil
	})
}

func recoverInterruptedJobContainer(ctx context.Context, record JobRunRecord) error {
	if record.Container == "" {
		return nil
	}
	if !isSafeContainerName(record.Container) {
		return fmt.Errorf("invalid interrupted job container name")
	}
	output, err := runDocker(ctx, "ps", "-a", "--filter", "name=^"+record.Container+"$", "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("inspect interrupted job container: %w, output: %s", err, output)
	}
	if strings.TrimSpace(output) != record.Container {
		return nil
	}
	if err := verifyRecoveryContainerLabels(ctx, record.Container, map[string]string{
		"tako.project": record.Project, "tako.environment": record.Environment, "tako.service": record.Job,
		"tako.runtime": "takod", "tako.role": "job", "tako.attempt": record.ID,
	}); err != nil {
		return err
	}
	output, err = runDocker(ctx, "rm", "-f", record.Container)
	if err != nil {
		return fmt.Errorf("remove interrupted job container: %w, output: %s", err, output)
	}
	return nil
}

func verifyRecoveryContainerLabels(ctx context.Context, container string, expected map[string]string) error {
	output, err := runDocker(ctx, "inspect", container, "--format", "{{json .Config.Labels}}")
	if err != nil {
		return fmt.Errorf("inspect interrupted container identity: %w, output: %s", err, output)
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &labels); err != nil {
		return fmt.Errorf("inspect interrupted container identity: invalid labels")
	}
	for key, value := range expected {
		if labels[key] != value {
			return fmt.Errorf("interrupted container %s identity mismatch", container)
		}
	}
	return nil
}

func (s *JobScheduler) syncAlertSubject(spec JobSpec) error {
	if s.alerts == nil {
		return nil
	}
	if err := s.alerts.RegisterSubject(spec.Project, spec.Environment, "job", spec.Name); err != nil {
		return err
	}
	records, err := s.readRunRecords(spec.Project, spec.Environment, spec.Name)
	if err != nil {
		return err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Trigger != JobTriggerSchedule || records[index].Status == JobRunStatusRunning {
			continue
		}
		if records[index].ID == "" {
			records[index].ID = jobRunAlertID(records[index])
		}
		if err := s.alerts.Observe(jobRunAlertFact(records[index])); err != nil {
			return err
		}
	}
	return nil
}

func jobRunAlertFact(record JobRunRecord) AlertAttemptFact {
	return AlertAttemptFact{ID: record.ID, Project: record.Project, Environment: record.Environment, Kind: "job", Service: record.Job, Status: record.Status, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt}
}

func jobRunAlertID(record JobRunRecord) string {
	return fmt.Sprintf("job-%d-%d-%s", record.StartedAt.UnixNano(), record.FinishedAt.UnixNano(), record.Status)
}

func (s *JobScheduler) readRunRecords(project string, environment string, name string) ([]JobRunRecord, error) {
	path := jobRunsPath(s.dataDir, project, environment, name)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "job run path"); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read job run history: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("job run history must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read job run history: %w", err)
	}
	var records []JobRunRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("failed to parse job run history: %w", err)
	}
	for index := range records {
		if err := validateJobRunRecord(&records[index]); err != nil || records[index].Project != project || records[index].Environment != environment || records[index].Job != name {
			return nil, fmt.Errorf("invalid job run history")
		}
	}
	return records, nil
}

func validateJobRunRecord(record *JobRunRecord) error {
	if record == nil || !isSafeProjectName(record.Project) || !isSafeRuntimeName(record.Environment) || !isSafeServiceName(record.Job) || (record.Trigger != JobTriggerSchedule && record.Trigger != JobTriggerManual) || record.StartedAt.IsZero() {
		return fmt.Errorf("invalid job run identity")
	}
	if len(record.ID) > 128 || hasControlChars(record.ID) {
		return fmt.Errorf("invalid job run ID")
	}
	if record.Container != "" && !isSafeContainerName(record.Container) {
		return fmt.Errorf("invalid job run container")
	}
	if record.Status == JobRunStatusRunning {
		if record.ID == "" || !record.FinishedAt.IsZero() || record.Container != scheduledJobContainerName(record.Project, record.Environment, record.Job, record.ID) {
			return fmt.Errorf("invalid running job record")
		}
		return nil
	}
	if !validAlertAttemptStatus(record.Status) || record.FinishedAt.IsZero() || record.StartedAt.After(record.FinishedAt) {
		return fmt.Errorf("invalid completed job record")
	}
	if record.ID == "" {
		record.ID = jobRunAlertID(*record)
	}
	return nil
}

func scheduledJobContainerName(project, environment, job, attemptID string) string {
	return schedulerContainerName("job", project, environment, job, attemptID)
}

func schedulerContainerName(role, project, environment, service, attemptID string) string {
	component := func(value string) string {
		if len(value) > 20 {
			return value[:20]
		}
		return value
	}
	identity := attemptID
	if len(identity) > 12 {
		identity = identity[:12]
	}
	sum := sha256.Sum256([]byte(project + "\x00" + environment + "\x00" + service + "\x00" + role + "\x00" + attemptID))
	return fmt.Sprintf("tako_%s_%s_%s_%s_%s_%s", component(project), component(environment), component(service), role, identity, hex.EncodeToString(sum[:8]))
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
