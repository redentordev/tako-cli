package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/backup"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	backupVolume  string
	backupAll     bool
	backupList    bool
	backupRestore string
	backupDelete  string
	backupCleanup int
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup and restore Docker volumes",
	Long: `Backup and restore Docker volumes for your services.

Examples:
  # Backup a specific volume
  tako backup --volume data

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
}

func runBackup(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Get manager server
	managerName, err := cfg.GetManagerServer(envName)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	managerCfg := cfg.Servers[managerName]

	// Connect to server
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     managerCfg.Host,
		Port:     managerCfg.Port,
		User:     managerCfg.User,
		SSHKey:   managerCfg.SSHKey,
		Password: managerCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer client.Close()

	mgr := backup.NewManager(client, cfg.Project.Name, envName, verbose)

	// Handle different operations
	switch {
	case backupList:
		return listBackups(mgr, backupVolume)

	case backupRestore != "":
		if backupVolume == "" {
			return fmt.Errorf("--volume is required for restore")
		}
		return restoreBackup(mgr, backupVolume, backupRestore)

	case backupDelete != "":
		if backupVolume == "" {
			return fmt.Errorf("--volume is required for delete")
		}
		return deleteBackup(mgr, backupVolume, backupDelete)

	case backupCleanup > 0:
		return cleanupBackups(mgr, backupCleanup)

	case backupAll:
		return backupAllVolumes(mgr, cfg)

	case backupVolume != "":
		return createBackup(mgr, backupVolume)

	default:
		return fmt.Errorf("specify --volume, --all, --list, --restore, --delete, or --cleanup")
	}
}

func listBackups(mgr *backup.Manager, volumeName string) error {
	fmt.Printf("=== Volume Backups ===\n\n")

	backups, err := mgr.ListBackups(volumeName)
	if err != nil {
		return err
	}

	if len(backups) == 0 {
		fmt.Println("No backups found")
		return nil
	}

	// Group by volume
	byVolume := make(map[string][]backup.BackupInfo)
	for _, b := range backups {
		byVolume[b.Volume] = append(byVolume[b.Volume], b)
	}

	for vol, vBackups := range byVolume {
		fmt.Printf("Volume: %s\n", vol)
		for _, b := range vBackups {
			sizeStr := formatSize(b.Size)
			fmt.Printf("  - %s  %s  %s\n", b.ID, b.CreatedAt.Format("2006-01-02 15:04"), sizeStr)
		}
		fmt.Println()
	}

	return nil
}

func createBackup(mgr *backup.Manager, volumeName string) error {
	fmt.Printf("=== Backing up volume: %s ===\n\n", volumeName)

	info, err := mgr.BackupVolume(volumeName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	fmt.Printf("\n✓ Backup created successfully\n")
	fmt.Printf("  ID:   %s\n", info.ID)
	fmt.Printf("  Size: %s\n", formatSize(info.Size))
	fmt.Printf("  Path: %s\n", info.Path)

	return nil
}

func restoreBackup(mgr *backup.Manager, volumeName, backupID string) error {
	fmt.Printf("=== Restoring volume: %s from backup %s ===\n\n", volumeName, backupID)
	fmt.Printf("⚠️  WARNING: This will overwrite all data in the volume!\n\n")

	if err := mgr.RestoreVolume(volumeName, backupID); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}

	fmt.Printf("\n✓ Volume restored successfully\n")
	return nil
}

func deleteBackup(mgr *backup.Manager, volumeName, backupID string) error {
	fmt.Printf("=== Deleting backup: %s_%s ===\n\n", volumeName, backupID)

	if err := mgr.DeleteBackup(volumeName, backupID); err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}

	fmt.Printf("✓ Backup deleted\n")
	return nil
}

func cleanupBackups(mgr *backup.Manager, days int) error {
	fmt.Printf("=== Cleaning up backups older than %d days ===\n\n", days)

	count, err := mgr.CleanupOldBackups(days)
	if err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}

	fmt.Printf("✓ Cleaned up %d old backups\n", count)
	return nil
}

func backupAllVolumes(mgr *backup.Manager, cfg *config.Config) error {
	fmt.Printf("=== Backing up all volumes ===\n\n")

	backups, err := mgr.BackupAllVolumes(cfg)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	fmt.Printf("\n✓ Backed up %d volumes\n", len(backups))
	for _, b := range backups {
		fmt.Printf("  - %s: %s (%s)\n", b.Volume, b.ID, formatSize(b.Size))
	}

	return nil
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

// Helper to check if a string contains substring (case insensitive)
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
