package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var prometheusCmd = &cobra.Command{
	Use:   "prometheus",
	Short: "Export metrics in Prometheus format",
	Long: `Export system and container metrics in Prometheus exposition format.

This command outputs metrics that can be scraped by Prometheus or viewed directly.
The output follows the Prometheus text-based exposition format.`,
	RunE: runPrometheus,
}

func init() {
	rootCmd.AddCommand(prometheusCmd)
}

func runPrometheus(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Determine environment
	envName := getEnvironmentName(cfg)

	// Get servers for environment
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer factory.CloseIdleConnections()

	alertOwners := map[string]bool{}
	if schedulerAlertsConfigured(cfg) {
		owners, err := schedulerAlertOwners(cfg.Servers, servers)
		if err != nil {
			return err
		}
		for _, owner := range owners {
			alertOwners[owner] = true
		}
		exportAlertOwnerPresence(os.Stdout, cfg.Project.Name, envName, len(owners) > 0)
	}
	alertMetadata, monitorMetadata := true, true
	for _, result := range collectPrometheusNodes(cfg, envName, servers, factory) {
		if alertOwners[result.serverName] {
			exportAlertMetrics(os.Stdout, result.serverName, result.host, cfg.Project.Name, envName, result.alertStatus, result.alertsRead, alertMetadata)
			alertMetadata = false
		}
		if result.alertsRead && result.alertStatus != nil && result.alertStatus.Sources != nil {
			exportMonitorMetrics(os.Stdout, result.serverName, cfg.Project.Name, envName, result.alertStatus.Sources, monitorMetadata)
			monitorMetadata = false
		}
		if result.err != nil || result.metrics == nil {
			continue
		}
		exportSystemMetrics(result.serverName, result.host, result.metrics)
		if len(result.stats) > 0 {
			exportContainerMetrics(result.serverName, result.host, result.stats)
		}
	}

	return nil
}

func schedulerAlertOwners(configured map[string]config.ServerConfig, environmentServers []string) ([]string, error) {
	owners := make([]string, 0, len(environmentServers))
	for _, serverName := range environmentServers {
		server, ok := configured[serverName]
		if !ok {
			return nil, fmt.Errorf("target server %s is not defined", serverName)
		}
		if server.Schedulable() {
			owners = append(owners, serverName)
		}
	}
	return owners, nil
}

type prometheusNodeResult struct {
	index       int
	serverName  string
	host        string
	metrics     *MetricsData
	stats       []takod.ContainerStat
	alertStatus *takod.AlertStatusResponse
	alertsRead  bool
	err         error
}

type prometheusReadFunc func(serverName string, server config.ServerConfig) (*MetricsData, []takod.ContainerStat, *takod.AlertStatusResponse, bool, error)

func collectPrometheusNodes(cfg *config.Config, envName string, serverNames []string, factory *nodeclient.Factory) []prometheusNodeResult {
	return collectPrometheusNodesWith(cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) (*MetricsData, []takod.ContainerStat, *takod.AlertStatusResponse, bool, error) {
		client, _, err := factory.Client(context.Background(), serverName)
		if err != nil {
			return nil, nil, nil, false, err
		}
		alertStatus, alertErr := readAlertStatusViaTakod(client, cfg, envName)
		metrics, err := readMetricsViaTakod(client, cfg, true)
		if err != nil {
			return nil, nil, alertStatus, alertErr == nil, err
		}

		statsResponse, err := readStatsViaTakodWithOptions(client, cfg, envName, "", false)
		if err != nil {
			return metrics, nil, alertStatus, alertErr == nil, nil
		}
		return metrics, statsResponse.Stats, alertStatus, alertErr == nil, nil
	})
}

