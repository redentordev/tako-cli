package cmd

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	volumeBackupServer       string
	volumeBackupID           string
	volumeBackupsServer      string
	volumeRestoreServer      string
	volumeRestoreForce       bool
	volumeBackupDeleteServer string
	volumeBackupDeleteForce  bool
)

var volumeBackupCmd = &cobra.Command{
	Use:               "backup SERVICE VOLUME",
	Short:             "Create a backup of a service volume on nodes where it exists",
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeVolumeBackupArgs,
	RunE:              runVolumeBackup,
}

var volumeBackupsCmd = &cobra.Command{
	Use:               "backups [SERVICE] [VOLUME]",
	Short:             "List volume backups on takod nodes",
	Args:              cobra.RangeArgs(0, 2),
	ValidArgsFunction: completeVolumeBackupArgs,
	RunE:              runVolumeBackups,
}

var volumeRestoreCmd = &cobra.Command{
	Use:               "restore SERVICE VOLUME BACKUP_ID",
	Short:             "Restore a service volume backup on nodes that have the backup",
	Args:              cobra.ExactArgs(3),
	ValidArgsFunction: completeVolumeRestoreArgs,
	RunE:              runVolumeRestore,
}

var volumeBackupDeleteCmd = &cobra.Command{
	Use:               "delete SERVICE VOLUME BACKUP_ID",
	Short:             "Delete a service volume backup from nodes that have it",
	Args:              cobra.ExactArgs(3),
	ValidArgsFunction: completeVolumeRestoreArgs,
	RunE:              runVolumeBackupDelete,
}

func init() {
	volumeCmd.AddCommand(volumeBackupCmd)
	volumeCmd.AddCommand(volumeBackupsCmd)
	volumeCmd.AddCommand(volumeRestoreCmd)
	volumeBackupCmd.AddCommand(volumeBackupDeleteCmd)

	volumeBackupCmd.Flags().StringVarP(&volumeBackupServer, "server", "s", "", "Node to query and mutate")
	volumeBackupCmd.Flags().StringVar(&volumeBackupID, "backup-id", "", "Backup ID to use (UTC timestamp format)")
	volumeBackupsCmd.Flags().StringVarP(&volumeBackupsServer, "server", "s", "", "Node to query")
	volumeRestoreCmd.Flags().StringVarP(&volumeRestoreServer, "server", "s", "", "Node to mutate")
	volumeRestoreCmd.Flags().BoolVar(&volumeRestoreForce, "force", false, "Confirm volume restore")
	volumeBackupDeleteCmd.Flags().StringVarP(&volumeBackupDeleteServer, "server", "s", "", "Node to mutate")
	volumeBackupDeleteCmd.Flags().BoolVar(&volumeBackupDeleteForce, "force", false, "Confirm backup deletion")
}

func runVolumeBackup(cmd *cobra.Command, args []string) error {
	ctx, err := newVolumeBackupCommandContext(volumeBackupServer)
	if err != nil {
		return err
	}
	defer ctx.pool.CloseAll()

	target, err := resolveServiceVolume(ctx.cfg, ctx.environment, args[0], args[1])
	if err != nil {
		return err
	}
	nodes, err := ctx.nodesWithVolume(target.DockerName)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("volume %s for %s/%s does not exist on selected node(s)", target.DockerName, target.Service, target.LogicalName)
	}

	backupID := strings.TrimSpace(volumeBackupID)
	if backupID == "" {
		backupID = time.Now().UTC().Format("20060102-150405")
	}
	var rows []volumeBackupRow
	for _, node := range nodes {
		var info takod.BackupInfo
		if err := ctx.requestJSON(node, "POST", "/v1/backups", backupRequest(ctx.cfg, ctx.environment, target, backupID), &info); err != nil {
			return fmt.Errorf("failed to create backup on %s: %w", node, err)
		}
		rows = append(rows, volumeBackupRow{Node: node, Service: target.Service, DockerName: target.DockerName, Backup: info})
	}
	printVolumeBackupRows(rows)
	return nil
}

