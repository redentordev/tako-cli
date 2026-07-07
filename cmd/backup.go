package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
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
	Use:          "backup",
	Short:        "Backup and restore service volumes",
	SilenceUsage: true,
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
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	if backupList {
		return listBackupsAcrossNodes(cfg, sshPool, envName, servers, backupVolume)
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

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServerNames, "backup")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Fprintf(humanOut(), "→ Acquired remote backup leases: %s\n", leaseSet.Summary())
	}

	switch {
	case backupRestore != "":
		serverName := targetServerNames[0]
		return restoreBackupOnNode(cfg, sshPool, envName, serverName, servers[serverName], backupVolume, backupRestore)

	case backupDelete != "":
		return deleteBackupAcrossNodes(cfg, sshPool, envName, servers, backupVolume, backupDelete)

	case backupCleanup > 0:
		return cleanupBackupsAcrossNodes(cfg, sshPool, envName, servers, backupCleanup)

	case backupAll:
		return backupAllVolumesAcrossNodes(cfg, sshPool, envName, servers, newBackupID())

	case backupVolume != "":
		return createBackupAcrossNodes(cfg, sshPool, envName, servers, backupVolume, newBackupID())

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

func listBackupsAcrossNodes(cfg *config.Config, pool sshClientProvider, envName string, servers map[string]config.ServerConfig, volumeName string) error {
	fmt.Fprintf(humanOut(), "=== Volume Backups ===\n\n")

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		backups, err := readBackupsFromNode(cfg, pool, serverCfg, envName, volumeName)
		return backupNodeActionResult{backups: backups}, err
	})

	totalBackups := 0
	totalErrors := 0
	for _, result := range results {
		fmt.Fprintf(humanOut(), "Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			totalErrors++
			fmt.Fprintf(humanOut(), "  Failed: %v\n\n", result.err)
			continue
		}
		totalBackups += len(result.backups)
		if len(result.backups) == 0 {
			fmt.Fprintln(humanOut(), "  No backups found")
			fmt.Fprintln(humanOut())
			continue
		}
		printBackupGroups(result.backups)
	}

	if totalBackups == 0 && totalErrors == 0 {
		fmt.Fprintln(humanOut(), "No backups found on target nodes")
	}
	var err error
	if totalErrors == len(results) {
		err = fmt.Errorf("failed to list backups on all target nodes")
	}
	return emitBackupResult(cfg, envName, engine.BackupActionList, volumeName, "", results, err)
}

func createBackupAcrossNodes(cfg *config.Config, pool sshClientProvider, envName string, servers map[string]config.ServerConfig, volumeName string, backupID string) error {
	fmt.Fprintf(humanOut(), "=== Backing up volume: %s ===\n", volumeName)
	fmt.Fprintf(humanOut(), "Backup ID: %s\n\n", backupID)

	spec, err := backupVolumeSpecForName(cfg, envName, volumeName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		info, err := createBackupOnNode(cfg, pool, serverCfg, envName, spec, backupID)
		if backupVolumeMissing(err) {
			return backupNodeActionResult{skipped: []string{fmt.Sprintf("%s: volume not present on node", volumeName)}}, nil
		}
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{backups: []takod.BackupInfo{info}}, nil
	})

	err = printBackupMutationResults(results, "backup", "No target node had that volume")
	return emitBackupResult(cfg, envName, engine.BackupActionCreate, volumeName, backupID, results, err)
}

func restoreBackup(client *ssh.Client, cfg *config.Config, envName string, volumeName string, backupID string) error {
	fmt.Fprintf(humanOut(), "=== Restoring volume: %s from backup %s ===\n\n", volumeName, backupID)
	fmt.Fprintf(humanOut(), "⚠️  WARNING: This will overwrite all data in the volume!\n\n")

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

	fmt.Fprintf(humanOut(), "\n✓ Volume restored successfully\n")
	return nil
}

func restoreBackupOnNode(cfg *config.Config, pool sshClientProvider, envName string, serverName string, serverCfg config.ServerConfig, volumeName string, backupID string) error {
	client, err := connectBackupNode(pool, serverCfg)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}
	if verbose {
		fmt.Fprintf(humanOut(), "Using node: %s (%s)\n", serverName, serverCfg.Host)
	}
	err = restoreBackup(client, cfg, envName, volumeName, backupID)
	results := []backupNodeResult{{serverName: serverName, host: serverCfg.Host, err: err}}
	return emitBackupResult(cfg, envName, engine.BackupActionRestore, volumeName, backupID, results, err)
}

