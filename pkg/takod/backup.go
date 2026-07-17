package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const (
	BackupDir        = "/var/lib/tako/backups"
	DefaultRetention = 7
	backupImage      = "alpine:3.20"
)

var backupRootDir = BackupDir

type BackupRequest struct {
	Project        string               `json:"project"`
	Environment    string               `json:"environment"`
	Volume         string               `json:"volume,omitempty"`
	DockerVolume   string               `json:"dockerVolume,omitempty"`
	ExternalVolume bool                 `json:"externalVolume,omitempty"`
	BackupID       string               `json:"backupId,omitempty"`
	RetentionDays  int                  `json:"retentionDays,omitempty"`
	Storage        *BackupStorageConfig `json:"storage,omitempty"`
}

type BackupInfo struct {
	ID          string            `json:"id"`
	Service     string            `json:"service,omitempty"`
	Volume      string            `json:"volume"`
	Size        int64             `json:"size"`
	CreatedAt   time.Time         `json:"createdAt"`
	Path        string            `json:"path"`
	Compression string            `json:"compression"`
	Remote      *BackupRemoteInfo `json:"remote,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

type BackupListResponse struct {
	Backups []BackupInfo `json:"backups"`
}

type BackupCleanupResponse struct {
	Deleted int `json:"deleted"`
}

type BackupRemoteInfo struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	Endpoint string `json:"endpoint,omitempty"`
}

type BackupStorageConfig struct {
	Provider        string `json:"provider,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Region          string `json:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
	ForcePathStyle  bool   `json:"forcePathStyle,omitempty"`
}

func CreateVolumeBackup(ctx context.Context, req BackupRequest) (*BackupInfo, error) {
	if err := validateBackupRequest(req, true, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	if err := os.MkdirAll(backupPath, 0750); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	fullVolumeName := fullBackupVolumeName(req)
	if _, err := runDocker(ctx, "volume", "inspect", fullVolumeName); err != nil {
		return nil, fmt.Errorf("volume %s does not exist", fullVolumeName)
	}

	backupID := backupIDForRequest(req, time.Now())
	backupFile := backupFileName(req.Volume, backupID)
	if _, err := runDocker(
		ctx,
		"run", "--rm",
		"-v", fullVolumeName+":/source:ro",
		"-v", backupPath+":/backup",
		backupImage,
		"tar", "-czf", "/backup/"+backupFile, "-C", "/source", ".",
	); err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	path := filepath.Join(backupPath, backupFile)
	info, err := backupInfoFromPath(backupPath, path)
	if err != nil {
		return nil, err
	}
	if req.Storage != nil {
		remote, err := UploadBackupObject(ctx, *req.Storage, BackupObject{
			Project:     req.Project,
			Environment: req.Environment,
			Volume:      req.Volume,
			BackupID:    backupID,
			Path:        path,
			CreatedAt:   info.CreatedAt,
		})
		if err != nil {
			info.Warnings = append(info.Warnings, fmt.Sprintf("local backup created but upload failed: %v", err))
			return &info, nil
		}
		info.Remote = remote
		if req.RetentionDays > 0 {
			if err := CleanupBackupObjects(ctx, *req.Storage, BackupObjectRetention{
				Project:       req.Project,
				Environment:   req.Environment,
				Volume:        req.Volume,
				RetentionDays: req.RetentionDays,
			}); err != nil {
				info.Warnings = append(info.Warnings, fmt.Sprintf("remote backup retention cleanup failed: %v", err))
			}
		}
	}
	return &info, nil
}

func RestoreVolumeBackup(ctx context.Context, req BackupRequest) error {
	if err := validateBackupRequest(req, true, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backupPath := backupDirectory(req)
	backupFile := backupFileName(req.Volume, req.BackupID)
	backupFullPath := filepath.Join(backupPath, backupFile)
	if _, err := os.Stat(backupFullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("backup not found: %s", backupFullPath)
		}
		return fmt.Errorf("failed to inspect backup: %w", err)
	}

	fullVolumeName := fullBackupVolumeName(req)
	if _, err := runDocker(ctx, "volume", "inspect", fullVolumeName); err != nil {
		if req.ExternalVolume {
			return fmt.Errorf("external volume %s does not exist", fullVolumeName)
		}
		if createErr := ensureDockerVolume(ctx, req.Project, req.Environment, "", fullVolumeName); createErr != nil {
			return fmt.Errorf("failed to ensure volume exists: %w", createErr)
		}
	}

	if _, err := runDocker(
		ctx,
		"run", "--rm",
		"-v", fullVolumeName+":/target",
		"-v", backupPath+":/backup:ro",
		backupImage,
		"sh", "-c", restoreVolumeScript(backupFile),
	); err != nil {
		return fmt.Errorf("failed to restore backup: %w", err)
	}
	return nil
}

func ListVolumeBackups(ctx context.Context, req BackupRequest) (*BackupListResponse, error) {
	if err := validateBackupRequest(req, false, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	entries, err := os.ReadDir(backupPath)
	if os.IsNotExist(err) {
		return &BackupListResponse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	var backups []BackupInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(backupPath, entry.Name())
		info, err := backupInfoFromPath(backupPath, path)
		if err != nil {
			continue
		}
		if req.Volume != "" && info.Volume != req.Volume {
			continue
		}
		backups = append(backups, info)
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].Volume == backups[j].Volume {
			return backups[i].CreatedAt.After(backups[j].CreatedAt)
		}
		return backups[i].Volume < backups[j].Volume
	})
	return &BackupListResponse{Backups: backups}, nil
}

