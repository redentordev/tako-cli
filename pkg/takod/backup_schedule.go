package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

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

	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]bool
}

func NewBackupScheduler(dataDir string) *BackupScheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &BackupScheduler{
		dataDir: dataDir,
		parser:  parser,
		cron:    cron.New(cron.WithParser(parser), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		entries: map[string]cron.EntryID{},
		running: map[string]bool{},
	}
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
	s.mu.Lock()
	if entryID, ok := s.entries[backupScheduleKey(request)]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, backupScheduleKey(request))
	}
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
		s.mu.Lock()
		err = s.scheduleLocked(request)
		s.mu.Unlock()
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
	entryID, err := s.cron.AddFunc(request.Schedule, func() {
		s.runScheduledBackup(request)
	})
	if err != nil {
		return fmt.Errorf("failed to schedule backup: %w", err)
	}
	s.entries[key] = entryID
	return nil
}

func (s *BackupScheduler) runScheduledBackup(request BackupScheduleRequest) {
	key := backupScheduleKey(request)
	s.mu.Lock()
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
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	backupID := backupIDForRequest(BackupRequest{}, time.Now())
	for _, volume := range request.Volumes {
		_, err := CreateVolumeBackup(ctx, BackupRequest{
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
