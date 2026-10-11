package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	metricsLive   bool
	metricsOnce   bool
	metricsServer string
	metricsSince  string
	metricsStep   string
)

// MetricsData is the takod /v1/metrics document. The legacy fields keep the
// bash agent's schema; Source, IntervalSeconds, CPUCount, DockerDisk and the
// rate and total fields are filled by takod's native collector.
type MetricsData struct {
	Timestamp       string         `json:"timestamp"`
	CPUPercent      string         `json:"cpu_percent"`
	Memory          MemoryMetrics  `json:"memory"`
	Disk            DiskMetrics    `json:"disk"`
	Network         NetworkMetrics `json:"network"`
	DiskIO          DiskIOMetrics  `json:"disk_io"`
	UptimeSeconds   int            `json:"uptime_seconds"`
	LoadAverage     LoadAvgMetrics `json:"load_average"`
	Source          string         `json:"source,omitempty"`
	IntervalSeconds int64          `json:"interval_seconds,omitempty"`
	CPUCount        int            `json:"cpu_count,omitempty"`
	DockerDisk      *DiskMetrics   `json:"docker_disk,omitempty"`
	Warnings        []string       `json:"warnings,omitempty"`

	// Status is takod's node status (node.status-v1), carried next to the
	// metrics document rather than inside it.
	Status *takod.NodeStatusResponse `json:"-"`
	// nativeDocument is the same document decoded with takod's native type
	// (nil for legacy-agent documents); it feeds the Prometheus node series.
	nativeDocument *takod.NodeMetricsDocument
}

// native reports whether the document came from takod's native collector.
func (m *MetricsData) native() bool { return m != nil && m.Source == takod.NodeMetricsSource }

type MemoryMetrics struct {
	TotalMB     int    `json:"total_mb"`
	UsedMB      int    `json:"used_mb"`
	AvailableMB int    `json:"available_mb"`
	Percent     string `json:"percent"`
	SwapTotalMB int    `json:"swap_total_mb"`
	SwapUsedMB  int    `json:"swap_used_mb"`
}

type DiskMetrics struct {
	Path          string `json:"path,omitempty"`
	TotalMB       int    `json:"total_mb"`
	UsedMB        int    `json:"used_mb"`
	AvailableMB   int    `json:"available_mb"`
	Percent       string `json:"percent"`
	InodesPercent string `json:"inodes_percent,omitempty"`
}

type LoadAvgMetrics struct {
	OneMin     string `json:"1min"`
	FiveMin    string `json:"5min"`
	FifteenMin string `json:"15min"`
}

type NetworkMetrics struct {
	RxMB          string  `json:"rx_mb"`
	TxMB          string  `json:"tx_mb"`
	RxBytes       int64   `json:"rx_bytes"`
	TxBytes       int64   `json:"tx_bytes"`
	RxBytesPerSec float64 `json:"rx_bytes_per_sec,omitempty"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec,omitempty"`
	RxBytesTotal  int64   `json:"rx_bytes_total,omitempty"`
	TxBytesTotal  int64   `json:"tx_bytes_total,omitempty"`
}

type DiskIOMetrics struct {
	ReadMB            string  `json:"read_mb"`
	WriteMB           string  `json:"write_mb"`
	ReadSectors       int64   `json:"read_sectors"`
	WriteSectors      int64   `json:"write_sectors"`
	ReadBytesPerSec   float64 `json:"read_bytes_per_sec,omitempty"`
	WriteBytesPerSec  float64 `json:"write_bytes_per_sec,omitempty"`
	ReadBytesTotal    int64   `json:"read_bytes_total,omitempty"`
	WrittenBytesTotal int64   `json:"written_bytes_total,omitempty"`
}

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "View system metrics from servers",
	Long: `Display system metrics (CPU, memory, disk, load, network and disk I/O)
collected by takod on each server, plus OS update and reboot status.

takod samples host metrics natively every 60 seconds and keeps a bounded
on-node history (7 days by default). Without --since this command shows the
latest sample; --since shows min/avg/max/last and a sparkline per metric for
that window (for example --since 24h or --since 2026-10-11T00:00:00Z), with
--step choosing the bucket size.`,
	RunE: runMetrics,
}

func init() {
	rootCmd.AddCommand(metricsCmd)
	metricsCmd.Flags().BoolVar(&metricsLive, "live", false, "Continuous live updates")
	metricsCmd.Flags().BoolVar(&metricsOnce, "once", false, "Collect metrics once immediately")
	metricsCmd.Flags().StringVarP(&metricsServer, "server", "s", "", "Specific server to monitor")
	metricsCmd.Flags().StringVar(&metricsSince, "since", "", "Show history for this window: a duration (24h, 7d) or an RFC3339 timestamp")
	metricsCmd.Flags().StringVar(&metricsStep, "step", "", "History bucket size with --since (e.g. 5m; default picks at most 360 buckets)")
}

