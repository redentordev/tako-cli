package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	backupVolume  string
	backupAll     bool
	backupList    bool
	backupRestore string
	backupDelete  string
	backupCleanup int
	backupServer  string
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup and restore service volumes",
	Long: `Backup and restore service volumes.

Examples:
  # Backup a specific volume
  tako backup --volume data

  # Backup a specific node's volume
  tako backup --server node-a --volume data

  # Backup all volumes
  tako backup --all

  # List all backups
  tako backup --list

  # Restore a volume from a backup
  tako backup --volume data --restore 20240101-120000

  # Delete old backups
  tako backup --cleanup 7  # Delete backups older than 7 days
`,
	RunE: runBackup,
}

func init() {
	rootCmd.AddCommand(backupCmd)
	backupCmd.Flags().StringVar(&backupVolume, "volume", "", "Volume name to backup/restore")
	backupCmd.Flags().BoolVar(&backupAll, "all", false, "Backup all volumes")
	backupCmd.Flags().BoolVar(&backupList, "list", false, "List available backups")
	backupCmd.Flags().StringVar(&backupRestore, "restore", "", "Backup ID to restore from")
	backupCmd.Flags().StringVar(&backupDelete, "delete", "", "Backup ID to delete")
	backupCmd.Flags().IntVar(&backupCleanup, "cleanup", 0, "Delete backups older than N days")
	backupCmd.Flags().StringVarP(&backupServer, "server", "s", "", "Node to run the backup operation on")
}

func runBackup(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	serverName, serverCfg, err := resolveServer(cfg, envName, backupServer)
	if err != nil {
		return err
	}

	// Connect to server
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverCfg.Host,
		Port:     serverCfg.Port,
		User:     serverCfg.User,
		SSHKey:   serverCfg.SSHKey,
		Password: serverCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	if verbose {
		fmt.Printf("Using node: %s (%s)\n", serverName, serverCfg.Host)
	}

	// Handle different operations
	switch {
	case backupList:
		return listBackups(client, cfg, envName, backupVolume)

	case backupRestore != "":
		if backupVolume == "" {
			return fmt.Errorf("--volume is required for restore")
		}
		return restoreBackup(client, cfg, envName, backupVolume, backupRestore)

	case backupDelete != "":
		if backupVolume == "" {
			return fmt.Errorf("--volume is required for delete")
		}
		return deleteBackup(client, cfg, envName, backupVolume, backupDelete)

	case backupCleanup > 0:
		return cleanupBackups(client, cfg, envName, backupCleanup)

	case backupAll:
		return backupAllVolumes(client, cfg, envName)

	case backupVolume != "":
		return createBackup(client, cfg, envName, backupVolume)

	default:
		return fmt.Errorf("specify --volume, --all, --list, --restore, --delete, or --cleanup")
	}
}

func listBackups(client *ssh.Client, cfg *config.Config, envName string, volumeName string) error {
	fmt.Printf("=== Volume Backups ===\n\n")

	var response takod.BackupListResponse
	err := takodBackupRequestJSON(
		client,
		cfg,
		"GET",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, volumeName, ""),
		nil,
		&response,
	)
	if err != nil {
		return err
	}

	if len(response.Backups) == 0 {
		fmt.Println("No backups found")
		return nil
	}

	// Group by volume
	byVolume := make(map[string][]takod.BackupInfo)
	for _, b := range response.Backups {
		byVolume[b.Volume] = append(byVolume[b.Volume], b)
	}

	volumes := make([]string, 0, len(byVolume))
	for vol := range byVolume {
		volumes = append(volumes, vol)
	}
	sort.Strings(volumes)

	for _, vol := range volumes {
		fmt.Printf("Volume: %s\n", vol)
		for _, b := range byVolume[vol] {
			sizeStr := formatSize(b.Size)
			fmt.Printf("  - %s  %s  %s\n", b.ID, b.CreatedAt.Format("2006-01-02 15:04"), sizeStr)
		}
		fmt.Println()
	}

	return nil
}