func runVolumeBackups(cmd *cobra.Command, args []string) error {
	ctx, err := newVolumeBackupCommandContext(volumeBackupsServer)
	if err != nil {
		return err
	}
	defer ctx.pool.CloseAll()

	var (
		queryVolume string
		filters     map[string]resolvedServiceVolume
	)
	switch len(args) {
	case 1:
		volumes, err := listServiceBackupVolumes(ctx.cfg, ctx.environment, args[0])
		if err != nil {
			return err
		}
		filters = volumeFilterMap(volumes)
	case 2:
		target, err := resolveServiceVolume(ctx.cfg, ctx.environment, args[0], args[1])
		if err != nil {
			return err
		}
		queryVolume = target.BackupKey
		filters = volumeFilterMap([]resolvedServiceVolume{target})
	}

	var rows []volumeBackupRow
	for _, node := range ctx.serverNames {
		backups, err := ctx.listBackups(node, queryVolume)
		if err != nil {
			return err
		}
		for _, backup := range backups {
			target, ok := filters[backup.Volume]
			if len(filters) > 0 && !ok {
				continue
			}
			row := volumeBackupRow{Node: node, Backup: backup}
			if ok {
				row.Service = target.Service
				row.DockerName = target.DockerName
			}
			rows = append(rows, row)
		}
	}
	printVolumeBackupRows(rows)
	return nil
}

func runVolumeRestore(cmd *cobra.Command, args []string) error {
	if !volumeRestoreForce {
		return fmt.Errorf("--force is required to restore a volume backup")
	}
	ctx, err := newVolumeBackupCommandContext(volumeRestoreServer)
	if err != nil {
		return err
	}
	defer ctx.pool.CloseAll()

	target, err := resolveServiceVolume(ctx.cfg, ctx.environment, args[0], args[1])
	if err != nil {
		return err
	}
	backupID := args[2]
	nodes, err := ctx.nodesWithBackup(target.BackupKey, backupID)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("backup %s for %s/%s was not found on selected node(s)", backupID, target.Service, target.BackupKey)
	}
	for _, node := range nodes {
		if err := ctx.requestJSON(node, "POST", "/v1/backups/restore", backupRequest(ctx.cfg, ctx.environment, target, backupID), nil); err != nil {
			return fmt.Errorf("failed to restore backup on %s: %w", node, err)
		}
		fmt.Printf("%s: restored %s from backup %s\n", node, target.DockerName, backupID)
	}
	return nil
}

func runVolumeBackupDelete(cmd *cobra.Command, args []string) error {
	if !volumeBackupDeleteForce {
		return fmt.Errorf("--force is required to delete a volume backup")
	}
	ctx, err := newVolumeBackupCommandContext(volumeBackupDeleteServer)
	if err != nil {
		return err
	}
	defer ctx.pool.CloseAll()

	target, err := resolveServiceVolume(ctx.cfg, ctx.environment, args[0], args[1])
	if err != nil {
		return err
	}
	backupID := args[2]
	nodes, err := ctx.nodesWithBackup(target.BackupKey, backupID)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("backup %s for %s/%s was not found on selected node(s)", backupID, target.Service, target.BackupKey)
	}
	for _, node := range nodes {
		endpoint := takodclient.BackupsEndpoint(ctx.cfg.Project.Name, ctx.environment, target.BackupKey, backupID)
		if err := ctx.requestJSON(node, "DELETE", endpoint, nil, nil); err != nil {
			return fmt.Errorf("failed to delete backup on %s: %w", node, err)
		}
		fmt.Printf("%s: deleted backup %s for %s\n", node, backupID, target.BackupKey)
	}
	return nil
}

type volumeBackupCommandContext struct {
	cfg         *config.Config
	environment string
	pool        *ssh.Pool
	serverNames []string
}

func newVolumeBackupCommandContext(serverFlag string) (*volumeBackupCommandContext, error) {
	cfg, envName, pool, serverNames, err := resourceCommandContext(serverFlag)
	if err != nil {
		return nil, err
	}
	return &volumeBackupCommandContext{
		cfg:         cfg,
		environment: envName,
		pool:        pool,
		serverNames: serverNames,
	}, nil
}

