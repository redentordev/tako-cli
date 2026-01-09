package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	statsLive    bool
	statsService string
	statsAll     bool
)

// ContainerStats represents resource usage for a single container
type ContainerStats struct {
	Name       string
	CPUPercent string
	MemUsage   string
	MemPercent string
	NetIO      string
	BlockIO    string
	PIDs       string
}

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
	// Load configuration
	cfg, err := config.LoadConfigWithInfra(cfgFile, ".tako")
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

	if statsLive {
		return showLiveStats(cfg, servers, sshPool, envName)
	}

	return showStatsOnce(cfg, servers, sshPool, envName)
}

func showStatsOnce(cfg *config.Config, servers []string, sshPool *ssh.Pool, envName string) error {
	fmt.Printf("\n=== Container Statistics ===\n")
	fmt.Printf("Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, serverName := range servers {
		server := cfg.Servers[serverName]

		// Get or create SSH client (supports both key and password auth)
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			fmt.Printf("❌ %s (%s): Failed to connect - %v\n\n", serverName, server.Host, err)
			continue
		}

		// Build docker stats command
		statsCmd := buildDockerStatsCommand(cfg, envName)

		// Execute docker stats
		output, err := client.Execute(statsCmd)
		if err != nil {
			fmt.Printf("❌ %s (%s): Failed to get stats - %v\n\n", serverName, server.Host, err)
			continue
		}

		// Parse and display stats
		displayContainerStats(serverName, server.Host, output)
	}

	return nil
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

			statsCmd := buildDockerStatsCommand(cfg, envName)
			output, err := client.Execute(statsCmd)
			if err != nil {
				fmt.Printf("❌ %s: Stats unavailable\n\n", serverName)
				continue
			}

			displayContainerStats(serverName, server.Host, output)
		}

		<-ticker.C
	}
}

func buildDockerStatsCommand(cfg *config.Config, envName string) string {
	// Use docker stats with JSON format and no-stream (single snapshot)
	cmd := "docker stats --no-stream --format '{{json .}}'"

	// If not showing all containers, filter by project
	if !statsAll {
		projectName := cfg.Project.Name
		// In swarm mode, containers are named: {project}_{env}_{service}.{replica}.{hash}
		cmd = fmt.Sprintf("docker ps --format '{{.Names}}' | grep '^%s_%s_' | xargs -r docker stats --no-stream --format '{{json .}}'", projectName, envName)
	}

	// Filter by specific service if requested
	if statsService != "" {
		projectName := cfg.Project.Name
		cmd = fmt.Sprintf("docker ps --format '{{.Names}}' | grep '^%s_%s_%s\\.' | xargs -r docker stats --no-stream --format '{{json .}}'", projectName, envName, statsService)
	}

	return cmd
}

func displayContainerStats(serverName, serverHost, output string) {
	if strings.TrimSpace(output) == "" {
		fmt.Printf("📊 %s (%s)\n", serverName, serverHost)
		fmt.Printf("No containers found\n\n")
		return
	}

	// Parse JSON output (one JSON object per line)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var stats []ContainerStats

	for _, line := range lines {
		if line == "" {
			continue
		}

		var rawStat map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rawStat); err != nil {
			continue
		}

		stat := ContainerStats{
			Name:       getString(rawStat, "Name"),
			CPUPercent: getString(rawStat, "CPUPerc"),
			MemUsage:   getString(rawStat, "MemUsage"),
			MemPercent: getString(rawStat, "MemPerc"),
			NetIO:      getString(rawStat, "NetIO"),
			BlockIO:    getString(rawStat, "BlockIO"),
			PIDs:       getString(rawStat, "PIDs"),
		}

		stats = append(stats, stat)
	}

	// Sort by name
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	// Display
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

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}