func createBackup(client *ssh.Client, cfg *config.Config, envName string, volumeName string) error {
	fmt.Printf("=== Backing up volume: %s ===\n\n", volumeName)

	var info takod.BackupInfo
	err := takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups",
		backupRequest(cfg, envName, volumeName, "", 0),
		&info,
	)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	fmt.Printf("\n✓ Backup created successfully\n")
	fmt.Printf("  ID:   %s\n", info.ID)
	fmt.Printf("  Size: %s\n", formatSize(info.Size))
	fmt.Printf("  Path: %s\n", info.Path)

	return nil
}

func restoreBackup(client *ssh.Client, cfg *config.Config, envName string, volumeName string, backupID string) error {
	fmt.Printf("=== Restoring volume: %s from backup %s ===\n\n", volumeName, backupID)
	fmt.Printf("⚠️  WARNING: This will overwrite all data in the volume!\n\n")

	var response map[string]bool
	err := takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups/restore",
		backupRequest(cfg, envName, volumeName, backupID, 0),
		&response,
	)
	if err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}

	fmt.Printf("\n✓ Volume restored successfully\n")
	return nil
}

func deleteBackup(client *ssh.Client, cfg *config.Config, envName string, volumeName string, backupID string) error {
	fmt.Printf("=== Deleting backup: %s_%s ===\n\n", volumeName, backupID)

	var response map[string]bool
	err := takodBackupRequestJSON(
		client,
		cfg,
		"DELETE",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, volumeName, backupID),
		nil,
		&response,
	)
	if err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}

	fmt.Printf("✓ Backup deleted\n")
	return nil
}

func cleanupBackups(client *ssh.Client, cfg *config.Config, envName string, days int) error {
	fmt.Printf("=== Cleaning up backups older than %d days ===\n\n", days)

	var response takod.BackupCleanupResponse
	err := takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups/cleanup",
		backupRequest(cfg, envName, "", "", days),
		&response,
	)
	if err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}

	fmt.Printf("✓ Cleaned up %d old backups\n", response.Deleted)
	return nil
}

func backupAllVolumes(client *ssh.Client, cfg *config.Config, envName string) error {
	fmt.Printf("=== Backing up all volumes ===\n\n")

	volumes, err := backupVolumesFromConfig(cfg, envName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	var backups []takod.BackupInfo
	for _, volume := range volumes {
		var info takod.BackupInfo
		err := takodBackupRequestJSON(
			client,
			cfg,
			"POST",
			"/v1/backups",
			backupRequest(cfg, envName, volume.name, "", 0),
			&info,
		)
		if err != nil {
			fmt.Printf("  ⚠ Failed to backup %s: %v\n", volume.name, err)
			continue
		}
		info.Service = volume.service
		backups = append(backups, info)
	}

	fmt.Printf("\n✓ Backed up %d volumes\n", len(backups))
	for _, b := range backups {
		fmt.Printf("  - %s: %s (%s)\n", b.Volume, b.ID, formatSize(b.Size))
	}

	return nil
}

type backupVolumeSpec struct {
	name    string
	service string
}

func backupVolumesFromConfig(cfg *config.Config, envName string) ([]backupVolumeSpec, error) {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]backupVolumeSpec)
	for serviceName, service := range services {
		for _, volume := range service.Volumes {
			source, _, _ := strings.Cut(volume, ":")
			source = strings.TrimSpace(source)
			if source == "" || strings.HasPrefix(source, "/") {
				continue
			}
			if _, ok := seen[source]; !ok {
				seen[source] = backupVolumeSpec{name: source, service: serviceName}
			}
		}
	}

	volumes := make([]backupVolumeSpec, 0, len(seen))
	for _, volume := range seen {
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].name < volumes[j].name
	})
	return volumes, nil
}

func backupRequest(cfg *config.Config, envName string, volumeName string, backupID string, retentionDays int) takod.BackupRequest {
	return takod.BackupRequest{
		Project:       cfg.Project.Name,
		Environment:   envName,
		Volume:        volumeName,
		BackupID:      backupID,
		RetentionDays: retentionDays,
	}
}

func takodBackupRequestJSON(client *ssh.Client, cfg *config.Config, method string, endpoint string, request any, response any) error {
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), method, endpoint, request)
	if err != nil {
		return err
	}
	if response == nil {
		return nil
	}
	return decodeTakodJSON(output, response)
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
