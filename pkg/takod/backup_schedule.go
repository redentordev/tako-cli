package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/recovery"
	"github.com/robfig/cron/v3"
)

const (
	backupScheduleDirName    = "backup-schedules"
	backupRunsDirName        = "backup-runs"
	maxBackupRunRecords      = 50
	maxBackupScheduleVolumes = 256
)

type BackupScheduleRequest struct {
	Project       string                 `json:"project"`
	Environment   string                 `json:"environment"`
	Service       string                 `json:"service"`
	Schedule      string                 `json:"schedule"`
	RetentionDays int                    `json:"retentionDays,omitempty"`
	Volumes       []BackupScheduleVolume `json:"volumes"`
	Storage       *BackupStorageConfig   `json:"storage,omitempty"`
}

type BackupScheduleVolume struct {
	Volume         string `json:"volume"`
	DockerVolume   string `json:"dockerVolume"`
	ExternalVolume bool   `json:"externalVolume,omitempty"`
}

type BackupScheduleResponse struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Service     string    `json:"service"`
	Scheduled   bool      `json:"scheduled"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type BackupRunRecord struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Service     string    `json:"service"`
	BackupID    string    `json:"backupId,omitempty"`
	Containers  []string  `json:"containers,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	Status      string    `json:"status"`
}

type BackupScheduler struct {
	dataDir            string
	parser             cron.Parser
	cron               *cron.Cron
	admit              func(...string) error
	alerts             *AlertManager
	createBackup       func(context.Context, BackupRequest) (*BackupInfo, error)
	cleanupBackups     func(context.Context, BackupRequest) (*BackupCleanupResponse, error)
	recoverInterrupted func(context.Context, BackupRunRecord) error

	mu             sync.Mutex
	entries        map[string]cron.EntryID
	schedules      map[string]BackupScheduleRequest
	generations    map[string]uint64
	nextGeneration uint64
	running        map[string]bool
	draining       map[string]bool
	recoverable    map[string]bool
	mutating       map[string]int
	runningChanged chan struct{}
	runContext     context.Context
	runsMu         sync.Mutex
	ready          chan struct{}
	readyOnce      sync.Once
	startupErr     error
}

func NewBackupScheduler(dataDir string) *BackupScheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &BackupScheduler{
		dataDir:            dataDir,
		parser:             parser,
		cron:               cron.New(cron.WithParser(parser), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		createBackup:       CreateVolumeBackup,
		cleanupBackups:     CleanupOldBackups,
		recoverInterrupted: recoverInterruptedBackupContainers,
		entries:            map[string]cron.EntryID{},
		schedules:          map[string]BackupScheduleRequest{},
		generations:        map[string]uint64{},
		running:            map[string]bool{},
		draining:           map[string]bool{},
		recoverable:        map[string]bool{},
		mutating:           map[string]int{},
		runningChanged:     make(chan struct{}),
		ready:              make(chan struct{}),
	}
}

func (s *BackupScheduler) BeginProjectDrain(ctx context.Context, project string, environment string) (func(), error) {
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

func (s *BackupScheduler) StartProjectDrain(project string, environment string) (func(context.Context) error, func(), error) {
	wait, release, _, err := s.startProjectDrain(project, environment)
	return wait, release, err
}

func (s *BackupScheduler) startProjectDrain(project string, environment string) (func(context.Context) error, func(), bool, error) {
	if s == nil {
		return func(context.Context) error { return nil }, func() {}, false, nil
	}
	if !isSafeProjectName(project) || (environment != "" && !isSafeRuntimeName(environment)) {
		return nil, nil, false, fmt.Errorf("invalid backup drain scope")
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
			return nil, nil, false, fmt.Errorf("backup drain already active for overlapping scope")
		}
	}
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "backups", project, environment)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, false, err
	}
	if !markerExists {
		err = writeSchedulerDrainMarker(s.dataDir, "backups", project, environment)
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

func (s *BackupScheduler) projectDrainHandles(scope string, project string, environment string) (func(context.Context) error, func()) {
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
		if err := removeSchedulerDrainMarker(s.dataDir, "backups", project, environment); err != nil {
			s.mu.Lock()
			s.recoverable[scope] = true
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "takod failed to clear backup drain marker: %v\n", err)
			return
		}
		s.mu.Lock()
		delete(s.draining, scope)
		delete(s.recoverable, scope)
		s.mu.Unlock()
	}
	return wait, release
}