func runMetrics(cmd *cobra.Command, args []string) error {
	historyRequest, err := parseMetricsHistoryFlags(metricsSince, metricsStep, metricsLive, metricsOnce)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
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
	factory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer factory.CloseIdleConnections()

	if historyRequest != nil {
		return showMetricsHistory(cfg, envName, servers, factory, *historyRequest)
	}
	if metricsLive {
		if machineOutputEnabled() {
			return &engine.InvalidRequestError{Err: fmt.Errorf("--live is interactive-only; run single-shot metrics reads in machine output modes")}
		}
		return showLiveMetrics(cfg, servers, factory)
	}

	return showMetricsOnce(cfg, envName, servers, factory, metricsOnce)
}

func showMetricsOnce(cfg *config.Config, envName string, servers []string, factory *nodeclient.Factory, collectNew bool) error {
	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}
	fmt.Fprintf(out, "\n=== System Metrics ===\n")
	fmt.Fprintf(out, "Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	result := engine.MetricsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindMetricsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      metricsServer,
		CollectedAt: time.Now().UTC(),
		Nodes:       []engine.MetricsNodeSample{},
	}
	failures := 0
	for _, nodeResult := range collectMetricsOnce(cfg, servers, factory, collectNew) {
		sample := engine.MetricsNodeSample{Server: nodeResult.serverName, Host: nodeResult.host}
		switch {
		case nodeResult.connectErr != nil:
			fmt.Fprintf(out, "❌ %s (%s): Failed to connect - %v\n\n", nodeResult.serverName, nodeResult.host, nodeResult.connectErr)
			sample.Error = nodeResult.connectErr.Error()
			failures++
		case nodeResult.metricsErr != nil:
			fmt.Fprintf(out, "❌ %s (%s): Metrics unavailable - %v\n", nodeResult.serverName, nodeResult.host, nodeResult.metricsErr)
			fmt.Fprintf(out, "   takod collects host metrics natively; run 'tako upgrade servers' if this node runs an older takod\n\n")
			sample.Error = nodeResult.metricsErr.Error()
			failures++
		default:
			displayServerMetrics(out, nodeResult.serverName, nodeResult.host, nodeResult.metrics)
			if payload, err := json.Marshal(nodeResult.metrics); err == nil {
				sample.Metrics = payload
			}
			if nodeResult.metrics.Status != nil {
				if payload, err := json.Marshal(nodeResult.metrics.Status); err == nil {
					sample.Status = payload
				}
			}
		}
		result.Nodes = append(result.Nodes, sample)
	}

	var err error
	switch {
	case failures == len(result.Nodes) && failures > 0:
		err = fmt.Errorf("failed to read metrics from all %d node(s)", failures)
		result.Error = err.Error()
	case failures > 0:
		err = &engine.AttentionError{Err: fmt.Errorf("failed to read metrics from %d of %d node(s)", failures, len(result.Nodes))}
		result.Error = err.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
		err = emitErr
	}
	return err
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