func DeleteVolumeBackup(ctx context.Context, req BackupRequest) error {
	if err := validateBackupRequest(req, true, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(backupDirectory(req), backupFileName(req.Volume, req.BackupID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete backup: %w", err)
	}
	return nil
}

func CleanupOldBackups(ctx context.Context, req BackupRequest) (*BackupCleanupResponse, error) {
	if err := validateBackupRequest(req, false, false); err != nil {
		return nil, err
	}
	if req.RetentionDays <= 0 {
		req.RetentionDays = DefaultRetention
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	entries, err := os.ReadDir(backupPath)
	if os.IsNotExist(err) {
		return &BackupCleanupResponse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -req.RetentionDays)
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := backupInfoFromPath(backupPath, filepath.Join(backupPath, entry.Name()))
		if err != nil {
			continue
		}
		if info.CreatedAt.IsZero() || info.CreatedAt.After(cutoff) {
			continue
		}
		if req.Volume != "" && info.Volume != req.Volume {
			continue
		}
		if err := os.Remove(info.Path); err != nil {
			return nil, fmt.Errorf("failed to delete old backup %s: %w", info.Path, err)
		}
		deleted++
	}
	return &BackupCleanupResponse{Deleted: deleted}, nil
}

func validateBackupRequest(req BackupRequest, requireVolume bool, requireBackupID bool) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if requireVolume || req.Volume != "" {
		if !isSafeBackupVolume(req.Volume) {
			return fmt.Errorf("invalid volume name")
		}
	}
	if req.DockerVolume != "" && !isSafeDockerVolumeName(req.DockerVolume) {
		return fmt.Errorf("invalid docker volume name")
	}
	if requireBackupID || req.BackupID != "" {
		if !isSafeBackupID(req.BackupID) {
			return fmt.Errorf("invalid backup ID")
		}
	}
	if req.RetentionDays < 0 {
		return fmt.Errorf("retentionDays cannot be negative")
	}
	if req.Storage != nil {
		if err := ValidateBackupStorage(*req.Storage); err != nil {
			return err
		}
	}
	return nil
}

func backupIDForRequest(req BackupRequest, now time.Time) string {
	if req.BackupID != "" {
		return req.BackupID
	}
	return now.UTC().Format("20060102-150405")
}

func backupDirectory(req BackupRequest) string {
	return filepath.Join(backupRootDir, req.Project, req.Environment)
}

func fullBackupVolumeName(req BackupRequest) string {
	if req.DockerVolume != "" {
		return req.DockerVolume
	}
	return runtimeid.VolumeName(req.Project, req.Environment, req.Volume)
}

func backupFileName(volume string, backupID string) string {
	return volume + "_" + backupID + ".tar.gz"
}

func restoreVolumeScript(backupFile string) string {
	quotedBackup := shellQuote(backupFile)
	return fmt.Sprintf(`set -eu
backupPath=/backup/%s
if [ ! -d /target ] || [ -L /target ]; then
  echo "invalid restore target" >&2
  exit 1
fi
if [ ! -f "$backupPath" ]; then
  echo "backup file not found" >&2
  exit 1
fi
tar -tzf "$backupPath" | awk '
BEGIN { bad = 0 }
/^\/|(^|\/)\.\.(\/|$)/ {
  print "unsafe backup entry: " $0 > "/dev/stderr"
  bad = 1
}
END { exit bad }
'
find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
tar -xzf "$backupPath" -C /target
`, quotedBackup)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func backupInfoFromPath(root string, path string) (BackupInfo, error) {
	filename := filepath.Base(path)
	if !strings.HasSuffix(filename, ".tar.gz") {
		return BackupInfo{}, fmt.Errorf("not a backup file")
	}
	base := strings.TrimSuffix(filename, ".tar.gz")
	separator := strings.LastIndex(base, "_")
	if separator <= 0 || separator == len(base)-1 {
		return BackupInfo{}, fmt.Errorf("invalid backup filename")
	}
	volume := base[:separator]
	backupID := base[separator+1:]
	if !isSafeBackupVolume(volume) || !isSafeBackupID(backupID) {
		return BackupInfo{}, fmt.Errorf("invalid backup filename")
	}
	info, err := os.Stat(path)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("failed to stat backup: %w", err)
	}
	createdAt, _ := backupIDTimestamp(backupID)
	return BackupInfo{
		ID:          backupID,
		Volume:      volume,
		Size:        info.Size(),
		CreatedAt:   createdAt.UTC(),
		Path:        filepath.Join(root, filename),
		Compression: "gzip",
	}, nil
}

func isSafeBackupVolume(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '-' || r == '_' || r == '.') {
			continue
		}
		return false
	}
	return true
}

func isSafeBackupID(value string) bool {
	_, err := backupIDTimestamp(value)
	return err == nil
}

func backupIDTimestamp(value string) (time.Time, error) {
	const layout = "20060102-150405"
	if len(value) != len(layout) {
		const randomSuffixLength = 32
		if len(value) != len(layout)+1+randomSuffixLength || value[len(layout)] != '-' {
			return time.Time{}, fmt.Errorf("invalid backup ID")
		}
		for _, char := range value[len(layout)+1:] {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return time.Time{}, fmt.Errorf("invalid backup ID")
			}
		}
	}
	return time.Parse(layout, value[:len(layout)])
}
