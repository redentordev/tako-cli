package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	execServer      string
	execAll         bool
	execService     string
	execContainer   string
	execInteractive bool
)

var execCmd = &cobra.Command{
	Use:   "exec <command>",
	Short: "Execute a command on remote server(s) or inside containers",
	Long: `Execute an arbitrary command on one or all configured servers via SSH,
or execute a command inside a running container.

This command is useful for:
  - Inspecting Docker services and containers
  - Running commands inside containers (database migrations, etc.)
  - Checking server state
  - Running maintenance commands
  - Debugging deployments

Examples:
  # Server commands
  tako exec "docker ps"                           # Run on default/all servers
  tako exec --server server1 "docker service ls"  # Run on specific server
  tako exec "df -h"                               # Check disk usage
  
  # Container commands
  tako exec --service ghost "ls -la"              # Execute in ghost service container
  tako exec --service ghost "npm run migrate"    # Run migrations
  tako exec --service mysql "mysql -u root -p"   # Access MySQL (interactive)
  
  # Interactive mode
  tako exec --service ghost -i "/bin/sh"          # Open shell in container
  tako exec --service ghost -i "bash"             # Open bash shell
`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execServer, "server", "s", "", "Execute on specific server")
	execCmd.Flags().BoolVarP(&execAll, "all", "a", false, "Execute on all servers (default: manager only for swarm)")
	execCmd.Flags().StringVar(&execService, "service", "", "Execute command inside service container")
	execCmd.Flags().StringVar(&execContainer, "container", "", "Execute command inside specific container")
	execCmd.Flags().BoolVarP(&execInteractive, "interactive", "i", false, "Interactive mode (allocate TTY)")
}

func runExec(cmd *cobra.Command, args []string) error {
	command := args[0]

	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)

	// If --service or --container is specified, execute inside container
	if execService != "" || execContainer != "" {
		return runExecInContainer(cfg, envName, command)
	}

	// Determine which servers to execute on
	serversToExec := make(map[string]config.ServerConfig)

	if execServer != "" {
		// Execute on specific server
		server, ok := cfg.Servers[execServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", execServer)
		}
		serversToExec[execServer] = server
	} else if execAll {
		// Execute on all servers
		serversToExec = cfg.Servers
	} else {
		// Execute on manager server (for Swarm) or first server
		envServers, err := cfg.GetEnvironmentServers(envName)
		if err != nil {
			return fmt.Errorf("failed to get environment servers: %w", err)
		}

		if len(envServers) > 1 {
			// Multi-server (Swarm): use manager
			managerName, err := cfg.GetManagerServer(envName)
			if err != nil {
				return fmt.Errorf("failed to get manager server: %w", err)
			}
			serversToExec[managerName] = cfg.Servers[managerName]
		} else if len(envServers) == 1 {
			// Single server: use that server
			serverName := envServers[0]
			serversToExec[serverName] = cfg.Servers[serverName]
		} else {
			return fmt.Errorf("no servers configured for environment %s", envName)
		}
	}

	// Execute command on each server
	for serverName, serverCfg := range serversToExec {
		if len(serversToExec) > 1 {
			fmt.Printf("\n=== Server: %s (%s) ===\n", serverName, serverCfg.Host)
		}

		// Connect to server (supports both key and password auth)
		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     serverCfg.Host,
			Port:     serverCfg.Port,
			User:     serverCfg.User,
			SSHKey:   serverCfg.SSHKey,
			Password: serverCfg.Password,
		})
		if err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n", serverName, err)
			continue
		}
		if err := client.Connect(); err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n", serverName, err)
			continue
		}
		defer client.Close()

		// Execute command
		output, err := client.Execute(command)
		if err != nil {
			fmt.Printf("❌ Command failed on %s: %v\n", serverName, err)
			if output != "" {
				fmt.Printf("Output:\n%s\n", output)
			}
			continue
		}

		// Display output
		if strings.TrimSpace(output) != "" {
			fmt.Println(output)
		} else {
			fmt.Println("(no output)")
		}
	}

	return nil
}

// runExecInContainer executes a command inside a container
func runExecInContainer(cfg *config.Config, envName, command string) error {
	// Determine which server to use (default to manager/first server)
	var serverName string
	var serverCfg config.ServerConfig

	if execServer != "" {
		// Use specified server
		var ok bool
		serverCfg, ok = cfg.Servers[execServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", execServer)
		}
		serverName = execServer
	} else {
		// Use manager or first server
		envServers, err := cfg.GetEnvironmentServers(envName)
		if err != nil {
			return fmt.Errorf("failed to get environment servers: %w", err)
		}

		if len(envServers) == 0 {
			return fmt.Errorf("no servers configured for environment %s", envName)
		}

		if len(envServers) > 1 {
			managerName, err := cfg.GetManagerServer(envName)
			if err != nil {
				return fmt.Errorf("failed to get manager server: %w", err)
			}
			serverName = managerName
			serverCfg = cfg.Servers[managerName]
		} else {
			serverName = envServers[0]
			serverCfg = cfg.Servers[serverName]
		}
	}

	// Connect to server (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverCfg.Host,
		Port:     serverCfg.Port,
		User:     serverCfg.User,
		SSHKey:   serverCfg.SSHKey,
		Password: serverCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	var containerName string

	if execContainer != "" {
		// Use specific container name
		containerName = execContainer
	} else if execService != "" {
		// Find container for service
		// Check if we're in Swarm mode
		swarmStateFile := fmt.Sprintf(".tako/swarm_%s_%s.json", cfg.Project.Name, envName)
		useSwarmMode := false
		if _, err := os.Stat(swarmStateFile); err == nil {
			useSwarmMode = true
		}

		if useSwarmMode {
			// In Swarm mode, find the actual container name for the service
			serviceName := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, execService)

			// Get container ID for the service
			findCmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Names}}' | head -1", serviceName)
			output, err := client.Execute(findCmd)
			if err != nil {
				return fmt.Errorf("failed to find container for service %s: %w", execService, err)
			}

			containerName = strings.TrimSpace(output)
			if containerName == "" {
				return fmt.Errorf("no running container found for service %s", execService)
			}
		} else {
			// Single-server mode: use standard naming
			containerName = fmt.Sprintf("%s_%s_%s_1", cfg.Project.Name, envName, execService)
		}
	}

	if verbose {
		fmt.Printf("→ Executing in container: %s\n", containerName)
	}

	// Build docker exec command
	dockerCmd := "docker exec"
	if execInteractive {
		dockerCmd += " -it"
	}
	dockerCmd += fmt.Sprintf(" %s %s", containerName, command)

	if verbose {
		fmt.Printf("→ Command: %s\n\n", dockerCmd)
	}

	// Execute command
	output, err := client.Execute(dockerCmd)
	if err != nil {
		return fmt.Errorf("failed to execute command: %w", err)
	}

	if strings.TrimSpace(output) != "" {
		fmt.Println(output)
	}

	return nil
}
