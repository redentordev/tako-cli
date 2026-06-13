package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/redentordev/tako-cli/pkg/accesslog"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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
	Short: "Stream proxy access logs",
	Long: `Stream and format access logs from tako-proxy.

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
	accessCmd.Flags().StringVarP(&accessServer, "server", "s", "", "Node to fetch logs from (defaults to first reachable environment node)")
}

func runAccess(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cfg.IsTakodRuntime() {
		return fmt.Errorf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
	}
	if accessTail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	accessService = ""
	if len(args) > 0 {
		accessService = args[0]
		if _, exists := services[accessService]; !exists {
			return fmt.Errorf("service '%s' not found in config", accessService)
		}
	}

	serverName, _, client, err := connectResolvedServer(cfg, envName, accessServer)
	if err != nil {
		return err
	}
	defer client.Close()

	if verbose {
		fmt.Printf("→ Fetching access logs from %s (service: %s)\n", serverName, accessService)
	}

	formatter := accesslog.NewFormatter(verbose)
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

	reader, writer := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		endpoint := takodclient.AccessLogsEndpoint(accessTail, accessFollow)
		err := takodclient.StreamOutput(client, takodSocketFromConfig(cfg), endpoint, writer, os.Stderr)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		errCh <- err
	}()

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
		return fmt.Errorf("error reading access logs: %w", err)
	}
	if err := <-errCh; err != nil {
		return fmt.Errorf("failed to stream access logs: %w", err)
	}
	return nil
}