func deleteBackupAcrossNodes(cfg *config.Config, pool sshClientProvider, envName string, servers map[string]config.ServerConfig, volumeName string, backupID string) error {
	fmt.Fprintf(humanOut(), "=== Deleting backup: %s_%s ===\n\n", volumeName, backupID)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		err := deleteBackupOnNode(cfg, pool, serverCfg, envName, volumeName, backupID)
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{deleted: 1}, nil
	})

	err := printBackupDeleteResults(results)
	return emitBackupResult(cfg, envName, engine.BackupActionDelete, volumeName, backupID, results, err)
}

func cleanupBackupsAcrossNodes(cfg *config.Config, pool sshClientProvider, envName string, servers map[string]config.ServerConfig, days int) error {
	fmt.Fprintf(humanOut(), "=== Cleaning up backups older than %d days ===\n\n", days)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		response, err := cleanupBackupsOnNode(cfg, pool, serverCfg, envName, days)
		if err != nil {
			return backupNodeActionResult{}, err
		}
		return backupNodeActionResult{deleted: response.Deleted}, nil
	})

	err := printBackupCleanupResults(results)
	return emitBackupResult(cfg, envName, engine.BackupActionCleanup, "", "", results, err)
}

func backupAllVolumesAcrossNodes(cfg *config.Config, pool sshClientProvider, envName string, servers map[string]config.ServerConfig, backupID string) error {
	volumes, err := backupVolumesFromConfig(cfg, envName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	if len(volumes) == 0 {
		return fmt.Errorf("no named service volumes configured")
	}

	fmt.Fprintf(humanOut(), "=== Backing up all configured volumes ===\n")
	fmt.Fprintf(humanOut(), "Backup ID: %s\n\n", backupID)

	results := collectBackupNodes(servers, func(_ string, serverCfg config.ServerConfig) (backupNodeActionResult, error) {
		var payload backupNodeActionResult
		var failures []string
		for _, volume := range volumes {
			info, err := createBackupOnNode(cfg, pool, serverCfg, envName, volume, backupID)
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

	err = printBackupMutationResults(results, "backup", "No configured volumes were present on target nodes")
	return emitBackupResult(cfg, envName, engine.BackupActionCreate, "", backupID, results, err)
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

// emitBackupResult builds the versioned BackupResult document from per-node
// outcomes, emits it in machine modes, and passes the operation error through.
func emitBackupResult(cfg *config.Config, envName string, action string, volume string, backupID string, results []backupNodeResult, opErr error) error {
	doc := engine.BackupResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindBackupResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Action:      action,
		Volume:      volume,
		BackupID:    backupID,
		Nodes:       []engine.BackupNodeOutcome{},
	}
	for _, result := range results {
		outcome := engine.BackupNodeOutcome{
			Server:  result.serverName,
			Host:    result.host,
			Backups: result.backups,
			Deleted: result.deleted,
			Skipped: result.skipped,
		}
		if result.err != nil {
			outcome.Error = result.err.Error()
		}
		doc.Nodes = append(doc.Nodes, outcome)
	}
	if opErr != nil {
		doc.Error = opErr.Error()
	}
	if emitErr := emitResultDocument(doc); emitErr != nil && opErr == nil {
		return emitErr
	}
	return opErr
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
		fmt.Fprintf(humanOut(), "  Volume: %s\n", volume)
		for _, backup := range byVolume[volume] {
			sizeStr := formatSize(backup.Size)
			fmt.Fprintf(humanOut(), "    - %s  %s  %s\n", backup.ID, backup.CreatedAt.Format("2006-01-02 15:04"), sizeStr)
		}
	}
	fmt.Fprintln(humanOut())
}

func printBackupMutationResults(results []backupNodeResult, operation string, emptyMessage string) error {
	successes := 0
	failures := 0
	skipped := 0
	for _, result := range results {
		fmt.Fprintf(humanOut(), "Node: %s (%s)\n", result.serverName, result.host)
		for _, message := range result.skipped {
			skipped++
			fmt.Fprintf(humanOut(), "  Skipped: %s\n", message)
		}
		for _, backup := range result.backups {
			successes++
			sizeStr := formatSize(backup.Size)
			serviceLabel := ""
			if backup.Service != "" {
				serviceLabel = " (" + backup.Service + ")"
			}
			fmt.Fprintf(humanOut(), "  Created: %s%s  %s  %s\n", backup.Volume, serviceLabel, backup.ID, sizeStr)
			if backup.Remote != nil {
				fmt.Fprintf(humanOut(), "    Remote: %s://%s/%s\n", backup.Remote.Provider, backup.Remote.Bucket, backup.Remote.Key)
			}
			for _, warning := range backup.Warnings {
				fmt.Fprintf(humanOut(), "    Warning: %s\n", warning)
			}
		}
		if result.err != nil {
			failures++
			fmt.Fprintf(humanOut(), "  Failed: %v\n", result.err)
		}
		fmt.Fprintln(humanOut())
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
	fmt.Fprintf(humanOut(), "✓ %s completed on %d node volume(s)\n", backupOperationTitle(operation), successes)
	return nil
}

func printBackupDeleteResults(results []backupNodeResult) error {
	failures := 0
	deleted := 0
	for _, result := range results {
		fmt.Fprintf(humanOut(), "Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			failures++
			fmt.Fprintf(humanOut(), "  Failed: %v\n\n", result.err)
			continue
		}
		deleted += result.deleted
		fmt.Fprintln(humanOut(), "  Deleted")
		fmt.Fprintln(humanOut())
	}
	if failures > 0 {
		return fmt.Errorf("delete completed with %d error(s)", failures)
	}
	fmt.Fprintf(humanOut(), "✓ Delete completed on %d node(s)\n", deleted)
	return nil
}

func printBackupCleanupResults(results []backupNodeResult) error {
	failures := 0
	totalDeleted := 0
	for _, result := range results {
		fmt.Fprintf(humanOut(), "Node: %s (%s)\n", result.serverName, result.host)
		if result.err != nil {
			failures++
			fmt.Fprintf(humanOut(), "  Failed: %v\n\n", result.err)
			continue
		}
		totalDeleted += result.deleted
		fmt.Fprintf(humanOut(), "  Cleaned up %d old backup(s)\n\n", result.deleted)
	}
	if failures > 0 {
		return fmt.Errorf("cleanup completed with %d error(s)", failures)
	}
	fmt.Fprintf(humanOut(), "✓ Cleaned up %d old backup(s)\n", totalDeleted)
	return nil
}

type backupVolumeSpec struct {
	name          string
	service       string
	retentionDays int
	storage       *config.BackupStorageConfig
}

func backupVolumesFromConfig(cfg *config.Config, envName string) ([]backupVolumeSpec, error) {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]backupVolumeSpec)
	serviceNames := make([]string, 0, len(services))
	for serviceName := range services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		service := services[serviceName]
		for _, spec := range backupVolumeSpecsForService(serviceName, service) {
			existing, ok := seen[spec.name]
			if !ok || (existing.storage == nil && spec.storage != nil) {
				seen[spec.name] = spec
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

func backupVolumeSpecForName(cfg *config.Config, envName string, volumeName string) (backupVolumeSpec, error) {
	volumes, err := backupVolumesFromConfig(cfg, envName)
	if err != nil {
		return backupVolumeSpec{}, err
	}
	for _, volume := range volumes {
		if volume.name == volumeName {
			return volume, nil
		}
	}
	return backupVolumeSpec{name: volumeName}, nil
}

func backupVolumeSpecsForService(serviceName string, service config.ServiceConfig) []backupVolumeSpec {
	backupVolumeSet := map[string]bool{}
	if service.Backup != nil && len(service.Backup.Volumes) > 0 {
		for _, volume := range service.Backup.Volumes {
			backupVolumeSet[strings.TrimSpace(volume)] = true
		}
	}

	var specs []backupVolumeSpec
	for _, volume := range service.Volumes {
		source, ok := backupVolumeNameFromSpec(volume)
		if !ok {
			continue
		}
		spec := backupVolumeSpec{name: source, service: serviceName}
		if service.Backup != nil && (len(backupVolumeSet) == 0 || backupVolumeSet[source]) {
			spec.retentionDays = service.Backup.Retain
			spec.storage = cloneConfigBackupStorage(service.Backup.Storage)
		}
		specs = append(specs, spec)
	}
	return specs
}

func backupVolumeNameFromSpec(volume string) (string, bool) {
	source, target, hasTarget := strings.Cut(volume, ":")
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if source == "" {
		return "", false
	}
	if !hasTarget {
		return source, true
	}
	if target == "" || strings.HasPrefix(source, "/") || config.IsNFSVolume(volume) {
		return "", false
	}
	return source, true
}

func backupRequest(cfg *config.Config, envName string, volumeName string, backupID string, retentionDays int) takod.BackupRequest {
	return backupRequestForSpec(cfg, envName, backupVolumeSpec{name: volumeName, retentionDays: retentionDays}, backupID)
}

func backupRequestForSpec(cfg *config.Config, envName string, volume backupVolumeSpec, backupID string) takod.BackupRequest {
	request := takod.BackupRequest{
		Project:       cfg.Project.Name,
		Environment:   envName,
		Volume:        backupArchiveVolumeName(volume.name),
		BackupID:      backupID,
		RetentionDays: volume.retentionDays,
		Storage:       takodBackupStorageFromConfig(volume.storage),
	}
	if volume.name != "" {
		request.DockerVolume = cfg.GetVolumeName(volume.name, envName)
		request.ExternalVolume = cfg.IsVolumeExternal(volume.name)
	}
	return request
}

func readBackupsFromNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, envName string, volumeName string) ([]takod.BackupInfo, error) {
	client, err := connectBackupNode(pool, serverCfg)
	if err != nil {
		return nil, err
	}

	var response takod.BackupListResponse
	err = takodBackupRequestJSON(
		client,
		cfg,
		"GET",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, backupArchiveVolumeName(volumeName), ""),
		nil,
		&response,
	)
	if err != nil {
		return nil, err
	}
	return response.Backups, nil
}

func createBackupOnNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, envName string, volume backupVolumeSpec, backupID string) (takod.BackupInfo, error) {
	client, err := connectBackupNode(pool, serverCfg)
	if err != nil {
		return takod.BackupInfo{}, err
	}

	var info takod.BackupInfo
	err = takodBackupRequestJSON(
		client,
		cfg,
		"POST",
		"/v1/backups",
		backupRequestForSpec(cfg, envName, volume, backupID),
		&info,
	)
	if err != nil {
		return takod.BackupInfo{}, err
	}
	return info, nil
}

func deleteBackupOnNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, envName string, volumeName string, backupID string) error {
	client, err := connectBackupNode(pool, serverCfg)
	if err != nil {
		return err
	}

	var response map[string]bool
	return takodBackupRequestJSON(
		client,
		cfg,
		"DELETE",
		takodclient.BackupsEndpoint(cfg.Project.Name, envName, backupArchiveVolumeName(volumeName), backupID),
		nil,
		&response,
	)
}

func cleanupBackupsOnNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, envName string, days int) (takod.BackupCleanupResponse, error) {
	client, err := connectBackupNode(pool, serverCfg)
	if err != nil {
		return takod.BackupCleanupResponse{}, err
	}

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

func connectBackupNode(pool sshClientProvider, serverCfg config.ServerConfig) (*ssh.Client, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func backupVolumeMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "volume ") && strings.Contains(err.Error(), " does not exist")
}

