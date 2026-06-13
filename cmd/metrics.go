package cmd

import (
	"encoding/json"
	"fmt"
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
	metricsLive   bool
	metricsOnce   bool
	metricsServer string
)

// MetricsData represents the JSON structure returned by the monitoring agent
type MetricsData struct {
	Timestamp     string         `json:"timestamp"`
	CPUPercent    string         `json:"cpu_percent"`
	Memory        MemoryMetrics  `json:"memory"`
	Disk          DiskMetrics    `json:"disk"`
	Network       NetworkMetrics `json:"network"`
	DiskIO        DiskIOMetrics  `json:"disk_io"`
	UptimeSeconds int            `json:"uptime_seconds"`
	LoadAverage   LoadAvgMetrics `json:"load_average"`
}

type MemoryMetrics struct {
	TotalMB     int    `json:"total_mb"`
	UsedMB      int    `json:"used_mb"`
	AvailableMB int    `json:"available_mb"`
	Percent     string `json:"percent"`
	SwapTotalMB int    `json:"swap_total_mb"`
	SwapUsedMB  int    `json:"swap_used_mb"`
}

type DiskMetrics struct {
	TotalMB     int    `json:"total_mb"`
	UsedMB      int    `json:"used_mb"`
	AvailableMB int    `json:"available_mb"`
	Percent     string `json:"percent"`
}

type LoadAvgMetrics struct {
	OneMin     string `json:"1min"`
	FiveMin    string `json:"5min"`
	FifteenMin string `json:"15min"`
}