func collectPrometheusNodesWith(configuredServers map[string]config.ServerConfig, serverNames []string, read prometheusReadFunc) []prometheusNodeResult {
	resultCh := make(chan prometheusNodeResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := configuredServers[serverName]
		if !ok {
			resultCh <- prometheusNodeResult{
				index:      index,
				serverName: serverName,
				err:        fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			metrics, stats, alertStatus, alertsRead, err := read(serverName, server)
			resultCh <- prometheusNodeResult{
				index: index, serverName: serverName, host: server.Host, metrics: metrics, stats: stats,
				alertStatus: alertStatus, alertsRead: alertsRead, err: err,
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]prometheusNodeResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func exportSystemMetrics(serverName, serverHost string, metrics *MetricsData) {
	labels := fmt.Sprintf(`{server="%s",host="%s"}`, serverName, serverHost)

	// CPU
	fmt.Printf("# HELP tako_cpu_usage_percent CPU usage percentage\n")
	fmt.Printf("# TYPE tako_cpu_usage_percent gauge\n")
	fmt.Printf("tako_cpu_usage_percent%s %s\n", labels, metrics.CPUPercent)
	fmt.Println()

	// Memory
	fmt.Printf("# HELP tako_memory_total_bytes Total memory in bytes\n")
	fmt.Printf("# TYPE tako_memory_total_bytes gauge\n")
	fmt.Printf("tako_memory_total_bytes%s %d\n", labels, metrics.Memory.TotalMB*1024*1024)
	fmt.Println()

	fmt.Printf("# HELP tako_memory_used_bytes Used memory in bytes\n")
	fmt.Printf("# TYPE tako_memory_used_bytes gauge\n")
	fmt.Printf("tako_memory_used_bytes%s %d\n", labels, metrics.Memory.UsedMB*1024*1024)
	fmt.Println()

	fmt.Printf("# HELP tako_memory_usage_percent Memory usage percentage\n")
	fmt.Printf("# TYPE tako_memory_usage_percent gauge\n")
	fmt.Printf("tako_memory_usage_percent%s %s\n", labels, metrics.Memory.Percent)
	fmt.Println()

	// Swap
	if metrics.Memory.SwapTotalMB > 0 {
		fmt.Printf("# HELP tako_swap_total_bytes Total swap in bytes\n")
		fmt.Printf("# TYPE tako_swap_total_bytes gauge\n")
		fmt.Printf("tako_swap_total_bytes%s %d\n", labels, metrics.Memory.SwapTotalMB*1024*1024)
		fmt.Println()

		fmt.Printf("# HELP tako_swap_used_bytes Used swap in bytes\n")
		fmt.Printf("# TYPE tako_swap_used_bytes gauge\n")
		fmt.Printf("tako_swap_used_bytes%s %d\n", labels, metrics.Memory.SwapUsedMB*1024*1024)
		fmt.Println()
	}

	// Disk
	fmt.Printf("# HELP tako_disk_total_bytes Total disk space in bytes\n")
	fmt.Printf("# TYPE tako_disk_total_bytes gauge\n")
	fmt.Printf("tako_disk_total_bytes%s %d\n", labels, metrics.Disk.TotalMB*1024*1024)
	fmt.Println()

	fmt.Printf("# HELP tako_disk_used_bytes Used disk space in bytes\n")
	fmt.Printf("# TYPE tako_disk_used_bytes gauge\n")
	fmt.Printf("tako_disk_used_bytes%s %d\n", labels, metrics.Disk.UsedMB*1024*1024)
	fmt.Println()

	fmt.Printf("# HELP tako_disk_usage_percent Disk usage percentage\n")
	fmt.Printf("# TYPE tako_disk_usage_percent gauge\n")
	fmt.Printf("tako_disk_usage_percent%s %s\n", labels, metrics.Disk.Percent)
	fmt.Println()

	// Network I/O
	if metrics.Network.RxBytes > 0 || metrics.Network.TxBytes > 0 {
		fmt.Printf("# HELP tako_network_receive_bytes Network bytes received\n")
		fmt.Printf("# TYPE tako_network_receive_bytes counter\n")
		fmt.Printf("tako_network_receive_bytes%s %d\n", labels, metrics.Network.RxBytes)
		fmt.Println()

		fmt.Printf("# HELP tako_network_transmit_bytes Network bytes transmitted\n")
		fmt.Printf("# TYPE tako_network_transmit_bytes counter\n")
		fmt.Printf("tako_network_transmit_bytes%s %d\n", labels, metrics.Network.TxBytes)
		fmt.Println()
	}

	// Disk I/O
	if metrics.DiskIO.ReadSectors > 0 || metrics.DiskIO.WriteSectors > 0 {
		fmt.Printf("# HELP tako_disk_read_bytes Disk bytes read\n")
		fmt.Printf("# TYPE tako_disk_read_bytes counter\n")
		fmt.Printf("tako_disk_read_bytes%s %d\n", labels, metrics.DiskIO.ReadSectors*512)
		fmt.Println()

		fmt.Printf("# HELP tako_disk_write_bytes Disk bytes written\n")
		fmt.Printf("# TYPE tako_disk_write_bytes counter\n")
		fmt.Printf("tako_disk_write_bytes%s %d\n", labels, metrics.DiskIO.WriteSectors*512)
		fmt.Println()
	}

	// Load average
	fmt.Printf("# HELP tako_load_average_1m Load average 1 minute\n")
	fmt.Printf("# TYPE tako_load_average_1m gauge\n")
	fmt.Printf("tako_load_average_1m%s %s\n", labels, metrics.LoadAverage.OneMin)
	fmt.Println()

	fmt.Printf("# HELP tako_load_average_5m Load average 5 minutes\n")
	fmt.Printf("# TYPE tako_load_average_5m gauge\n")
	fmt.Printf("tako_load_average_5m%s %s\n", labels, metrics.LoadAverage.FiveMin)
	fmt.Println()

	fmt.Printf("# HELP tako_load_average_15m Load average 15 minutes\n")
	fmt.Printf("# TYPE tako_load_average_15m gauge\n")
	fmt.Printf("tako_load_average_15m%s %s\n", labels, metrics.LoadAverage.FifteenMin)
	fmt.Println()

	// Uptime
	fmt.Printf("# HELP tako_uptime_seconds System uptime in seconds\n")
	fmt.Printf("# TYPE tako_uptime_seconds counter\n")
	fmt.Printf("tako_uptime_seconds%s %d\n", labels, metrics.UptimeSeconds)
	fmt.Println()
}

func schedulerAlertsConfigured(cfg *config.Config) bool {
	if cfg == nil || cfg.Notifications == nil || cfg.Notifications.Alerts == nil {
		return false
	}
	alerts := cfg.Notifications.Alerts
	return alerts.Jobs.ConsecutiveFailures > 0 || alerts.Jobs.ConsecutiveTimeouts > 0 || alerts.Jobs.ConsecutiveSkipped > 0 || strings.TrimSpace(alerts.Jobs.NoSuccessFor) != "" || strings.TrimSpace(alerts.Backups.MaxAge) != ""
}

func exportAlertOwnerPresence(out io.Writer, project, environment string, present bool) {
	value := 0
	if present {
		value = 1
	}
	labels := fmt.Sprintf(`{project=%s,environment=%s}`, strconv.Quote(project), strconv.Quote(environment))
	fmt.Fprintln(out, "# HELP tako_scheduler_alert_owner_present Whether this environment has at least one schedulable alert owner")
	fmt.Fprintln(out, "# TYPE tako_scheduler_alert_owner_present gauge")
	fmt.Fprintf(out, "tako_scheduler_alert_owner_present%s %d\n\n", labels, value)
}

func exportAlertMetrics(out io.Writer, serverName, serverHost, project, environment string, status *takod.AlertStatusResponse, reachable, includeMetadata bool) {
	labels := fmt.Sprintf(`{server=%s,host=%s,project=%s,environment=%s}`, strconv.Quote(serverName), strconv.Quote(serverHost), strconv.Quote(project), strconv.Quote(environment))
	if includeMetadata {
		for _, metric := range []struct{ name, help string }{
			{"tako_scheduler_alert_owner_reachable", "Whether the external observer reached the scheduler owner node"},
			{"tako_scheduler_alert_policy_loaded", "Whether the scheduler alert policy is loaded on the node"},
			{"tako_scheduler_alert_active_episodes", "Active scheduler alert episodes"},
			{"tako_scheduler_alert_pending_deliveries", "Pending scheduler alert deliveries"},
			{"tako_scheduler_alert_exhausted_deliveries", "Exhausted scheduler alert deliveries"},
			{"tako_scheduler_alert_last_evaluation_timestamp_seconds", "Last scheduler alert evaluation time"},
			{"tako_scheduler_alert_subject_active", "Whether a scheduler alert subject has an active episode"},
			{"tako_scheduler_alert_subject_last_success_timestamp_seconds", "Last successful attempt time for a scheduler alert subject"},
		} {
			fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		}
	}
	reachableValue := 0
	if reachable {
		reachableValue = 1
	}
	fmt.Fprintf(out, "tako_scheduler_alert_owner_reachable%s %d\n\n", labels, reachableValue)
	if !reachable || status == nil {
		return
	}
	configured := 0
	if status.Configured {
		configured = 1
	}
	fmt.Fprintf(out, "tako_scheduler_alert_policy_loaded%s %d\n\n", labels, configured)
	fmt.Fprintf(out, "tako_scheduler_alert_active_episodes%s %d\n\n", labels, status.ActiveEpisodes)
	fmt.Fprintf(out, "tako_scheduler_alert_pending_deliveries%s %d\n\n", labels, status.PendingDelivery)
	fmt.Fprintf(out, "tako_scheduler_alert_exhausted_deliveries%s %d\n\n", labels, status.Exhausted)
	if !status.EvaluatedAt.IsZero() {
		fmt.Fprintf(out, "tako_scheduler_alert_last_evaluation_timestamp_seconds%s %d\n\n", labels, status.EvaluatedAt.Unix())
	}
	for _, subject := range status.Subjects {
		subjectLabels := fmt.Sprintf(`{server=%s,host=%s,project=%s,environment=%s,kind=%s,service=%s}`, strconv.Quote(serverName), strconv.Quote(serverHost), strconv.Quote(project), strconv.Quote(environment), strconv.Quote(subject.Kind), strconv.Quote(subject.Service))
		active := 0
		if subject.Active {
			active = 1
		}
		fmt.Fprintf(out, "tako_scheduler_alert_subject_active%s %d\n", subjectLabels, active)
		if !subject.LastSuccessAt.IsZero() {
			fmt.Fprintf(out, "tako_scheduler_alert_subject_last_success_timestamp_seconds%s %d\n", subjectLabels, subject.LastSuccessAt.Unix())
		}
	}
}

// exportMonitorMetrics exports node-side health monitoring state
// (monitoring.health-v1). Labels carry only public names and closed
// vocabulary values; targets and credentials are never exported.
func exportMonitorMetrics(out io.Writer, node, project, environment string, status *takod.SourceAlertStatus, includeMetadata bool) {
	if status == nil {
		return
	}
	if includeMetadata {
		for _, metric := range []struct{ name, help string }{
			{"tako_monitor_subject_active", "Whether a monitored subject is alerting (1) or not (0)"},
			{"tako_monitor_subject_last_observed_timestamp_seconds", "Last observation time of a monitored subject"},
			{"tako_monitor_pending_deliveries", "Pending node-side monitoring alert deliveries"},
			{"tako_monitor_exhausted_deliveries", "Exhausted node-side monitoring alert deliveries"},
			{"tako_host_metrics_stale", "Whether the node's host metrics sample is stale"},
		} {
			fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		}
	}
	quote := strconv.Quote
	nodeLabels := fmt.Sprintf(`{project=%s,environment=%s,node=%s}`, quote(project), quote(environment), quote(node))
	stale := 0
	for _, subject := range status.Subjects {
		labels := fmt.Sprintf(`{project=%s,environment=%s,node=%s,kind=%s,subject=%s,severity=%s}`, quote(project), quote(environment), quote(node), quote(subject.Kind), quote(subject.Subject), quote(subject.Severity))
		active := 0
		if subject.State == takod.SubjectStateAlerting || subject.State == takod.SubjectStateFlapping {
			active = 1
		}
		fmt.Fprintf(out, "tako_monitor_subject_active%s %d\n", labels, active)
		if !subject.LastObservedAt.IsZero() {
			fmt.Fprintf(out, "tako_monitor_subject_last_observed_timestamp_seconds%s %d\n", labels, subject.LastObservedAt.Unix())
		}
		if subject.Kind == takod.AlertKindHost {
			for _, reason := range subject.Reasons {
				if reason == "metrics_stale" {
					stale = 1
				}
			}
		}
	}
	fmt.Fprintf(out, "tako_monitor_pending_deliveries%s %d\n", nodeLabels, status.PendingDeliveries)
	fmt.Fprintf(out, "tako_monitor_exhausted_deliveries%s %d\n", nodeLabels, status.ExhaustedDeliveries)
	fmt.Fprintf(out, "tako_host_metrics_stale{node=%s} %d\n\n", quote(node), stale)
}

func exportContainerMetrics(serverName, serverHost string, stats []takod.ContainerStat) {
	for _, stat := range stats {
		containerName := stat.Name
		cpuPercent := strings.TrimSuffix(stat.CPUPercent, "%")
		memPercent := strings.TrimSuffix(stat.MemPercent, "%")

		// Parse memory usage (format: "100MiB / 2GiB")
		memUsage := stat.MemUsage
		memParts := strings.Split(memUsage, " / ")
		var memUsedBytes, memTotalBytes int64
		if len(memParts) == 2 {
			memUsedBytes = parseMemoryString(memParts[0])
			memTotalBytes = parseMemoryString(memParts[1])
		}

		labels := fmt.Sprintf(`{server="%s",host="%s",container="%s"}`, serverName, serverHost, containerName)

		// Container CPU
		fmt.Printf("# HELP tako_container_cpu_usage_percent Container CPU usage percentage\n")
		fmt.Printf("# TYPE tako_container_cpu_usage_percent gauge\n")
		fmt.Printf("tako_container_cpu_usage_percent%s %s\n", labels, cpuPercent)
		fmt.Println()

		// Container Memory
		if memUsedBytes > 0 {
			fmt.Printf("# HELP tako_container_memory_used_bytes Container memory usage in bytes\n")
			fmt.Printf("# TYPE tako_container_memory_used_bytes gauge\n")
			fmt.Printf("tako_container_memory_used_bytes%s %d\n", labels, memUsedBytes)
			fmt.Println()

			fmt.Printf("# HELP tako_container_memory_limit_bytes Container memory limit in bytes\n")
			fmt.Printf("# TYPE tako_container_memory_limit_bytes gauge\n")
			fmt.Printf("tako_container_memory_limit_bytes%s %d\n", labels, memTotalBytes)
			fmt.Println()
		}

		fmt.Printf("# HELP tako_container_memory_usage_percent Container memory usage percentage\n")
		fmt.Printf("# TYPE tako_container_memory_usage_percent gauge\n")
		fmt.Printf("tako_container_memory_usage_percent%s %s\n", labels, memPercent)
		fmt.Println()
	}
}

func parseMemoryString(s string) int64 {
	s = strings.TrimSpace(s)
	var value float64
	var unit string

	fmt.Sscanf(s, "%f%s", &value, &unit)

	multiplier := int64(1)
	switch strings.ToUpper(unit) {
	case "KIB", "KB":
		multiplier = 1024
	case "MIB", "MB":
		multiplier = 1024 * 1024
	case "GIB", "GB":
		multiplier = 1024 * 1024 * 1024
	case "TIB", "TB":
		multiplier = 1024 * 1024 * 1024 * 1024
	}

	return int64(value * float64(multiplier))
}
