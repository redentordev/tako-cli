package cmd

import (
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
	statsLive    bool
	statsService string
	statsAll     bool
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
	statsCmd.Flags().StringVarP(&statsService, "service", "s", "", "Filter by service name")
	statsCmd.Flags().BoolVar(&statsAll, "all", false, "Show all containers (not just current project)")
}

func runStats(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cfg.IsTakodRuntime() {
		return fmt.Errorf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
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

	if statsLive {
		return showLiveStats(cfg, servers, sshPool, envName)
	}

	return showStatsOnce(cfg, servers, sshPool, envName)
}

func showStatsOnce(cfg *config.Config, servers []string, sshPool *ssh.Pool, envName string) error {
	fmt.Printf("\n=== Container Statistics ===\n")
	fmt.Printf("Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, result := range collectStatsOnce(cfg, servers, sshPool, envName) {
		if result.connectErr != nil {
			fmt.Printf("❌ %s (%s): Failed to connect - %v\n\n", result.serverName, result.host, result.connectErr)
			continue
		}
		if result.statsErr != nil {
			fmt.Printf("❌ %s (%s): Failed to get stats - %v\n\n", result.serverName, result.host, result.statsErr)
			continue
		}
		displayContainerStats(result.serverName, result.host, result.stats)
	}

	return nil
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

func collectStatsOnce(cfg *config.Config, servers []string, sshPool *ssh.Pool, envName string) []statsNodeResult {
	return collectStatsOnceWith(cfg.Servers, servers, func(serverName string, server config.ServerConfig) (*takod.StatsResponse, error) {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
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

func showLiveStats(cfg *config.Config, servers []string, sshPool *ssh.Pool, envName string) error {
	fmt.Printf("=== Live Container Stats (Press Ctrl+C to exit) ===\n\n")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		// Clear screen (ANSI escape code)
		fmt.Print("\033[H\033[2J")

		fmt.Printf("=== Live Container Stats - %s ===\n\n", time.Now().Format("2006-01-02 15:04:05"))

		for _, serverName := range servers {
			server := cfg.Servers[serverName]

			client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
			if err != nil {
				fmt.Printf("❌ %s: Connection failed\n\n", serverName)
				continue
			}

			response, err := readStatsViaTakod(client, cfg, envName)
			if err != nil {
				fmt.Printf("❌ %s: Stats unavailable\n\n", serverName)
				continue
			}

			displayContainerStats(serverName, server.Host, response.Stats)
		}

		<-ticker.C
	}
}

func readStatsViaTakod(client *ssh.Client, cfg *config.Config, envName string) (*takod.StatsResponse, error) {
	return readStatsViaTakodWithOptions(client, cfg, envName, statsService, statsAll)
}

func readStatsViaTakodWithOptions(client *ssh.Client, cfg *config.Config, envName string, service string, all bool) (*takod.StatsResponse, error) {
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

func displayContainerStats(serverName, serverHost string, stats []takod.ContainerStat) {
	if len(stats) == 0 {
		fmt.Printf("📊 %s (%s)\n", serverName, serverHost)
		fmt.Printf("No containers found\n\n")
		return
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	fmt.Printf("📊 %s (%s)\n", serverName, serverHost)
	fmt.Printf("─────────────────────────────────────────────────────────────────────────────────────\n")
	fmt.Printf("%-40s %10s %20s %10s %20s %15s\n", "CONTAINER", "CPU %", "MEMORY", "MEM %", "NET I/O", "BLOCK I/O")
	fmt.Printf("─────────────────────────────────────────────────────────────────────────────────────\n")

	for _, stat := range stats {
		// Truncate container name if too long
		name := stat.Name
		if len(name) > 38 {
			name = name[:35] + "..."
		}

		fmt.Printf("%-40s %10s %20s %10s %20s %15s\n",
			name,
			stat.CPUPercent,
			stat.MemUsage,
			stat.MemPercent,
			stat.NetIO,
			stat.BlockIO,
		)
	}

	fmt.Printf("\n")
}