func (s *BackupScheduler) MarkProjectDrainRecoverable(project string, environment string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.recoverable[schedulerDrainScope(project, environment)] = true
	s.mu.Unlock()
}

func (s *BackupScheduler) ProjectDrainRecoverable(project string, environment string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverable[schedulerDrainScope(project, environment)]
}

func (s *BackupScheduler) hasActivityScopeLocked(scope string) bool {
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

func (s *BackupScheduler) beginProjectMutation(project string, environment string) (func(), error) {
	scope := schedulerDrainScope(project, environment)
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "backups", project, environment)
	if err != nil {
		return nil, fmt.Errorf("inspect backup drain marker: %w", err)
	}
	if markerExists {
		return nil, fmt.Errorf("%w: backup schedules cannot change while durable project cleanup is draining", errSchedulerDraining)
	}
	s.mu.Lock()
	for drain := range s.draining {
		if strings.HasPrefix(scope, drain) {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: backup schedules cannot change while project cleanup is draining", errSchedulerDraining)
		}
	}
	for active, count := range s.mutating {
		if count > 0 && (strings.HasPrefix(active, scope) || strings.HasPrefix(scope, active)) {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: backup schedule mutation already in progress for overlapping scope", errSchedulerMutationInProgress)
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

func (s *BackupScheduler) beginLoaderMutation(ctx context.Context, project string, environment string) (func(), bool, error) {
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

func (s *BackupScheduler) loaderShouldSuppressForDrain(project string, environment string) bool {
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
	markerExists, err := schedulerDrainMarkerExists(s.dataDir, "backups", project, environment)
	return err == nil && markerExists
}

func (s *BackupScheduler) notifyRunningChangedLocked() {
	close(s.runningChanged)
	s.runningChanged = make(chan struct{})
}

func (s *BackupScheduler) Run(ctx context.Context) {
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
		s.startupErr = fmt.Errorf("recover interrupted backups: %w", err)
		s.readyOnce.Do(func() { close(s.ready) })
		return
	}
	if err := s.loadSchedules(startupCtx); err != nil {
		unlockMutation()
		releaseBarrier()
		s.startupErr = fmt.Errorf("load backup schedules: %w", err)
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

func (s *BackupScheduler) StartupError() error {
	if s == nil {
		return nil
	}
	return s.startupErr
}

func (s *BackupScheduler) Upsert(ctx context.Context, request BackupScheduleRequest) (*BackupScheduleResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("backup scheduler is not initialized")
	}
	request = normalizeBackupScheduleRequest(request)
	if err := validateBackupScheduleRequest(request); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endMutation, err := s.beginProjectMutation(request.Project, request.Environment)
	if err != nil {
		return nil, err
	}
	defer endMutation()
	if err := s.persistSchedule(request); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.scheduleLocked(request); err != nil {
		return nil, err
	}
	if err := s.syncAlertSubject(request); err != nil {
		return nil, fmt.Errorf("backup alerts: %w", err)
	}
	return &BackupScheduleResponse{
		Project:     request.Project,
		Environment: request.Environment,
		Service:     request.Service,
		Scheduled:   true,
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

func (s *BackupScheduler) Delete(ctx context.Context, project string, environment string, service string) (*BackupScheduleResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("backup scheduler is not initialized")
	}
	request := BackupScheduleRequest{Project: project, Environment: environment, Service: service}
	if !isSafeProjectName(request.Project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(request.Environment) {
		return nil, fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(request.Service) {
		return nil, fmt.Errorf("invalid service name")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endMutation, err := s.beginProjectMutation(request.Project, request.Environment)
	if err != nil {
		return nil, err
	}
	defer endMutation()
	s.mu.Lock()
	if s.running[backupScheduleKey(request)] {
		s.mu.Unlock()
		return nil, fmt.Errorf("backup schedule %s cannot be removed while a backup is in progress", service)
	}
	if entryID, ok := s.entries[backupScheduleKey(request)]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, backupScheduleKey(request))
	}
	delete(s.generations, backupScheduleKey(request))
	delete(s.schedules, backupScheduleKey(request))
	s.mu.Unlock()
	schedulePath := backupSchedulePath(s.dataDir, request)
	runsPath := backupRunsPath(s.dataDir, project, environment, service)
	if err := validatePathParentsNoSymlink(s.dataDir, schedulePath, "backup schedule path"); err != nil {
		return nil, err
	}
	if err := validatePathParentsNoSymlink(s.dataDir, runsPath, "backup run path"); err != nil {
		return nil, err
	}
	if err := os.Remove(schedulePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove backup schedule: %w", err)
	}
	if err := os.Remove(runsPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove backup run history: %w", err)
	}
	if s.alerts != nil {
		if err := s.alerts.RemoveSubject(project, environment, "backup", service); err != nil {
			return nil, fmt.Errorf("failed to remove backup alert state: %w", err)
		}
	}
	return &BackupScheduleResponse{
		Project:     request.Project,
		Environment: request.Environment,
		Service:     request.Service,
		Scheduled:   false,
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

// RemoveProject unschedules and deletes all backup schedules for one project
// scope before project containers are drained.
func (s *BackupScheduler) RemoveProject(project string, environment string) (int, error) {
	if s == nil {
		return 0, nil
	}
	if !isSafeProjectName(project) {
		return 0, fmt.Errorf("invalid project name")
	}
	if environment != "" && !isSafeRuntimeName(environment) {
		return 0, fmt.Errorf("invalid environment name")
	}
	prefix := project + "/"
	if environment != "" {
		prefix += environment + "/"
	}
	removed := 0
	var removedSchedules []BackupScheduleRequest
	s.mu.Lock()
	for key, running := range s.running {
		if running && strings.HasPrefix(key, prefix) {
			s.mu.Unlock()
			return 0, fmt.Errorf("cannot remove backup schedules while a backup is in progress")
		}
	}
	for key, entryID := range s.entries {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		s.cron.Remove(entryID)
		if request, ok := s.schedules[key]; ok {
			removedSchedules = append(removedSchedules, request)
		}
		delete(s.entries, key)
		delete(s.generations, key)
		delete(s.schedules, key)
		removed++
	}
	s.mu.Unlock()

	path := filepath.Join(s.dataDir, backupScheduleDirName, project)
	if environment != "" {
		path = filepath.Join(path, environment)
	}
	if err := validatePathParentsNoSymlink(s.dataDir, filepath.Join(path, "schedule.json"), "backup schedule path"); err != nil {
		return removed, err
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return removed, fmt.Errorf("failed to remove backup schedules: %w", err)
	}
	runsPath := filepath.Join(s.dataDir, backupRunsDirName, project)
	if environment != "" {
		runsPath = filepath.Join(runsPath, environment)
	}
	if err := validatePathParentsNoSymlink(s.dataDir, filepath.Join(runsPath, "runs.json"), "backup run path"); err != nil {
		return removed, err
	}
	if err := os.RemoveAll(runsPath); err != nil && !os.IsNotExist(err) {
		return removed, fmt.Errorf("failed to remove backup run history: %w", err)
	}
	if s.alerts != nil {
		for _, request := range removedSchedules {
			if err := s.alerts.RemoveSubject(request.Project, request.Environment, "backup", request.Service); err != nil {
				return removed, fmt.Errorf("failed to remove backup alert state: %w", err)
			}
		}
	}
	return removed, nil
}

func (s *BackupScheduler) alertSchedules(project, environment string) []BackupScheduleRequest {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var requests []BackupScheduleRequest
	for _, request := range s.schedules {
		if request.Project == project && request.Environment == environment {
			requests = append(requests, request)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Service < requests[j].Service })
	return requests
}

func (s *BackupScheduler) loadSchedules(ctx context.Context) error {
	root := filepath.Join(s.dataDir, backupScheduleDirName)
	if err := validateStorageTreeNoSymlink(root, "backup schedule storage"); err != nil {
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
			return fmt.Errorf("backup schedule %s must be a regular file", path)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var request BackupScheduleRequest
		if err := json.Unmarshal(data, &request); err != nil {
			return fmt.Errorf("failed to parse backup schedule %s: %w", path, err)
		}
		request = normalizeBackupScheduleRequest(request)
		if err := validateBackupScheduleRequest(request); err != nil {
			return fmt.Errorf("invalid backup schedule %s: %w", path, err)
		}
		if filepath.Clean(path) != filepath.Clean(backupSchedulePath(s.dataDir, request)) {
			return fmt.Errorf("backup schedule identity does not match path %s", path)
		}
		endMutation, admitted, err := s.beginLoaderMutation(ctx, request.Project, request.Environment)
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
		if err := json.Unmarshal(freshData, &request); err != nil {
			endMutation()
			return fmt.Errorf("failed to reparse backup schedule %s: %w", path, err)
		}
		request = normalizeBackupScheduleRequest(request)
		if err := validateBackupScheduleRequest(request); err != nil {
			endMutation()
			return fmt.Errorf("invalid refreshed backup schedule %s: %w", path, err)
		}
		if filepath.Clean(path) != filepath.Clean(backupSchedulePath(s.dataDir, request)) {
			endMutation()
			return fmt.Errorf("refreshed backup schedule identity does not match path %s", path)
		}
		s.mu.Lock()
		err = s.scheduleLocked(request)
		s.mu.Unlock()
		endMutation()
		if err != nil {
			return err
		}
		return s.syncAlertSubject(request)
	})
}

func (s *BackupScheduler) persistSchedule(request BackupScheduleRequest) error {
	path := backupSchedulePath(s.dataDir, request)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "backup schedule path"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create backup schedule directory: %w", err)
	}
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode backup schedule: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write backup schedule: %w", err)
	}
	return nil
}

func (s *BackupScheduler) scheduleLocked(request BackupScheduleRequest) error {
	key := backupScheduleKey(request)
	if entryID, ok := s.entries[key]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, key)
	}
	s.nextGeneration++
	generation := s.nextGeneration
	entryID, err := s.cron.AddFunc(request.Schedule, func() {
		s.runScheduledBackup(request, generation)
	})
	if err != nil {
		return fmt.Errorf("failed to schedule backup: %w", err)
	}
	s.entries[key] = entryID
	s.generations[key] = generation
	s.schedules[key] = request
	return nil
}

func (s *BackupScheduler) runScheduledBackup(request BackupScheduleRequest, expectedGeneration ...uint64) {
	key := backupScheduleKey(request)
	started := time.Now().UTC()
	record := func(status, backupID string) {
		finished := time.Now().UTC()
		if err := s.appendBackupRunRecord(BackupRunRecord{
			ID: newAlertID(), Project: request.Project, Environment: request.Environment, Service: request.Service,
			BackupID: backupID, StartedAt: started, FinishedAt: finished, Status: status,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "takod backup scheduler failed to persist run fact for %s: %v\n", key, err)
		}
	}
	s.mu.Lock()
	if len(expectedGeneration) > 0 && s.generations[key] != expectedGeneration[0] {
		s.mu.Unlock()
		return
	}
	draining := false
	for drain := range s.draining {
		if strings.HasPrefix(schedulerDrainScope(request.Project, request.Environment), drain) {
			draining = true
			break
		}
	}
	if draining {
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "takod backup schedule skipped while project cleanup is draining: %s\n", key)
		return
	}
	overlap := s.running[key]
	if overlap {
		s.mutating[schedulerDrainScope(request.Project, request.Environment)]++
	}
	runContext := s.runContext
	if runContext == nil {
		runContext = context.Background()
	}
	if !overlap {
		s.running[key] = true
	}
	s.mu.Unlock()
	if overlap {
		defer func() {
			s.mu.Lock()
			scope := schedulerDrainScope(request.Project, request.Environment)
			s.mutating[scope]--
			if s.mutating[scope] == 0 {
				delete(s.mutating, scope)
			}
			s.notifyRunningChangedLocked()
			s.mu.Unlock()
		}()
	} else {
		defer func() {
			s.mu.Lock()
			delete(s.running, key)
			s.notifyRunningChangedLocked()
			s.mu.Unlock()
		}()
	}
	barrierCtx, releaseBarrier, barrierErr := acquireBackupRestoreMutationBarrierContext(runContext)
	if barrierErr != nil {
		return
	}
	defer releaseBarrier()
	unlock, err := recovery.AcquireMutationLockContext(barrierCtx, s.dataDir)
	if err != nil {
		if barrierCtx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "takod backup schedule snapshot lock failed for %s: %v\n", key, err)
		return
	}
	defer unlock()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		fmt.Fprintf(os.Stderr, "takod backup schedule blocked by restore recovery for %s: %v\n", key, err)
		if !overlap {
			record(JobRunStatusFailed, "")
		}
		return
	}
	if overlap {
		fmt.Fprintf(os.Stderr, "takod backup schedule skipped overlapping run: %s\n", key)
		record(JobRunStatusSkipped, "")
		return
	}
	ctx, cancel := context.WithTimeout(barrierCtx, 6*time.Hour)
	defer cancel()
	backupID := backupIDForRequest(BackupRequest{}, time.Now())
	attempt := BackupRunRecord{
		ID: newAlertID(), Project: request.Project, Environment: request.Environment, Service: request.Service,
		BackupID: backupID, StartedAt: started, Status: JobRunStatusRunning,
	}
	for index := range request.Volumes {
		attempt.Containers = append(attempt.Containers, scheduledBackupContainerName(request, attempt.ID, index))
	}
	if err := s.beginBackupRunRecord(attempt); err != nil {
		fmt.Fprintf(os.Stderr, "takod backup scheduler refused to start untracked backup %s: %v\n", key, err)
		return
	}
	failed := false
	uncertainHelper := false
	for index, volume := range request.Volumes {
		if s.admit != nil {
			if err := s.admit(backupRootDir); err != nil {
				fmt.Fprintf(os.Stderr, "takod backup schedule denied by resource admission for %s volume %s: %v\n", key, volume.Volume, err)
				failed = true
				continue
			}
		}
		info, err := s.createBackup(ctx, BackupRequest{
			Project:        request.Project,
			Environment:    request.Environment,
			Volume:         volume.Volume,
			DockerVolume:   volume.DockerVolume,
			ExternalVolume: volume.ExternalVolume,
			BackupID:       backupID,
			RetentionDays:  request.RetentionDays,
			Storage:        request.Storage,
			ContainerName:  attempt.Containers[index],
			Service:        request.Service,
			AttemptID:      attempt.ID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduled backup failed for %s/%s/%s volume %s: %v\n", request.Project, request.Environment, request.Service, volume.Volume, err)
			failed = true
			uncertainHelper = true
		}
		if info != nil {
			for _, warning := range info.Warnings {
				fmt.Fprintf(os.Stderr, "takod scheduled backup warning for %s/%s/%s volume %s: %s\n", request.Project, request.Environment, request.Service, volume.Volume, warning)
			}
		}
		if request.Storage != nil && (info == nil || info.Remote == nil || info.Remote.VerifiedAt.IsZero()) {
			failed = true
		}
		if request.RetentionDays <= 0 {
			continue
		}
		if _, err := s.cleanupBackups(ctx, BackupRequest{
			Project:       request.Project,
			Environment:   request.Environment,
			Volume:        volume.Volume,
			RetentionDays: request.RetentionDays,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduled backup cleanup failed for %s/%s/%s volume %s: %v\n", request.Project, request.Environment, request.Service, volume.Volume, err)
		}
	}
	if ctx.Err() != nil {
		uncertainHelper = true
	}
	if uncertainHelper {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := s.recoverInterrupted(cleanupCtx, attempt)
		cleanupCancel()
		if cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "takod scheduled backup helper cleanup deferred to restart for %s: %v\n", key, cleanupErr)
			return
		}
	}
	status := JobRunStatusSucceeded
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status = JobRunStatusTimeout
	} else if ctx.Err() != nil || failed {
		status = JobRunStatusFailed
	}
	attempt.Status = status
	attempt.FinishedAt = time.Now().UTC()
	attempt.Containers = nil
	if err := s.completeBackupRunRecord(attempt); err != nil {
		fmt.Fprintf(os.Stderr, "takod backup scheduler failed to complete durable run history for %s: %v\n", key, err)
	}
}

