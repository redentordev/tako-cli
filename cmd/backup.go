package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
  # Backup a specific volume across the environment mesh
  tako backup --volume data

  # Backup a specific node's volume
  tako backup --server node-a --volume data

  # Backup all configured volumes across the environment mesh
  tako backup --all

  # List backups on every reachable environment node
  tako backup --list

  # Restore a node-local volume from a backup
  tako backup --server node-a --volume data --restore 20240101-120000

  # Delete old backups across the environment mesh
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
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)

	servers, err := resolveEnvironmentServerSet(cfg, envName, backupServer)
	if err != nil {
		return err
	}
	targetServerNames := sortedBackupServerNames(servers)
	if len(targetServerNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	if backupList {
		return listBackupsAcrossNodes(cfg, envName, servers, backupVolume)
	}

	if backupRestore != "" {
		if backupVolume == "" {
			return fmt.Errorf("--volume is required for restore")
		}
		if err := ensureSingleBackupRestoreTarget(targetServerNames, backupServer); err != nil {
			return err
		}
	}
	if backupDelete != "" && backupVolume == "" {
		return fmt.Errorf("--volume is required for delete")
	}
	if backupRestore == "" && backupDelete == "" && backupCleanup <= 0 && !backupAll && backupVolume == "" {
		return fmt.Errorf("specify --volume, --all, --list, --restore, --delete, or --cleanup")
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServerNames, "backup")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote backup leases: %s\n", leaseSet.Summary())
	}

	switch {
	case backupRestore != "":
		serverName := targetServerNames[0]
		return restoreBackupOnNode(cfg, envName, serverName, servers[serverName], backupVolume, backupRestore)

	case backupDelete != "":
		return deleteBackupAcrossNodes(cfg, envName, servers, backupVolume, backupDelete)

	case backupCleanup > 0:
		return cleanupBackupsAcrossNodes(cfg, envName, servers, backupCleanup)

	case backupAll:
		return backupAllVolumesAcrossNodes(cfg, envName, servers, newBackupID())

	case backupVolume != "":
		return createBackupAcrossNodes(cfg, envName, servers, backupVolume, newBackupID())

	default:
		return fmt.Errorf("specify --volume, --all, --list, --restore, --delete, or --cleanup")
	}
}

func ensureSingleBackupRestoreTarget(targetServerNames []string, requestedServer string) error {
	if len(targetServerNames) == 0 {
		return fmt.Errorf("no backup restore target selected")
	}
	if requestedServer == "" && len(targetServerNames) > 1 {
		return fmt.Errorf("restore is node-local in a multi-node environment; pass --server <node> to choose the node to restore")
	}
	return nil
}

func listBackupsAcrossNodes(cfg *config.Config, envName string, servers map[string]config.ServerConfig, volumeName string) error {
	fmt.Printf("=== Volume Backups ===\n\n")

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		backups, err := readBackupsFromNode(cfg, serverCfg, envName, volumeName)
		return backupNodeActionResult{backups: backups}, err
	})

	totalBackups := 0
	totalErrors := 0
	for _, result := range results {
		fmt.Printf("Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			totalErrors++
			fmt.Printf("  Failed: %v\n\n", result.err)
			continue
		}
		totalBackups += len(result.backups)
		if len(result.backups) == 0 {
			fmt.Println("  No backups found")
			fmt.Println()
			continue
		}
		printBackupGroups(result.backups)
	}

	if totalBackups == 0 && totalErrors == 0 {
		fmt.Println("No backups found on target nodes")
	}
	if totalErrors == len(results) {
		return fmt.Errorf("failed to list backups on all target nodes")
	}
	return nil
}

func createBackupAcrossNodes(cfg *config.Config, envName string, servers map[string]config.ServerConfig, volumeName string, backupID string) error {
	fmt.Printf("=== Backing up volume: %s ===\n", volumeName)
	fmt.Printf("Backup ID: %s\n\n", backupID)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		info, err := createBackupOnNode(cfg, serverCfg, envName, volumeName, backupID)
		if backupVolumeMissing(err) {
			return backupNodeActionResult{skipped: []string{fmt.Sprintf("%s: volume not present on node", volumeName)}}, nil
		}
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{backups: []takod.BackupInfo{info}}, nil
	})

	return printBackupMutationResults(results, "backup", "No target node had that volume")
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

