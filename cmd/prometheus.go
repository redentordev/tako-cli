package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
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
		return fmt.Errorf("failed to load config: %w", err)
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

	// Collect and export metrics
	for _, serverName := range servers {
		server := cfg.Servers[serverName]

		// Get or create SSH client
		client, err := sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
		if err != nil {
			continue // Skip unavailable servers
		}

		// Collect new metrics
		_, _ = client.Execute("/usr/local/bin/tako-monitor.sh once")

		// Fetch system metrics
		output, err := client.Execute("cat /var/lib/tako/metrics/current.json 2>/dev/null")
		if err != nil {
			continue
		}

		var metrics MetricsData
		if err := json.Unmarshal([]byte(output), &metrics); err != nil {
			continue
		}

		// Export system metrics
		exportSystemMetrics(serverName, server.Host, &metrics)

		// Fetch container stats
		statsCmd := fmt.Sprintf("docker ps --format '{{.Names}}' | grep '^%s_%s_' | xargs -r docker stats --no-stream --format '{{json .}}'", cfg.Project.Name, envName)
		statsOutput, err := client.Execute(statsCmd)
		if err == nil && strings.TrimSpace(statsOutput) != "" {
			exportContainerMetrics(serverName, server.Host, statsOutput)
		}
	}

	return nil
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

func exportContainerMetrics(serverName, serverHost, statsOutput string) {
	lines := strings.Split(strings.TrimSpace(statsOutput), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}

		var rawStat map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rawStat); err != nil {
			continue
		}

		containerName := getString(rawStat, "Name")
		cpuPercent := strings.TrimSuffix(getString(rawStat, "CPUPerc"), "%")
		memPercent := strings.TrimSuffix(getString(rawStat, "MemPerc"), "%")

		// Parse memory usage (format: "100MiB / 2GiB")
		memUsage := getString(rawStat, "MemUsage")
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