func normalizeBackupScheduleRequest(request BackupScheduleRequest) BackupScheduleRequest {
	request.RetentionDays = normalizeRetentionDays(request.RetentionDays)
	if request.Storage != nil {
		normalized := normalizeBackupStorage(*request.Storage)
		request.Storage = &normalized
	}
	return request
}

func validateBackupScheduleRequest(request BackupScheduleRequest) error {
	if !isSafeProjectName(request.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(request.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(request.Service) {
		return fmt.Errorf("invalid service name")
	}
	if request.Schedule == "" {
		return fmt.Errorf("backup schedule is required")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(request.Schedule); err != nil {
		return fmt.Errorf("invalid backup schedule: %w", err)
	}
	if request.RetentionDays < 0 {
		return fmt.Errorf("retentionDays cannot be negative")
	}
	if len(request.Volumes) == 0 || len(request.Volumes) > maxBackupScheduleVolumes {
		return fmt.Errorf("backup volumes must contain between 1 and %d entries", maxBackupScheduleVolumes)
	}
	for _, volume := range request.Volumes {
		if !isSafeBackupVolume(volume.Volume) {
			return fmt.Errorf("invalid backup volume")
		}
		if !isSafeDockerVolumeName(volume.DockerVolume) {
			return fmt.Errorf("invalid backup docker volume")
		}
	}
	if request.Storage != nil {
		if err := ValidateBackupStorage(*request.Storage); err != nil {
			return err
		}
	}
	return nil
}

func (s *BackupScheduler) appendBackupRunRecord(record BackupRunRecord) error {
	return s.persistBackupRunRecord(record, false)
}

func (s *BackupScheduler) beginBackupRunRecord(record BackupRunRecord) error {
	if record.ID == "" || record.Status != JobRunStatusRunning || !record.FinishedAt.IsZero() {
		return fmt.Errorf("invalid running backup record")
	}
	if err := validateBackupRunRecord(record, record.Project, record.Environment, record.Service); err != nil {
		return err
	}
	return s.persistBackupRunRecord(record, false)
}

func (s *BackupScheduler) completeBackupRunRecord(record BackupRunRecord) error {
	if record.ID == "" || !validAlertAttemptStatus(record.Status) || record.FinishedAt.IsZero() {
		return fmt.Errorf("invalid completed backup record")
	}
	return s.persistBackupRunRecord(record, true)
}

func (s *BackupScheduler) persistBackupRunRecord(record BackupRunRecord, replace bool) error {
	s.runsMu.Lock()
	records, err := s.readBackupRunRecords(record.Project, record.Environment, record.Service)
	if err != nil {
		s.runsMu.Unlock()
		return err
	}
	replaced := false
	if record.Status == JobRunStatusRunning {
		for _, existing := range records {
			if existing.Status == JobRunStatusRunning && existing.ID != record.ID {
				s.runsMu.Unlock()
				return fmt.Errorf("backup schedule already has a durable running attempt")
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
		records = append([]BackupRunRecord{record}, records...)
	}
	records = boundBackupRunRecords(records)
	path := backupRunsPath(s.dataDir, record.Project, record.Environment, record.Service)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "backup run path"); err != nil {
		s.runsMu.Unlock()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		s.runsMu.Unlock()
		return fmt.Errorf("create backup run history directory: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		s.runsMu.Unlock()
		return fmt.Errorf("encode backup run history: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		s.runsMu.Unlock()
		return fmt.Errorf("write backup run history: %w", err)
	}
	s.runsMu.Unlock()
	if s.alerts != nil && record.Status != JobRunStatusRunning {
		if err := s.alerts.Observe(backupRunAlertFact(record)); err != nil {
			return fmt.Errorf("persist backup alert fact: %w", err)
		}
	}
	return nil
}

func boundBackupRunRecords(records []BackupRunRecord) []BackupRunRecord {
	running := 0
	for _, record := range records {
		if record.Status == JobRunStatusRunning {
			running++
		}
	}
	terminalBudget := maxBackupRunRecords - running
	if terminalBudget < 0 {
		terminalBudget = 0
	}
	bounded := make([]BackupRunRecord, 0, min(len(records), maxBackupRunRecords))
	for _, record := range records {
		if record.Status == JobRunStatusRunning {
			bounded = append(bounded, record)
		} else if terminalBudget > 0 {
			record.Containers = nil
			bounded = append(bounded, record)
			terminalBudget--
		}
	}
	return bounded
}

func (s *BackupScheduler) recoverInterruptedRuns(ctx context.Context) error {
	root := filepath.Join(s.dataDir, backupRunsDirName)
	if err := validateStorageTreeNoSymlink(root, "backup run storage"); err != nil {
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
			return fmt.Errorf("backup run history %s must be a regular file", path)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var records []BackupRunRecord
		if err := json.Unmarshal(data, &records); err != nil {
			return fmt.Errorf("parse backup run history %s: %w", path, err)
		}
		for index := range records {
			record := records[index]
			if err := validateBackupRunRecord(record, record.Project, record.Environment, record.Service); err != nil || filepath.Clean(path) != filepath.Clean(backupRunsPath(s.dataDir, record.Project, record.Environment, record.Service)) {
				return fmt.Errorf("invalid backup run history %s", path)
			}
			if record.Status != JobRunStatusRunning {
				continue
			}
			if s.recoverInterrupted != nil {
				if err := s.recoverInterrupted(ctx, record); err != nil {
					return fmt.Errorf("recover interrupted backup %s/%s/%s: %w", record.Project, record.Environment, record.Service, err)
				}
			}
			record.Status = JobRunStatusFailed
			record.FinishedAt = time.Now().UTC()
			if err := s.completeBackupRunRecord(record); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BackupScheduler) readBackupRunRecords(project, environment, service string) ([]BackupRunRecord, error) {
	path := backupRunsPath(s.dataDir, project, environment, service)
	if err := validatePathParentsNoSymlink(s.dataDir, path, "backup run path"); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup run history: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("backup run history must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read backup run history: %w", err)
	}
	var records []BackupRunRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse backup run history: %w", err)
	}
	for _, record := range records {
		if err := validateBackupRunRecord(record, project, environment, service); err != nil {
			return nil, fmt.Errorf("invalid backup run history")
		}
	}
	return records, nil
}

func validateBackupRunRecord(record BackupRunRecord, project, environment, service string) error {
	running := record.Status == JobRunStatusRunning && record.FinishedAt.IsZero()
	completed := validAlertAttemptStatus(record.Status) && !record.FinishedAt.IsZero() && !record.StartedAt.After(record.FinishedAt)
	if record.ID == "" || len(record.ID) > 128 || hasControlChars(record.ID) || record.Project != project || record.Environment != environment || record.Service != service || !isSafeProjectName(project) || !isSafeRuntimeName(environment) || !isSafeServiceName(service) || record.StartedAt.IsZero() || (!running && !completed) {
		return fmt.Errorf("invalid backup run record")
	}
	seen := make(map[string]bool, len(record.Containers))
	request := BackupScheduleRequest{Project: project, Environment: environment, Service: service}
	for index, container := range record.Containers {
		if !isSafeContainerName(container) || container != scheduledBackupContainerName(request, record.ID, index) || seen[container] {
			return fmt.Errorf("invalid backup run container")
		}
		seen[container] = true
	}
	if running && len(record.Containers) == 0 {
		return fmt.Errorf("running backup has no container identity")
	}
	return nil
}

func scheduledBackupContainerName(request BackupScheduleRequest, attemptID string, index int) string {
	return fmt.Sprintf("%s_%d", schedulerContainerName("backup", request.Project, request.Environment, request.Service, attemptID), index+1)
}

func recoverInterruptedBackupContainers(ctx context.Context, record BackupRunRecord) error {
	for _, container := range record.Containers {
		output, err := runDocker(ctx, "ps", "-a", "--filter", "name=^"+container+"$", "--format", "{{.Names}}")
		if err != nil {
			return fmt.Errorf("inspect interrupted backup container: %w, output: %s", err, output)
		}
		if strings.TrimSpace(output) != container {
			continue
		}
		if err := verifyRecoveryContainerLabels(ctx, container, map[string]string{
			"tako.project": record.Project, "tako.environment": record.Environment, "tako.service": record.Service,
			"tako.runtime": "takod", "tako.role": "scheduled-backup", "tako.attempt": record.ID,
		}); err != nil {
			return err
		}
		output, err = runDocker(ctx, "rm", "-f", container)
		if err != nil {
			return fmt.Errorf("remove interrupted backup container: %w, output: %s", err, output)
		}
	}
	return nil
}

func (s *BackupScheduler) syncAlertSubject(request BackupScheduleRequest) error {
	if s.alerts == nil {
		return nil
	}
	if err := s.alerts.RegisterSubject(request.Project, request.Environment, "backup", request.Service); err != nil {
		return err
	}
	records, err := s.readBackupRunRecords(request.Project, request.Environment, request.Service)
	if err != nil {
		return err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Status == JobRunStatusRunning {
			continue
		}
		if err := s.alerts.Observe(backupRunAlertFact(records[index])); err != nil {
			return err
		}
	}
	return nil
}

func backupRunAlertFact(record BackupRunRecord) AlertAttemptFact {
	return AlertAttemptFact{ID: record.ID, Project: record.Project, Environment: record.Environment, Kind: "backup", Service: record.Service, Status: record.Status, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt}
}

func normalizeRetentionDays(retentionDays int) int {
	if retentionDays <= 0 {
		return DefaultRetention
	}
	return retentionDays
}

func backupSchedulePath(dataDir string, request BackupScheduleRequest) string {
	return filepath.Join(dataDir, backupScheduleDirName, request.Project, request.Environment, request.Service+".json")
}

func backupRunsPath(dataDir, project, environment, service string) string {
	return filepath.Join(dataDir, backupRunsDirName, project, environment, service+".json")
}

func backupScheduleKey(request BackupScheduleRequest) string {
	return request.Project + "/" + request.Environment + "/" + request.Service
}