type NetworkMetrics struct {
	RxMB    string `json:"rx_mb"`
	TxMB    string `json:"tx_mb"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

type DiskIOMetrics struct {
	ReadMB       string `json:"read_mb"`
	WriteMB      string `json:"write_mb"`
	ReadSectors  int64  `json:"read_sectors"`
	WriteSectors int64  `json:"write_sectors"`
}

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "View system metrics from servers",
	Long: `Display real-time system metrics (CPU, RAM, Disk) collected from deployed servers.

The monitoring agent runs continuously on each server, collecting metrics every 60 seconds.
This command fetches and displays the latest metrics.`,
	RunE: runMetrics,
}

func init() {
	rootCmd.AddCommand(metricsCmd)
	metricsCmd.Flags().BoolVar(&metricsLive, "live", false, "Continuous live updates")
	metricsCmd.Flags().BoolVar(&metricsOnce, "once", false, "Collect metrics once immediately")
	metricsCmd.Flags().StringVarP(&metricsServer, "server", "s", "", "Specific server to monitor")
}

func runMetrics(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)

	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	// Filter to specific server if requested
	if metricsServer != "" {
		found := false
		for _, s := range servers {
			if s == metricsServer {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("server %s not found in environment %s", metricsServer, envName)
		}
		servers = []string{metricsServer}
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	if metricsLive {
		return showLiveMetrics(cfg, servers, sshPool)
	}

	return showMetricsOnce(cfg, servers, sshPool, metricsOnce)
}

func showMetricsOnce(cfg *config.Config, servers []string, sshPool *ssh.Pool, collectNew bool) error {
	fmt.Printf("\n=== System Metrics ===\n")
	fmt.Printf("Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, result := range collectMetricsOnce(cfg, servers, sshPool, collectNew) {
		if result.connectErr != nil {
			fmt.Printf("❌ %s (%s): Failed to connect - %v\n\n", result.serverName, result.host, result.connectErr)
			continue
		}
		if result.metricsErr != nil {
			fmt.Printf("❌ %s (%s): Monitoring agent not installed or not running\n", result.serverName, result.host)
			fmt.Printf("   Run: tako setup --server %s\n\n", result.serverName)
			continue
		}
		displayServerMetrics(result.serverName, result.host, result.metrics)
	}

	return nil
}

type metricsNodeResult struct {
	index      int
	serverName string
	host       string
	metrics    *MetricsData
	connectErr error
	metricsErr error
}

type metricsReadFunc func(serverName string, server config.ServerConfig) (*MetricsData, error)

func collectMetricsOnce(cfg *config.Config, servers []string, sshPool *ssh.Pool, collectNew bool) []metricsNodeResult {
	return collectMetricsOnceWith(cfg.Servers, servers, func(serverName string, server config.ServerConfig) (*MetricsData, error) {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		metrics, err := readMetricsViaTakod(client, cfg, collectNew)
		if err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
		return metrics, nil
	})
}

func collectMetricsOnceWith(configuredServers map[string]config.ServerConfig, serverNames []string, read metricsReadFunc) []metricsNodeResult {
	resultCh := make(chan metricsNodeResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := configuredServers[serverName]
		if !ok {
			resultCh <- metricsNodeResult{
				index:      index,
				serverName: serverName,
				connectErr: fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			metrics, err := read(serverName, server)
			result := metricsNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
				metrics:    metrics,
			}
			if err != nil {
				if strings.HasPrefix(err.Error(), "connect: ") {
					result.connectErr = fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "connect: "))
				} else {
					result.metricsErr = err
				}
			}
			resultCh <- result
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]metricsNodeResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func showLiveMetrics(cfg *config.Config, servers []string, sshPool *ssh.Pool) error {
	fmt.Printf("=== Live System Metrics (Press Ctrl+C to exit) ===\n\n")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		// Clear screen (ANSI escape code)
		fmt.Print("\033[H\033[2J")

		fmt.Printf("=== Live Metrics - %s ===\n\n", time.Now().Format("2006-01-02 15:04:05"))

		for _, serverName := range servers {
			server := cfg.Servers[serverName]

			client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
			if err != nil {
				fmt.Printf("❌ %s: Connection failed\n\n", serverName)
				continue
			}

			metrics, err := readMetricsViaTakod(client, cfg, false)
			if err != nil {
				fmt.Printf("❌ %s: Monitoring not available\n\n", serverName)
				continue
			}

			displayServerMetrics(serverName, server.Host, metrics)
		}

		<-ticker.C
	}
}

func readMetricsViaTakod(client *ssh.Client, cfg *config.Config, collect bool) (*MetricsData, error) {
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.MetricsEndpoint(collect), nil)
	if err != nil {
		return nil, err
	}

	var response takod.MetricsResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	var metrics MetricsData
	if err := json.Unmarshal(response.Metrics, &metrics); err != nil {
		return nil, fmt.Errorf("failed to parse metrics: %w", err)
	}
	return &metrics, nil
}

func displayServerMetrics(serverName, serverHost string, metrics *MetricsData) {
	fmt.Printf("📊 %s (%s)\n", serverName, serverHost)
	fmt.Printf("─────────────────────────────────────────────────\n")

	// CPU
	fmt.Printf("CPU:    %s%%", metrics.CPUPercent)
	cpuBar := createProgressBar(metrics.CPUPercent, 20)
	fmt.Printf(" %s\n", cpuBar)

	// Memory
	fmt.Printf("Memory: %s%% (%d MB / %d MB)", metrics.Memory.Percent, metrics.Memory.UsedMB, metrics.Memory.TotalMB)
	memBar := createProgressBar(metrics.Memory.Percent, 20)
	fmt.Printf(" %s\n", memBar)

	// Swap (if used)
	if metrics.Memory.SwapUsedMB > 0 {
		swapPct := fmt.Sprintf("%.1f", float64(metrics.Memory.SwapUsedMB)/float64(metrics.Memory.SwapTotalMB)*100)
		fmt.Printf("Swap:   %s%% (%d MB / %d MB)\n", swapPct, metrics.Memory.SwapUsedMB, metrics.Memory.SwapTotalMB)
	}

	// Disk
	fmt.Printf("Disk:   %s%% (%d MB / %d MB)", metrics.Disk.Percent, metrics.Disk.UsedMB, metrics.Disk.TotalMB)
	diskBar := createProgressBar(metrics.Disk.Percent, 20)
	fmt.Printf(" %s\n", diskBar)

	// Network I/O (if available)
	if metrics.Network.RxMB != "" && metrics.Network.TxMB != "" {
		fmt.Printf("Net I/O: ↓ %s MB  ↑ %s MB (since last check)\n", metrics.Network.RxMB, metrics.Network.TxMB)
	}

	// Disk I/O (if available)
	if metrics.DiskIO.ReadMB != "" && metrics.DiskIO.WriteMB != "" {
		fmt.Printf("Disk I/O: ⬇ %s MB  ⬆ %s MB (since last check)\n", metrics.DiskIO.ReadMB, metrics.DiskIO.WriteMB)
	}

	// Load Average
	fmt.Printf("Load:   %s (1m) / %s (5m) / %s (15m)\n",
		metrics.LoadAverage.OneMin, metrics.LoadAverage.FiveMin, metrics.LoadAverage.FifteenMin)

	// Uptime
	uptime := formatUptime(metrics.UptimeSeconds)
	fmt.Printf("Uptime: %s\n", uptime)

	// Last updated
	fmt.Printf("Updated: %s\n", metrics.Timestamp)
	fmt.Printf("\n")
}

func createProgressBar(percentStr string, width int) string {
	// Parse percentage
	var pct float64
	fmt.Sscanf(percentStr, "%f", &pct)

	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}

	bar := "["
	for i := 0; i < width; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	bar += "]"

	return bar
}

func formatUptime(seconds int) string {
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	} else if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	} else {
		return fmt.Sprintf("%dm", minutes)
	}
}
