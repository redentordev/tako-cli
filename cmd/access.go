package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/accesslog"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	accessFollow  bool
	accessTail    int
	accessServer  string
	accessService string
)

var accessCmd = &cobra.Command{
	Use:   "access [service]",
	Short: "Stream access logs from Traefik reverse proxy",
	Long: `Stream and format access logs from the Traefik reverse proxy.

This command shows HTTP request logs including:
  - Timestamp
  - HTTP status code (color-coded)
  - HTTP method
  - Client IP address
  - Response time
  - Response size
  - Request path

Similar to Vercel or Cloudflare observability, but for your own infrastructure.

Examples:
  tako access              # Show recent logs for default service
  tako access -f           # Follow logs in real-time
  tako access web          # Show logs for 'web' service
  tako access --tail 50    # Show last 50 log entries
  tako access -v           # Verbose mode (includes User-Agent, Referer)`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAccess,
}

func init() {
	rootCmd.AddCommand(accessCmd)
	accessCmd.Flags().BoolVarP(&accessFollow, "follow", "f", false, "Follow log output")
	accessCmd.Flags().IntVarP(&accessTail, "tail", "n", 50, "Number of lines to show from the end")
	accessCmd.Flags().StringVarP(&accessServer, "server", "s", "", "Server to fetch logs from (defaults to first production server)")
}

func runAccess(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment and services
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Determine service name (for filtering)
	accessService = ""
	if len(args) > 0 {
		accessService = args[0]
		// Validate service exists
		if _, exists := services[accessService]; !exists {
			return fmt.Errorf("service '%s' not found in config", accessService)
		}
	}

	// Get server configuration
	serverConfig, serverName, err := getServerConfig(cfg, accessServer)
	if err != nil {
		return err
	}

	if verbose {
		fmt.Printf("→ Fetching access logs from %s (service: %s)\n", serverName, accessService)
	}

	// Connect to server (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverConfig.Host,
		Port:     serverConfig.Port,
		User:     serverConfig.User,
		SSHKey:   serverConfig.SSHKey,
		Password: serverConfig.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	defer client.Close()

	// Determine log file path (Traefik access logs)
	// In Swarm mode, all services log to a single access.log file
	logPath := "/var/log/traefik/access.log"

	// Build tail command
	tailCmd := fmt.Sprintf("sudo tail -n %d", accessTail)
	if accessFollow {
		tailCmd += " -f"
	}
	tailCmd += fmt.Sprintf(" %s 2>/dev/null || echo 'Log file not found. Deploy your application first.'", logPath)

	// Create formatter
	formatter := accesslog.NewFormatter(verbose)

	// Set service filter if specified
	if accessService != "" {
		formatter.SetServiceFilter(accessService)
		if verbose {
			fmt.Printf("→ Filtering logs for service: %s\n", accessService)
		}
	}

	// Print header
	fmt.Println()
	fmt.Println(formatter.FormatHeader())
	fmt.Println()

	// Stream logs
	if accessFollow {
		// Use streaming for follow mode
		reader, writer := io.Pipe()

		// Start SSH stream in goroutine
		go func() {
			defer writer.Close()
			if err := client.ExecuteStream(tailCmd, writer, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "Error streaming logs: %v\n", err)
			}
		}()

		// Read and format lines
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			formatted, err := formatter.FormatLine(line)
			if err != nil {
				if verbose {
					fmt.Fprintf(os.Stderr, "Warning: Failed to format line: %v\n", err)
				}
				continue
			}
			if formatted != "" {
				fmt.Println(formatted)
			}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("error reading logs: %w", err)
		}
	} else {
		// Execute once and format
		output, err := client.Execute(tailCmd)
		if err != nil {
			return fmt.Errorf("failed to fetch logs: %w", err)
		}

		// Split output into lines and format each
		for _, line := range splitLines(output) {
			formatted, err := formatter.FormatLine(line)
			if err != nil {
				if verbose {
					fmt.Fprintf(os.Stderr, "Warning: Failed to format line: %v\n", err)
				}
				continue
			}
			if formatted != "" {
				fmt.Println(formatted)
			}
		}
	}

	return nil
}

// splitLines splits output into lines
func splitLines(output string) []string {
	return strings.Split(strings.TrimSpace(output), "\n")
}

// getServerConfig returns the server configuration to use
func getServerConfig(cfg *config.Config, serverName string) (*config.ServerConfig, string, error) {
	if serverName != "" {
		// Use specified server
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, "", fmt.Errorf("server '%s' not found in config", serverName)
		}
		return &server, serverName, nil
	}

	// Find first production server (or any server)
	for name, server := range cfg.Servers {
		if name == "production" || len(cfg.Servers) == 1 {
			return &server, name, nil
		}
	}

	// Just use first server
	for name, server := range cfg.Servers {
		return &server, name, nil
	}

	return nil, "", fmt.Errorf("no servers configured")
}
