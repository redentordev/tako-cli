package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	statsLive     bool
	statsFollow   bool
	statsInterval time.Duration
	statsService  string
	statsAll      bool
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Display container resource usage statistics",
	Long: `Display real-time resource usage statistics for containers.

Shows CPU, memory, network I/O, and disk I/O for each running container.
By default, shows stats for the current project's containers.`,
	RunE: runStats,
}

func init() {
	rootCmd.AddCommand(statsCmd)
	statsCmd.Flags().BoolVar(&statsLive, "live", false, "Continuous live updates")
	statsCmd.Flags().BoolVar(&statsFollow, "follow", false, "Stream stats.sample events (requires --events ndjson)")
	statsCmd.Flags().DurationVar(&statsInterval, "interval", 5*time.Second, "Sample interval for --follow")
	statsCmd.Flags().StringVarP(&statsService, "service", "s", "", "Filter by service name")
	statsCmd.Flags().BoolVar(&statsAll, "all", false, "Show all containers (not just current project)")
}

func runStats(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	if statsService != "" {
		services, err := cfg.GetServices(envName)
		if err != nil {
			return fmt.Errorf("failed to get services: %w", err)
		}
		if _, ok := services[statsService]; !ok {
			return fmt.Errorf("service %s not found in environment %s", statsService, envName)
		}
	}

	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer factory.CloseIdleConnections()

	if statsLive {
		if machineOutputEnabled() {
			return &engine.InvalidRequestError{Err: fmt.Errorf("--live is interactive-only; use --follow --events ndjson to stream stats in machine output modes")}
		}
		return showLiveStats(cfg, servers, factory, envName)
	}

	if statsFollow {
		if eventsFormatFlag != eventsFormatNDJSON {
			return &engine.InvalidRequestError{Err: fmt.Errorf("--follow streams stats.sample events and requires --events ndjson")}
		}
		return followStats(cmd, cfg, servers, factory, envName)
	}

	return showStatsOnce(cfg, servers, factory, envName)
}