func restoreBackupOnNode(cfg *config.Config, envName string, serverName string, serverCfg config.ServerConfig, volumeName string, backupID string) error {
	client, err := connectBackupNode(serverCfg)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}
	defer client.Close()
	if verbose {
		fmt.Printf("Using node: %s (%s)\n", serverName, serverCfg.Host)
	}
	return restoreBackup(client, cfg, envName, volumeName, backupID)
}

func deleteBackupAcrossNodes(cfg *config.Config, envName string, servers map[string]config.ServerConfig, volumeName string, backupID string) error {
	fmt.Printf("=== Deleting backup: %s_%s ===\n\n", volumeName, backupID)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		err := deleteBackupOnNode(cfg, serverCfg, envName, volumeName, backupID)
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{deleted: 1}, nil
	})

	return printBackupDeleteResults(results)
}

func cleanupBackupsAcrossNodes(cfg *config.Config, envName string, servers map[string]config.ServerConfig, days int) error {
	fmt.Printf("=== Cleaning up backups older than %d days ===\n\n", days)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		response, err := cleanupBackupsOnNode(cfg, serverCfg, envName, days)
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{deleted: response.Deleted}, nil
	})

	return printBackupCleanupResults(results)
}

func backupAllVolumesAcrossNodes(cfg *config.Config, envName string, servers map[string]config.ServerConfig, backupID string) error {
	volumes, err := backupVolumesFromConfig(cfg, envName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	if len(volumes) == 0 {
		return fmt.Errorf("no named service volumes configured")
	}

	fmt.Printf("=== Backing up all configured volumes ===\n")
	fmt.Printf("Backup ID: %s\n\n", backupID)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		var payload backupNodeActionResult
		var failures []string
		for _, volume := range volumes {
			info, err := createBackupOnNode(cfg, serverCfg, envName, volume.name, backupID)
			if backupVolumeMissing(err) {
				payload.skipped = append(payload.skipped, fmt.Sprintf("%s: volume not present on node", volume.name))
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", volume.name, err))
				continue
			}
			info.Service = volume.service
			payload.backups = append(payload.backups, info)
		}
		if len(failures) > 0 {
			return payload, errors.New(strings.Join(failures, "; "))
		}
		return payload, nil
	})

	return printBackupMutationResults(results, "backup", "No configured volumes were present on target nodes")
}

type backupNodeActionResult struct {
	backups []takod.BackupInfo
	deleted int
	skipped []string
}

type backupNodeAction func(serverName string, serverCfg config.ServerConfig) (backupNodeActionResult, error)

type backupNodeResult struct {
	index      int
	serverName string
	host       string
	backupNodeActionResult
	err error
}