func (ctx *volumeBackupCommandContext) requestJSON(serverName string, method string, endpoint string, value any, out any) error {
	server, ok := ctx.cfg.Servers[serverName]
	if !ok {
		return fmt.Errorf("server %s not found in config", serverName)
	}
	client, err := ctx.pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(ctx.cfg), method, endpoint, value)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal([]byte(output), out); err != nil {
			return fmt.Errorf("failed to parse takod response from %s: %w", serverName, err)
		}
	}
	return nil
}

func (ctx *volumeBackupCommandContext) nodesWithVolume(dockerName string) ([]string, error) {
	var nodes []string
	for _, node := range ctx.serverNames {
		var response takod.VolumeInspectResponse
		err := ctx.requestJSON(node, "POST", "/v1/volumes/inspect", takod.VolumeInspectRequest{
			Project:     ctx.cfg.Project.Name,
			Environment: ctx.environment,
			Volumes:     []string{dockerName},
		}, &response)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect volumes on %s: %w", node, err)
		}
		if response.Volumes[dockerName] {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func (ctx *volumeBackupCommandContext) listBackups(node string, volume string) ([]takod.BackupInfo, error) {
	var response takod.BackupListResponse
	endpoint := takodclient.BackupsEndpoint(ctx.cfg.Project.Name, ctx.environment, volume, "")
	if err := ctx.requestJSON(node, "GET", endpoint, nil, &response); err != nil {
		return nil, fmt.Errorf("failed to list backups on %s: %w", node, err)
	}
	return response.Backups, nil
}

func (ctx *volumeBackupCommandContext) nodesWithBackup(volume string, backupID string) ([]string, error) {
	var nodes []string
	for _, node := range ctx.serverNames {
		backups, err := ctx.listBackups(node, volume)
		if err != nil {
			return nil, err
		}
		for _, backup := range backups {
			if backup.Volume == volume && backup.ID == backupID {
				nodes = append(nodes, node)
				break
			}
		}
	}
	return nodes, nil
}

func backupRequest(cfg *config.Config, environment string, target resolvedServiceVolume, backupID string) takod.BackupRequest {
	return takod.BackupRequest{
		Project:     cfg.Project.Name,
		Environment: environment,
		Volume:      target.BackupKey,
		VolumeName:  target.DockerName,
		BackupID:    backupID,
	}
}

type resolvedServiceVolume struct {
	Service     string
	LogicalName string
	Target      string
	DockerName  string
	BackupKey   string
	Spec        string
}

type serviceVolumeCandidate struct {
	resolvedServiceVolume
	Bind bool
}

func resolveServiceVolume(cfg *config.Config, environment string, serviceName string, selector string) (resolvedServiceVolume, error) {
	candidates, err := serviceVolumeCandidates(cfg, environment, serviceName)
	if err != nil {
		return resolvedServiceVolume{}, err
	}
	var matches []serviceVolumeCandidate
	bindMatched := false
	for _, candidate := range candidates {
		if !volumeCandidateMatches(candidate, selector) {
			continue
		}
		if candidate.Bind {
			bindMatched = true
			continue
		}
		matches = append(matches, candidate)
	}
	if len(matches) == 0 {
		if bindMatched {
			return resolvedServiceVolume{}, fmt.Errorf("volume %s for service %s is a bind mount; bind mounts are not managed by tako volume backup", selector, serviceName)
		}
		return resolvedServiceVolume{}, fmt.Errorf("service %s has no Docker volume matching %s", serviceName, selector)
	}

	unique := make(map[string]serviceVolumeCandidate, len(matches))
	for _, match := range matches {
		key := match.DockerName + "\x00" + match.BackupKey
		unique[key] = match
	}
	if len(unique) > 1 {
		var values []string
		for _, match := range matches {
			values = append(values, fmt.Sprintf("%s -> %s", match.Spec, match.DockerName))
		}
		sort.Strings(values)
		return resolvedServiceVolume{}, fmt.Errorf("volume selector %s is ambiguous for service %s: %s", selector, serviceName, strings.Join(values, ", "))
	}
	for _, match := range unique {
		return match.resolvedServiceVolume, nil
	}
	return resolvedServiceVolume{}, fmt.Errorf("service %s has no Docker volume matching %s", serviceName, selector)
}

func listServiceBackupVolumes(cfg *config.Config, environment string, serviceName string) ([]resolvedServiceVolume, error) {
	candidates, err := serviceVolumeCandidates(cfg, environment, serviceName)
	if err != nil {
		return nil, err
	}
	var volumes []resolvedServiceVolume
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.Bind {
			continue
		}
		key := candidate.DockerName + "\x00" + candidate.BackupKey
		if seen[key] {
			continue
		}
		seen[key] = true
		volumes = append(volumes, candidate.resolvedServiceVolume)
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("service %s has no Docker volumes", serviceName)
	}
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].BackupKey < volumes[j].BackupKey
	})
	return volumes, nil
}