func backupArchiveVolumeName(volumeName string) string {
	volumeName = strings.TrimSpace(volumeName)
	if volumeName == "" {
		return ""
	}
	if isBackupArchiveVolumeName(volumeName) {
		return volumeName
	}
	trimmed := strings.Trim(volumeName, "/")
	var out strings.Builder
	lastWasSeparator := false
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastWasSeparator = false
			continue
		}
		if !lastWasSeparator {
			out.WriteRune('_')
			lastWasSeparator = true
		}
	}
	cleaned := strings.Trim(out.String(), "_")
	if cleaned == "" {
		return "volume"
	}
	return cleaned
}

func cloneConfigBackupStorage(storage *config.BackupStorageConfig) *config.BackupStorageConfig {
	if storage == nil {
		return nil
	}
	copied := *storage
	return &copied
}

func takodBackupStorageFromConfig(storage *config.BackupStorageConfig) *takod.BackupStorageConfig {
	if storage == nil {
		return nil
	}
	return &takod.BackupStorageConfig{
		Provider:        storage.Provider,
		Bucket:          storage.Bucket,
		Region:          storage.Region,
		Endpoint:        storage.Endpoint,
		Prefix:          storage.Prefix,
		AccessKeyID:     storage.AccessKeyID,
		SecretAccessKey: storage.SecretAccessKey,
		SessionToken:    storage.SessionToken,
		ForcePathStyle:  storage.ForcePathStyle,
	}
}

func isBackupArchiveVolumeName(value string) bool {
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