func collectMetricsOnce(cfg *config.Config, servers []string, factory *nodeclient.Factory, collectNew bool) []metricsNodeResult {
	return collectMetricsOnceWith(cfg.Servers, servers, func(serverName string, server config.ServerConfig) (*MetricsData, error) {
		client, _, err := factory.Client(context.Background(), serverName)
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

func showLiveMetrics(cfg *config.Config, servers []string, factory *nodeclient.Factory) error {
	fmt.Printf("=== Live System Metrics (Press Ctrl+C to exit) ===\n\n")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		// Clear screen (ANSI escape code)
		fmt.Print("\033[H\033[2J")

		fmt.Printf("=== Live Metrics - %s ===\n\n", time.Now().Format("2006-01-02 15:04:05"))

		for _, serverName := range servers {
			server, err := serverConfigByName(cfg, serverName)
			if err != nil {
				return err
			}

			client, _, err := factory.Client(context.Background(), serverName)
			if err != nil {
				fmt.Printf("❌ %s: Connection failed\n\n", serverName)
				continue
			}

			metrics, err := readMetricsViaTakod(client, cfg, false)
			if err != nil {
				fmt.Printf("❌ %s: Metrics unavailable\n\n", serverName)
				continue
			}

			displayServerMetrics(os.Stdout, serverName, server.Host, metrics)
		}

		<-ticker.C
	}
}

func readMetricsViaTakod(client any, cfg *config.Config, collect bool) (*MetricsData, error) {
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.MetricsEndpoint(collect), nil)
	if err != nil {
		return nil, err
	}

	var response takod.MetricsResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return decodeMetricsResponse(&response)
}

func decodeMetricsResponse(response *takod.MetricsResponse) (*MetricsData, error) {
	var metrics MetricsData
	if err := json.Unmarshal(response.Metrics, &metrics); err != nil {
		return nil, fmt.Errorf("failed to parse metrics: %w", err)
	}
	metrics.Status = response.Status
	if metrics.native() {
		var document takod.NodeMetricsDocument
		if err := json.Unmarshal(response.Metrics, &document); err == nil {
			metrics.nativeDocument = &document
		}
	}
	return &metrics, nil
}

func readAlertStatusViaTakod(client any, cfg *config.Config, environment string) (*takod.AlertStatusResponse, error) {
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.AlertsEndpoint(cfg.Project.Name, environment), nil)
	if err != nil {
		return nil, err
	}
	var response takod.AlertStatusResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func displayServerMetrics(out io.Writer, serverName, serverHost string, metrics *MetricsData) {
	fmt.Fprintf(out, "📊 %s (%s)\n", serverName, serverHost)
	fmt.Fprintf(out, "─────────────────────────────────────────────────\n")

	// CPU
	fmt.Fprintf(out, "CPU:    %s%%", metrics.CPUPercent)
	cpuBar := createProgressBar(metrics.CPUPercent, 20)
	fmt.Fprintf(out, " %s\n", cpuBar)

	// Memory
	fmt.Fprintf(out, "Memory: %s%% (%d MB / %d MB)", metrics.Memory.Percent, metrics.Memory.UsedMB, metrics.Memory.TotalMB)
	memBar := createProgressBar(metrics.Memory.Percent, 20)
	fmt.Fprintf(out, " %s\n", memBar)

	// Swap (if used)
	if metrics.Memory.SwapUsedMB > 0 {
		swapPct := fmt.Sprintf("%.1f", float64(metrics.Memory.SwapUsedMB)/float64(metrics.Memory.SwapTotalMB)*100)
		fmt.Fprintf(out, "Swap:   %s%% (%d MB / %d MB)\n", swapPct, metrics.Memory.SwapUsedMB, metrics.Memory.SwapTotalMB)
	}

	// Disk
	fmt.Fprintf(out, "Disk:   %s%% (%d MB / %d MB)", metrics.Disk.Percent, metrics.Disk.UsedMB, metrics.Disk.TotalMB)
	diskBar := createProgressBar(metrics.Disk.Percent, 20)
	fmt.Fprintf(out, " %s\n", diskBar)

	if docker := metrics.DockerDisk; docker != nil {
		fmt.Fprintf(out, "Docker: %s%% (%d MB / %d MB)", docker.Percent, docker.UsedMB, docker.TotalMB)
		fmt.Fprintf(out, " %s\n", createProgressBar(docker.Percent, 20))
	}

	if metrics.native() {
		fmt.Fprintf(out, "Net I/O: ↓ %s  ↑ %s\n", formatByteRate(metrics.Network.RxBytesPerSec), formatByteRate(metrics.Network.TxBytesPerSec))
		fmt.Fprintf(out, "Disk I/O: read %s  write %s\n", formatByteRate(metrics.DiskIO.ReadBytesPerSec), formatByteRate(metrics.DiskIO.WriteBytesPerSec))
	} else {
		// Legacy agent documents carry per-interval deltas only.
		if metrics.Network.RxMB != "" && metrics.Network.TxMB != "" {
			fmt.Fprintf(out, "Net I/O: ↓ %s MB  ↑ %s MB (since last check)\n", metrics.Network.RxMB, metrics.Network.TxMB)
		}
		if metrics.DiskIO.ReadMB != "" && metrics.DiskIO.WriteMB != "" {
			fmt.Fprintf(out, "Disk I/O: ⬇ %s MB  ⬆ %s MB (since last check)\n", metrics.DiskIO.ReadMB, metrics.DiskIO.WriteMB)
		}
	}

	// Load Average
	fmt.Fprintf(out, "Load:   %s (1m) / %s (5m) / %s (15m)\n",
		metrics.LoadAverage.OneMin, metrics.LoadAverage.FiveMin, metrics.LoadAverage.FifteenMin)

	// Uptime
	uptime := formatUptime(metrics.UptimeSeconds)
	fmt.Fprintf(out, "Uptime: %s\n", uptime)

	if line := formatOSStatusLine(metrics.Status); line != "" {
		fmt.Fprintf(out, "%s\n", line)
	}

	// Last updated
	fmt.Fprintf(out, "Updated: %s\n", metrics.Timestamp)
	fmt.Fprintf(out, "\n")
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
