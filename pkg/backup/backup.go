package backup

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	// BackupDir is the directory where backups are stored on the server
	BackupDir = "/var/lib/tako/backups"

	// DefaultRetention is the default number of days to keep backups
	DefaultRetention = 7
)

// Manager handles volume backup and restore operations
type Manager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// BackupInfo contains metadata about a backup
type BackupInfo struct {
	ID          string    `json:"id"`
	Service     string    `json:"service"`
	Volume      string    `json:"volume"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	Path        string    `json:"path"`
	Compression string    `json:"compression"`
}

// NewManager creates a new backup manager
func NewManager(client *ssh.Client, projectName, environment string, verbose bool) *Manager {
	return &Manager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// BackupVolume creates a backup of a Docker volume
func (m *Manager) BackupVolume(volumeName string) (*BackupInfo, error) {
	// Full volume name with project/env prefix
	fullVolumeName := fmt.Sprintf("%s_%s_%s", m.projectName, m.environment, volumeName)

	if m.verbose {
		fmt.Printf("  → Backing up volume: %s\n", fullVolumeName)
	}

	// Create backup directory
	backupPath := filepath.Join(BackupDir, m.projectName, m.environment)
	if _, err := m.client.Execute(fmt.Sprintf("sudo mkdir -p %s", backupPath)); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Generate backup ID and filename
	backupID := time.Now().Format("20060102-150405")
	backupFile := fmt.Sprintf("%s_%s.tar.gz", volumeName, backupID)
	backupFullPath := filepath.Join(backupPath, backupFile)

	// Check if volume exists
	checkCmd := fmt.Sprintf("docker volume inspect %s >/dev/null 2>&1 && echo 'exists'", fullVolumeName)
	output, err := m.client.Execute(checkCmd)
	if err != nil || !strings.Contains(output, "exists") {
		return nil, fmt.Errorf("volume %s does not exist", fullVolumeName)
	}

	// Create backup using a temporary container
	// This mounts the volume and creates a compressed tar archive
	backupCmd := fmt.Sprintf(`docker run --rm \
		-v %s:/source:ro \
		-v %s:/backup \
		alpine:latest \
		tar -czf /backup/%s -C /source .`,
		fullVolumeName, backupPath, backupFile)

	if m.verbose {
		fmt.Printf("  → Creating compressed backup...\n")
	}

	if _, err := m.client.Execute(backupCmd); err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	// Get backup size
	sizeCmd := fmt.Sprintf("stat -c%%s %s 2>/dev/null || echo '0'", backupFullPath)
	sizeOutput, _ := m.client.Execute(sizeCmd)
	var size int64
	fmt.Sscanf(strings.TrimSpace(sizeOutput), "%d", &size)

	if m.verbose {
		fmt.Printf("  ✓ Backup created: %s (%.2f MB)\n", backupFile, float64(size)/1024/1024)
	}

	return &BackupInfo{
		ID:          backupID,
		Service:     "", // Will be set by caller if known
		Volume:      volumeName,
		Size:        size,
		CreatedAt:   time.Now(),
		Path:        backupFullPath,
		Compression: "gzip",
	}, nil
}

// RestoreVolume restores a volume from a backup
func (m *Manager) RestoreVolume(volumeName, backupID string) error {
	fullVolumeName := fmt.Sprintf("%s_%s_%s", m.projectName, m.environment, volumeName)
	backupPath := filepath.Join(BackupDir, m.projectName, m.environment)
	backupFile := fmt.Sprintf("%s_%s.tar.gz", volumeName, backupID)
	backupFullPath := filepath.Join(backupPath, backupFile)

	if m.verbose {
		fmt.Printf("  → Restoring volume: %s from backup %s\n", fullVolumeName, backupID)
	}

	// Check if backup exists
	checkCmd := fmt.Sprintf("test -f %s && echo 'exists'", backupFullPath)
	output, err := m.client.Execute(checkCmd)
	if err != nil || !strings.Contains(output, "exists") {
		return fmt.Errorf("backup not found: %s", backupFullPath)
	}

	// Check if volume exists, create if not
	createCmd := fmt.Sprintf("docker volume inspect %s >/dev/null 2>&1 || docker volume create %s",
		fullVolumeName, fullVolumeName)
	if _, err := m.client.Execute(createCmd); err != nil {
		return fmt.Errorf("failed to ensure volume exists: %w", err)
	}

	// Restore from backup using a temporary container
	restoreCmd := fmt.Sprintf(`docker run --rm \
		-v %s:/target \
		-v %s:/backup:ro \
		alpine:latest \
		sh -c "rm -rf /target/* /target/..?* /target/.[!.]* 2>/dev/null; tar -xzf /backup/%s -C /target"`,
		fullVolumeName, backupPath, backupFile)

	if m.verbose {
		fmt.Printf("  → Extracting backup...\n")
	}

	if _, err := m.client.Execute(restoreCmd); err != nil {
		return fmt.Errorf("failed to restore backup: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Volume restored successfully\n")
	}

	return nil
}

// ListBackups lists all backups for a volume or all volumes
func (m *Manager) ListBackups(volumeName string) ([]BackupInfo, error) {
	backupPath := filepath.Join(BackupDir, m.projectName, m.environment)

	pattern := "*.tar.gz"
	if volumeName != "" {
		pattern = fmt.Sprintf("%s_*.tar.gz", volumeName)
	}

	// List backup files
	listCmd := fmt.Sprintf("ls -la %s/%s 2>/dev/null | tail -n +2", backupPath, pattern)
	output, err := m.client.Execute(listCmd)
	if err != nil {
		return []BackupInfo{}, nil // No backups found
	}

	var backups []BackupInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse ls output: -rw-r--r-- 1 root root 12345 Jan 1 12:00 volume_20240101-120000.tar.gz
		parts := strings.Fields(line)
		if len(parts) < 9 {
			continue
		}

		filename := parts[len(parts)-1]
		if !strings.HasSuffix(filename, ".tar.gz") {
			continue
		}

		// Parse filename: volume_20240101-120000.tar.gz
		baseName := strings.TrimSuffix(filename, ".tar.gz")
		nameParts := strings.Split(baseName, "_")
		if len(nameParts) < 2 {
			continue
		}

		volName := strings.Join(nameParts[:len(nameParts)-1], "_")
		backupID := nameParts[len(nameParts)-1]

		var size int64
		fmt.Sscanf(parts[4], "%d", &size)

		// Parse timestamp from backup ID
		createdAt, _ := time.Parse("20060102-150405", backupID)

		backups = append(backups, BackupInfo{
			ID:          backupID,
			Volume:      volName,
			Size:        size,
			CreatedAt:   createdAt,
			Path:        filepath.Join(backupPath, filename),
			Compression: "gzip",
		})
	}

	return backups, nil
}

// DeleteBackup deletes a specific backup
func (m *Manager) DeleteBackup(volumeName, backupID string) error {
	backupPath := filepath.Join(BackupDir, m.projectName, m.environment)
	backupFile := fmt.Sprintf("%s_%s.tar.gz", volumeName, backupID)
	backupFullPath := filepath.Join(backupPath, backupFile)

	if m.verbose {
		fmt.Printf("  → Deleting backup: %s\n", backupFile)
	}

	if _, err := m.client.Execute(fmt.Sprintf("sudo rm -f %s", backupFullPath)); err != nil {
		return fmt.Errorf("failed to delete backup: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Backup deleted\n")
	}

	return nil
}

// CleanupOldBackups removes backups older than retention days
func (m *Manager) CleanupOldBackups(retentionDays int) (int, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetention
	}

	backupPath := filepath.Join(BackupDir, m.projectName, m.environment)

	if m.verbose {
		fmt.Printf("  → Cleaning up backups older than %d days...\n", retentionDays)
	}

	// Find and delete old backups
	cleanupCmd := fmt.Sprintf("find %s -name '*.tar.gz' -mtime +%d -delete -print 2>/dev/null | wc -l",
		backupPath, retentionDays)

	output, err := m.client.Execute(cleanupCmd)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old backups: %w", err)
	}

	var count int
	fmt.Sscanf(strings.TrimSpace(output), "%d", &count)

	if m.verbose {
		fmt.Printf("  ✓ Cleaned up %d old backups\n", count)
	}

	return count, nil
}

// BackupAllVolumes backs up all volumes for the project
func (m *Manager) BackupAllVolumes(cfg *config.Config) ([]BackupInfo, error) {
	env, err := cfg.GetEnvironment(m.environment)
	if err != nil {
		return nil, err
	}

	var backups []BackupInfo

	for serviceName, service := range env.Services {
		for _, volume := range service.Volumes {
			// Parse volume spec to get volume name
			parts := strings.SplitN(volume, ":", 2)
			volName := parts[0]

			// Skip bind mounts (absolute paths)
			if strings.HasPrefix(volName, "/") {
				continue
			}

			info, err := m.BackupVolume(volName)
			if err != nil {
				fmt.Printf("  ⚠ Failed to backup %s: %v\n", volName, err)
				continue
			}

			info.Service = serviceName
			backups = append(backups, *info)
		}
	}

	return backups, nil
}

// ScheduleBackup creates a cron job for scheduled backups
func (m *Manager) ScheduleBackup(schedule string, volumeName string) error {
	// Build the backup command
	takoCmd := fmt.Sprintf("tako backup --volume %s --env %s", volumeName, m.environment)

	// Create cron entry
	cronEntry := fmt.Sprintf("%s %s >> /var/log/tako/backup.log 2>&1", schedule, takoCmd)

	// Add to crontab
	addCronCmd := fmt.Sprintf(`(crontab -l 2>/dev/null | grep -v "tako backup.*%s"; echo "%s") | crontab -`,
		volumeName, cronEntry)

	if _, err := m.client.Execute(addCronCmd); err != nil {
		return fmt.Errorf("failed to schedule backup: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Scheduled backup: %s\n", schedule)
	}

	return nil
}