func serviceVolumeCandidates(cfg *config.Config, environment string, serviceName string) ([]serviceVolumeCandidate, error) {
	services, err := cfg.GetServices(environment)
	if err != nil {
		return nil, err
	}
	service, ok := services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %s not found in environment %s", serviceName, environment)
	}
	if len(service.Volumes) == 0 {
		return nil, fmt.Errorf("service %s has no volumes", serviceName)
	}

	candidates := make([]serviceVolumeCandidate, 0, len(service.Volumes))
	for _, spec := range service.Volumes {
		if config.IsNFSVolume(spec) {
			return nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}
		mount, err := config.ParseVolumeMountSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}
		if !mount.HasTarget {
			target := mount.Source
			source := operatorDockerVolumeName(cfg, environment, target)
			candidates = append(candidates, serviceVolumeCandidate{
				resolvedServiceVolume: resolvedServiceVolume{
					Service:     serviceName,
					LogicalName: target,
					Target:      target,
					DockerName:  source,
					BackupKey:   backupKeyForServiceVolume(target, target, source),
					Spec:        spec,
				},
			})
			continue
		}

		if strings.HasPrefix(mount.Source, "/") {
			candidates = append(candidates, serviceVolumeCandidate{
				resolvedServiceVolume: resolvedServiceVolume{
					Service:     serviceName,
					LogicalName: mount.Source,
					Target:      mount.Target,
					Spec:        spec,
				},
				Bind: true,
			})
			continue
		}

		dockerName := operatorDockerVolumeName(cfg, environment, mount.Source)
		candidates = append(candidates, serviceVolumeCandidate{
			resolvedServiceVolume: resolvedServiceVolume{
				Service:     serviceName,
				LogicalName: mount.Source,
				Target:      mount.Target,
				DockerName:  dockerName,
				BackupKey:   backupKeyForServiceVolume(mount.Source, mount.Target, dockerName),
				Spec:        spec,
			},
		})
	}
	return candidates, nil
}

func volumeCandidateMatches(candidate serviceVolumeCandidate, selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	for _, value := range volumeCandidateSelectors(candidate) {
		if value == selector {
			return true
		}
	}
	return false
}

func volumeCandidateSelectors(candidate serviceVolumeCandidate) []string {
	values := []string{
		candidate.LogicalName,
		candidate.Target,
		strings.Trim(candidate.Target, "/"),
		path.Base(candidate.Target),
		candidate.DockerName,
		candidate.BackupKey,
		candidate.Spec,
	}
	if strings.HasPrefix(candidate.LogicalName, "/") {
		values = append(values, strings.Trim(candidate.LogicalName, "/"), path.Base(candidate.LogicalName))
	}
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == "." || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func backupKeyForServiceVolume(logicalName string, target string, dockerName string) string {
	for _, value := range []string{logicalName, strings.Trim(target, "/"), path.Base(target), dockerName} {
		if key := sanitizeBackupKey(value); key != "" {
			return key
		}
	}
	return "volume"
}

func sanitizeBackupKey(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var out strings.Builder
	lastSeparator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastSeparator = false
			continue
		}
		if out.Len() == 0 || lastSeparator {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			out.WriteRune(r)
			lastSeparator = true
			continue
		}
		if r <= ' ' || r == '/' {
			out.WriteRune('_')
			lastSeparator = true
		}
	}
	key := strings.Trim(out.String(), "_.-")
	if len(key) > 96 {
		key = strings.Trim(key[:96], "_.-")
	}
	return key
}

