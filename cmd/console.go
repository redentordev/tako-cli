package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	consoleServer string
	consoleShell  string
)

var consoleCmd = &cobra.Command{
	Use:   "console [service]",
	Short: "Open an interactive shell inside a running container",
	Long: `Open an interactive shell inside a running service container.

This command connects to the remote server via SSH, finds the running container
for the specified service, and opens an interactive shell session.

If only one service is defined, it is used automatically.

Examples:
  tako console                     # Open /bin/sh in the only service
  tako console web                 # Open /bin/sh in the "web" service
  tako console web --shell bash    # Open bash in "web"
  tako console web -s server1     # Use a specific server
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runConsole,
}

func init() {
	rootCmd.AddCommand(consoleCmd)
	consoleCmd.Flags().StringVarP(&consoleServer, "server", "s", "", "Target server (default: manager/first)")
	consoleCmd.Flags().StringVar(&consoleShell, "shell", "/bin/sh", "Shell to open inside the container")
}

func runConsole(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfigWithInfra(cfgFile, ".tako")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)

	// Resolve service name
	serviceName, err := resolveServiceName(cfg, envName, args)
	if err != nil {
		return err
	}

	// Resolve server
	serverName, serverCfg, err := resolveServer(cfg, envName, consoleServer)
	if err != nil {
		return err
	}

	// Connect to server
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverCfg.Host,
		Port:     serverCfg.Port,
		User:     serverCfg.User,
		SSHKey:   serverCfg.SSHKey,
		Password: serverCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client for %s: %w", serverName, err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	// Find container name
	containerName, err := findContainerName(client, cfg, envName, serviceName)
	if err != nil {
		return err
	}

	if verbose {
		fmt.Printf("→ Opening %s in container: %s on %s\n", consoleShell, containerName, serverName)
	}

	// Build and execute interactive docker exec
	dockerCmd := fmt.Sprintf("docker exec -it %s %s", containerName, consoleShell)
	return client.ExecuteInteractive(dockerCmd)
}

// resolveServiceName picks the service from args or auto-selects if there's only one.
func resolveServiceName(cfg *config.Config, envName string, args []string) (string, error) {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return "", fmt.Errorf("failed to get services: %w", err)
	}

	if len(args) == 1 {
		name := args[0]
		if _, ok := services[name]; !ok {
			names := serviceNames(services)
			return "", fmt.Errorf("service '%s' not found. Available services: %s", name, strings.Join(names, ", "))
		}
		return name, nil
	}

	// No arg provided — auto-select if exactly one service
	if len(services) == 1 {
		for name := range services {
			return name, nil
		}
	}

	names := serviceNames(services)
	return "", fmt.Errorf("multiple services available, specify one: %s", strings.Join(names, ", "))
}

func serviceNames(services map[string]config.ServiceConfig) []string {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveServer picks the target server from the flag or defaults to manager/first.
func resolveServer(cfg *config.Config, envName, serverFlag string) (string, config.ServerConfig, error) {
	if serverFlag != "" {
		sc, ok := cfg.Servers[serverFlag]
		if !ok {
			return "", config.ServerConfig{}, fmt.Errorf("server '%s' not found in config", serverFlag)
		}
		return serverFlag, sc, nil
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return "", config.ServerConfig{}, fmt.Errorf("failed to get environment servers: %w", err)
	}

	if len(envServers) == 0 {
		return "", config.ServerConfig{}, fmt.Errorf("no servers configured for environment %s", envName)
	}

	if len(envServers) > 1 {
		managerName, err := cfg.GetManagerServer(envName)
		if err != nil {
			return "", config.ServerConfig{}, fmt.Errorf("failed to get manager server: %w", err)
		}
		return managerName, cfg.Servers[managerName], nil
	}

	name := envServers[0]
	return name, cfg.Servers[name], nil
}

// findContainerName resolves the running container name for a service.
func findContainerName(client *ssh.Client, cfg *config.Config, envName, serviceName string) (string, error) {
	// Check if we're in Swarm mode
	swarmStateFile := fmt.Sprintf(".tako/swarm_%s_%s.json", cfg.Project.Name, envName)
	if _, err := os.Stat(swarmStateFile); err == nil {
		// Swarm mode: query docker for the actual container
		filter := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, serviceName)
		findCmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Names}}' | head -1", filter)
		output, err := client.Execute(findCmd)
		if err != nil {
			return "", fmt.Errorf("failed to find container for service %s: %w", serviceName, err)
		}
		name := strings.TrimSpace(output)
		if name == "" {
			return "", fmt.Errorf("no running container found for service %s", serviceName)
		}
		return name, nil
	}

	// Single-server mode: use standard naming
	return fmt.Sprintf("%s_%s_%s_1", cfg.Project.Name, envName, serviceName), nil
}
