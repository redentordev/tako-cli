package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/recovery"
	"github.com/robfig/cron/v3"
)

const backupScheduleDirName = "backup-schedules"

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

type BackupScheduler struct {
	dataDir string
	parser  cron.Parser
	cron    *cron.Cron
	admit   func(...string) error

	mu             sync.Mutex
	entries        map[string]cron.EntryID
	generations    map[string]uint64
	nextGeneration uint64
	running        map[string]bool
	draining       map[string]bool
	recoverable    map[string]bool
	mutating       map[string]int
	runningChanged chan struct{}
}

func NewBackupScheduler(dataDir string) *BackupScheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &BackupScheduler{
		dataDir:        dataDir,
		parser:         parser,
		cron:           cron.New(cron.WithParser(parser), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		entries:        map[string]cron.EntryID{},
		generations:    map[string]uint64{},
		running:        map[string]bool{},
		draining:       map[string]bool{},
		recoverable:    map[string]bool{},
		mutating:       map[string]int{},
		runningChanged: make(chan struct{}),
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
	if err := s.loadSchedules(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "takod backup scheduler failed to load schedules: %v\n", err)
	}
	s.cron.Start()
	<-ctx.Done()
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(30 * time.Second):
	}
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
	if entryID, ok := s.entries[backupScheduleKey(request)]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, backupScheduleKey(request))
	}
	delete(s.generations, backupScheduleKey(request))
	s.mu.Unlock()
	if err := os.Remove(backupSchedulePath(s.dataDir, request)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove backup schedule: %w", err)
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
	s.mu.Lock()
	for key, entryID := range s.entries {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		s.cron.Remove(entryID)
		delete(s.entries, key)
		delete(s.generations, key)
		removed++
	}
	s.mu.Unlock()

	path := filepath.Join(s.dataDir, backupScheduleDirName, project)
	if environment != "" {
		path = filepath.Join(path, environment)
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return removed, fmt.Errorf("failed to remove backup schedules: %w", err)
	}
	return removed, nil
}

func (s *BackupScheduler) loadSchedules(ctx context.Context) error {
	root := filepath.Join(s.dataDir, backupScheduleDirName)
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
		var request BackupScheduleRequest
		if err := json.Unmarshal(data, &request); err != nil {
			return fmt.Errorf("failed to parse backup schedule %s: %w", path, err)
		}
		request = normalizeBackupScheduleRequest(request)
		if err := validateBackupScheduleRequest(request); err != nil {
			return fmt.Errorf("invalid backup schedule %s: %w", path, err)
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
		s.mu.Lock()
		err = s.scheduleLocked(request)
		s.mu.Unlock()
		endMutation()
		return err
	})
}

func (s *BackupScheduler) persistSchedule(request BackupScheduleRequest) error {
	path := backupSchedulePath(s.dataDir, request)
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
	return nil
}

func (s *BackupScheduler) runScheduledBackup(request BackupScheduleRequest, expectedGeneration ...uint64) {
	key := backupScheduleKey(request)
	unlock, err := recovery.AcquireMutationLock(s.dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "takod backup schedule snapshot lock failed for %s: %v\n", key, err)
		return
	}
	defer unlock()
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
	if s.running[key] {
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "takod backup schedule skipped overlapping run: %s\n", key)
		return
	}
	s.running[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, key)
		s.notifyRunningChangedLocked()
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	backupID := backupIDForRequest(BackupRequest{}, time.Now())
	for _, volume := range request.Volumes {
		if s.admit != nil {
			if err := s.admit(backupRootDir); err != nil {
				fmt.Fprintf(os.Stderr, "takod backup schedule denied by resource admission for %s volume %s: %v\n", key, volume.Volume, err)
				continue
			}
		}
		info, err := CreateVolumeBackup(ctx, BackupRequest{
			Project:        request.Project,
			Environment:    request.Environment,
			Volume:         volume.Volume,
			DockerVolume:   volume.DockerVolume,
			ExternalVolume: volume.ExternalVolume,
			BackupID:       backupID,
			RetentionDays:  request.RetentionDays,
			Storage:        request.Storage,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduled backup failed for %s/%s/%s volume %s: %v\n", request.Project, request.Environment, request.Service, volume.Volume, err)
		}
		if info != nil {
			for _, warning := range info.Warnings {
				fmt.Fprintf(os.Stderr, "takod scheduled backup warning for %s/%s/%s volume %s: %s\n", request.Project, request.Environment, request.Service, volume.Volume, warning)
			}
		}
		if request.RetentionDays <= 0 {
			continue
		}
		if _, err := CleanupOldBackups(ctx, BackupRequest{
			Project:       request.Project,
			Environment:   request.Environment,
			Volume:        volume.Volume,
			RetentionDays: request.RetentionDays,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduled backup cleanup failed for %s/%s/%s volume %s: %v\n", request.Project, request.Environment, request.Service, volume.Volume, err)
		}
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
	if len(request.Volumes) == 0 {
		return fmt.Errorf("at least one backup volume is required")
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

func normalizeRetentionDays(retentionDays int) int {
	if retentionDays <= 0 {
		return DefaultRetention
	}
	return retentionDays
}

func backupSchedulePath(dataDir string, request BackupScheduleRequest) string {
	return filepath.Join(dataDir, backupScheduleDirName, request.Project, request.Environment, request.Service+".json")
}

func backupScheduleKey(request BackupScheduleRequest) string {
	return request.Project + "/" + request.Environment + "/" + request.Service
}