// followStats streams one stats.sample event per node per interval until the
// command context is cancelled (Ctrl+C / SIGTERM), mirroring `logs --follow`.
func followStats(cmd *cobra.Command, cfg *config.Config, servers []string, factory *nodeclient.Factory, envName string) error {
	interval := statsInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		for _, nodeResult := range collectStatsOnce(cfg, servers, factory, envName) {
			sample := statsNodeSampleDocument(nodeResult)
			data := map[string]any{
				"server":     sample.Server,
				"host":       sample.Host,
				"containers": sample.Containers,
			}
			if sample.Error != "" {
				data["error"] = sample.Error
			}
			cliEngine().EventStream().Emit(events.Event{
				Type:  events.TypeStatsSample,
				Phase: events.PhaseLogs,
				Level: events.LevelInfo,
				Data:  data,
			})
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

func statsNodeSampleDocument(nodeResult statsNodeResult) engine.StatsNodeSample {
	sample := engine.StatsNodeSample{Server: nodeResult.serverName, Host: nodeResult.host}
	switch {
	case nodeResult.connectErr != nil:
		sample.Error = nodeResult.connectErr.Error()
	case nodeResult.statsErr != nil:
		sample.Error = nodeResult.statsErr.Error()
	default:
		sample.Containers = nodeResult.stats
	}
	return sample
}

func showStatsOnce(cfg *config.Config, servers []string, factory *nodeclient.Factory, envName string) error {
	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}
	fmt.Fprintf(out, "\n=== Container Statistics ===\n")
	fmt.Fprintf(out, "Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	result := engine.StatsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindStatsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     statsService,
		All:         statsAll,
		CollectedAt: time.Now().UTC(),
		Nodes:       []engine.StatsNodeSample{},
	}
	failures := 0
	for _, nodeResult := range collectStatsOnce(cfg, servers, factory, envName) {
		if nodeResult.connectErr != nil {
			fmt.Fprintf(out, "❌ %s (%s): Failed to connect - %v\n\n", nodeResult.serverName, nodeResult.host, nodeResult.connectErr)
			failures++
		} else if nodeResult.statsErr != nil {
			fmt.Fprintf(out, "❌ %s (%s): Failed to get stats - %v\n\n", nodeResult.serverName, nodeResult.host, nodeResult.statsErr)
			failures++
		} else {
			displayContainerStats(out, nodeResult.serverName, nodeResult.host, nodeResult.stats)
		}
		result.Nodes = append(result.Nodes, statsNodeSampleDocument(nodeResult))
	}

	var err error
	switch {
	case failures == len(result.Nodes) && failures > 0:
		err = fmt.Errorf("failed to read stats from all %d node(s)", failures)
		result.Error = err.Error()
	case failures > 0:
		err = &engine.AttentionError{Err: fmt.Errorf("failed to read stats from %d of %d node(s)", failures, len(result.Nodes))}
		result.Error = err.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
		err = emitErr
	}
	return err
}

type statsNodeResult struct {
	index      int
	serverName string
	host       string
	stats      []takod.ContainerStat
	connectErr error
	statsErr   error
}

type statsReadFunc func(serverName string, server config.ServerConfig) (*takod.StatsResponse, error)

func collectStatsOnce(cfg *config.Config, servers []string, factory *nodeclient.Factory, envName string) []statsNodeResult {
	return collectStatsOnceWith(cfg.Servers, servers, func(serverName string, server config.ServerConfig) (*takod.StatsResponse, error) {
		client, _, err := factory.Client(context.Background(), serverName)
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		response, err := readStatsViaTakod(client, cfg, envName)
		if err != nil {
			return nil, fmt.Errorf("stats: %w", err)
		}
		return response, nil
	})
}

func collectStatsOnceWith(configuredServers map[string]config.ServerConfig, serverNames []string, read statsReadFunc) []statsNodeResult {
	resultCh := make(chan statsNodeResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := configuredServers[serverName]
		if !ok {
			resultCh <- statsNodeResult{
				index:      index,
				serverName: serverName,
				connectErr: fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			response, err := read(serverName, server)
			result := statsNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
			}
			if response != nil {
				result.stats = response.Stats
			}
			if err != nil {
				if strings.HasPrefix(err.Error(), "connect: ") {
					result.connectErr = fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "connect: "))
				} else {
					result.statsErr = err
				}
			}
			resultCh <- result
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]statsNodeResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func showLiveStats(cfg *config.Config, servers []string, factory *nodeclient.Factory, envName string) error {
	fmt.Printf("=== Live Container Stats (Press Ctrl+C to exit) ===\n\n")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		// Clear screen (ANSI escape code)
		fmt.Print("\033[H\033[2J")

		fmt.Printf("=== Live Container Stats - %s ===\n\n", time.Now().Format("2006-01-02 15:04:05"))

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

			response, err := readStatsViaTakod(client, cfg, envName)
			if err != nil {
				fmt.Printf("❌ %s: Stats unavailable\n\n", serverName)
				continue
			}

			displayContainerStats(os.Stdout, serverName, server.Host, response.Stats)
		}

		<-ticker.C
	}
}

func readStatsViaTakod(client any, cfg *config.Config, envName string) (*takod.StatsResponse, error) {
	return readStatsViaTakodWithOptions(client, cfg, envName, statsService, statsAll)
}

func readStatsViaTakodWithOptions(client any, cfg *config.Config, envName string, service string, all bool) (*takod.StatsResponse, error) {
	endpoint := takodclient.StatsEndpoint(cfg.Project.Name, envName, service, all)
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	var response takod.StatsResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func displayContainerStats(out io.Writer, serverName, serverHost string, stats []takod.ContainerStat) {
	if len(stats) == 0 {
		fmt.Fprintf(out, "📊 %s (%s)\n", serverName, serverHost)
		fmt.Fprintf(out, "No containers found\n\n")
		return
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	fmt.Fprintf(out, "📊 %s (%s)\n", serverName, serverHost)
	fmt.Fprintf(out, "─────────────────────────────────────────────────────────────────────────────────────\n")
	fmt.Fprintf(out, "%-40s %10s %20s %10s %20s %15s\n", "CONTAINER", "CPU %", "MEMORY", "MEM %", "NET I/O", "BLOCK I/O")
	fmt.Fprintf(out, "─────────────────────────────────────────────────────────────────────────────────────\n")

	for _, stat := range stats {
		// Truncate container name if too long
		name := stat.Name
		if len(name) > 38 {
			name = name[:35] + "..."
		}

		fmt.Fprintf(out, "%-40s %10s %20s %10s %20s %15s\n",
			name,
			stat.CPUPercent,
			stat.MemUsage,
			stat.MemPercent,
			stat.NetIO,
			stat.BlockIO,
		)
	}

	fmt.Fprintf(out, "\n")
}