func collectBackupNodes(servers map[string]config.ServerConfig, action backupNodeAction) []backupNodeResult {
	names := sortedBackupServerNames(servers)
	resultCh := make(chan backupNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		serverCfg := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			payload, err := action(serverName, serverCfg)
			resultCh <- backupNodeResult{
				index:                  index,
				serverName:             serverName,
				host:                   serverCfg.Host,
				backupNodeActionResult: payload,
				err:                    err,
			}
		}(index, serverName, serverCfg)
	}
	wg.Wait()
	close(resultCh)

	results := make([]backupNodeResult, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func sortedBackupServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func printBackupGroups(backups []takod.BackupInfo) {
	byVolume := make(map[string][]takod.BackupInfo)
	for _, backup := range backups {
		byVolume[backup.Volume] = append(byVolume[backup.Volume], backup)
	}

	volumes := make([]string, 0, len(byVolume))
	for volume := range byVolume {
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)

	for _, volume := range volumes {
		fmt.Printf("  Volume: %s\n", volume)
		for _, backup := range byVolume[volume] {
			sizeStr := formatSize(backup.Size)
			fmt.Printf("    - %s  %s  %s\n", backup.ID, backup.CreatedAt.Format("2006-01-02 15:04"), sizeStr)
		}
	}
	fmt.Println()
}

func printBackupMutationResults(results []backupNodeResult, operation string, emptyMessage string) error {
	successes := 0
	failures := 0
	skipped := 0
	for _, result := range results {
		fmt.Printf("Node: %s (%s)\n", result.serverName, result.host)
		for _, message := range result.skipped {
			skipped++
			fmt.Printf("  Skipped: %s\n", message)
		}
		for _, backup := range result.backups {
			successes++
			sizeStr := formatSize(backup.Size)
			serviceLabel := ""
			if backup.Service != "" {
				serviceLabel = " (" + backup.Service + ")"
			}
			fmt.Printf("  Created: %s%s  %s  %s\n", backup.Volume, serviceLabel, backup.ID, sizeStr)
		}
		if result.err != nil {
			failures++
			fmt.Printf("  Failed: %v\n", result.err)
		}
		fmt.Println()
	}

	if successes == 0 && failures == 0 {
		if skipped > 0 {
			return errors.New(emptyMessage)
		}
		return fmt.Errorf("no target nodes selected for %s", operation)
	}
	if failures > 0 {
		return fmt.Errorf("%s completed with %d error(s)", operation, failures)
	}
	fmt.Printf("✓ %s completed on %d node volume(s)\n", backupOperationTitle(operation), successes)
	return nil
}

func printBackupDeleteResults(results []backupNodeResult) error {
	failures := 0
	deleted := 0
	for _, result := range results {
		fmt.Printf("Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			failures++
			fmt.Printf("  Failed: %v\n\n", result.err)
			continue
		}
		deleted += result.deleted
		fmt.Println("  Deleted")
		fmt.Println()
	}
	if failures > 0 {
		return fmt.Errorf("delete completed with %d error(s)", failures)
	}
	fmt.Printf("✓ Delete completed on %d node(s)\n", deleted)
	return nil
}

func printBackupCleanupResults(results []backupNodeResult) error {
	failures := 0
	totalDeleted := 0
	for _, result := range results {
		fmt.Printf("Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			failures++
			fmt.Printf("  Failed: %v\n\n", result.err)
			continue
		}
		totalDeleted += result.deleted
		fmt.Printf("  Cleaned up %d old backup(s)\n\n", result.deleted)
	}
	if failures > 0 {
		return fmt.Errorf("cleanup completed with %d error(s)", failures)
	}
	fmt.Printf("✓ Cleaned up %d old backup(s)\n", totalDeleted)
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

func readBackupsFromNode(cfg *config.Config, serverCfg config.ServerConfig, envName string, volumeName string) ([]takod.BackupInfo, error) {
	client, err := connectBackupNode(serverCfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	var response takod.BackupListResponse
	err = takodBackupRequestJSON(
		client,
		cfg,
		"GET",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, volumeName, ""),
		nil,
		&response,
	)
	if err != nil {
		return nil, err
	}
	return response.Backups, nil
}

func createBackupOnNode(cfg *config.Config, serverCfg config.ServerConfig, envName string, volumeName string, backupID string) (takod.BackupInfo, error) {
	client, err := connectBackupNode(serverCfg)
	if err != nil {
		return takod.BackupInfo{}, err
	}
	defer client.Close()

	var info takod.BackupInfo
	err = takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups",
		backupRequest(cfg, envName, volumeName, backupID, 0),
		&info,
	)
	if err != nil {
		return takod.BackupInfo{}, err
	}
	return info, nil
}

func deleteBackupOnNode(cfg *config.Config, serverCfg config.ServerConfig, envName string, volumeName string, backupID string) error {
	client, err := connectBackupNode(serverCfg)
	if err != nil {
		return err
	}
	defer client.Close()

	var response map[string]bool
	return takodBackupRequestJSON(
		client,
		cfg,
		"DELETE",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, volumeName, backupID),
		nil,
		&response,
	)
}

func cleanupBackupsOnNode(cfg *config.Config, serverCfg config.ServerConfig, envName string, days int) (takod.BackupCleanupResponse, error) {
	client, err := connectBackupNode(serverCfg)
	if err != nil {
		return takod.BackupCleanupResponse{}, err
	}
	defer client.Close()

	var response takod.BackupCleanupResponse
	err = takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups/cleanup",
		backupRequest(cfg, envName, "", "", days),
		&response,
	)
	if err != nil {
		return takod.BackupCleanupResponse{}, err
	}
	return response, nil
}

func connectBackupNode(serverCfg config.ServerConfig) (*ssh.Client, error) {
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverCfg.Host,
		Port:     serverCfg.Port,
		User:     serverCfg.User,
		SSHKey:   serverCfg.SSHKey,
		Password: serverCfg.Password,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Connect(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func backupVolumeMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "volume ") && strings.Contains(err.Error(), " does not exist")
}

func newBackupID() string {
	return time.Now().UTC().Format("20060102-150405")
}

func backupOperationTitle(operation string) string {
	if operation == "" {
		return ""
	}
	return strings.ToUpper(operation[:1]) + operation[1:]
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