func volumeFilterMap(volumes []resolvedServiceVolume) map[string]resolvedServiceVolume {
	out := make(map[string]resolvedServiceVolume, len(volumes))
	for _, volume := range volumes {
		out[volume.BackupKey] = volume
	}
	return out
}

type volumeBackupRow struct {
	Node       string
	Service    string
	DockerName string
	Backup     takod.BackupInfo
}

func printVolumeBackupRows(rows []volumeBackupRow) {
	if len(rows) == 0 {
		fmt.Println("No volume backups found")
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Node == rows[j].Node {
			if rows[i].Backup.Volume == rows[j].Backup.Volume {
				return rows[i].Backup.ID > rows[j].Backup.ID
			}
			return rows[i].Backup.Volume < rows[j].Backup.Volume
		}
		return rows[i].Node < rows[j].Node
	})
	fmt.Printf("%-14s %-16s %-20s %-16s %-10s %s\n", "NODE", "SERVICE", "VOLUME", "BACKUP", "SIZE", "CREATED")
	fmt.Println(strings.Repeat("-", 92))
	for _, row := range rows {
		service := row.Service
		if service == "" {
			service = "-"
		}
		fmt.Printf(
			"%-14s %-16s %-20s %-16s %-10s %s\n",
			row.Node,
			truncateResource(service, 16),
			truncateResource(row.Backup.Volume, 20),
			row.Backup.ID,
			formatBackupSize(row.Backup.Size),
			formatBackupCreated(row.Backup.CreatedAt),
		)
	}
}

func formatBackupSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
	}
	return fmt.Sprintf("%.1fPiB", value/1024)
}

func formatBackupCreated(created time.Time) string {
	if created.IsZero() {
		return "-"
	}
	return created.UTC().Format(time.RFC3339)
}

func completeVolumeBackupArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeServicesForFlag(cmd, args, toComplete)
	case 1:
		return completeServiceVolumeSelectors(args[0], toComplete), cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func completeVolumeRestoreArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeServicesForFlag(cmd, args, toComplete)
	case 1:
		return completeServiceVolumeSelectors(args[0], toComplete), cobra.ShellCompDirectiveNoFileComp
	case 2:
		return filterCompletions(completeBackupIDs(cmd, args[0], args[1]), toComplete), cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func completeServiceVolumeSelectors(serviceName string, toComplete string) []string {
	cfg, err := completionConfig()
	if err != nil {
		return nil
	}
	envName := getEnvironmentName(cfg)
	candidates, err := serviceVolumeCandidates(cfg, envName, serviceName)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, candidate := range candidates {
		if candidate.Bind {
			continue
		}
		for _, value := range volumeCandidateSelectors(candidate) {
			if strings.Contains(value, ":") || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	return filterCompletions(out, toComplete)
}

func completeBackupIDs(cmd *cobra.Command, serviceName string, volumeSelector string) []string {
	cfg, err := completionConfig()
	if err != nil {
		return nil
	}
	envName := getEnvironmentName(cfg)
	target, err := resolveServiceVolume(cfg, envName, serviceName, volumeSelector)
	if err != nil {
		return nil
	}
	values := completeRemoteResources(cmd, "server", func(client takodclient.RequestExecutor, cfg *config.Config, envName string) ([]string, error) {
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.BackupsEndpoint(cfg.Project.Name, envName, target.BackupKey, ""), nil)
		if err != nil {
			return nil, err
		}
		var response takod.BackupListResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(response.Backups))
		for _, backup := range response.Backups {
			if backup.Volume == target.BackupKey {
				ids = append(ids, backup.ID)
			}
		}
		return ids, nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(values)))
	return values
}
