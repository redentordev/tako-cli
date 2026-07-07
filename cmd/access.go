package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/accesslog"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
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
  tako access              # Show recent logs from the environment mesh
  tako access -f           # Follow logs in real-time
  tako access web          # Show logs for 'web' service
  tako access --server prod # Show logs from a specific node
  tako access --tail 50    # Show last 50 log entries
  tako access -v           # Verbose mode (includes User-Agent, Referer)`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAccess,
}

func init() {
	rootCmd.AddCommand(accessCmd)
	accessCmd.Flags().BoolVarP(&accessFollow, "follow", "f", false, "Follow log output")
	accessCmd.Flags().IntVarP(&accessTail, "tail", "n", 50, "Number of lines to show from the end")
	accessCmd.Flags().StringVarP(&accessServer, "server", "s", "", "Node to fetch logs from (default: all environment nodes)")
}

func runAccess(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
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

	servers, err := resolveEnvironmentServerSet(cfg, envName, accessServer)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	formatter := accesslog.NewFormatter(verbose)
	if accessService != "" {
		formatter.SetServiceFilter(accessService)
		if verbose {
			fmt.Printf("→ Filtering logs for service: %s\n", accessService)
		}
	}

	// Machine modes reserve stdout for parseable output: entries stream as
	// access.line events and the human rendering moves to stderr.
	var headerOut io.Writer = os.Stdout
	var sink accessLineSink
	if machineOutputEnabled() {
		headerOut = os.Stderr
		service := accessService
		sink = func(serverName string, rawLine string, formatted string, prefix bool) {
			var message strings.Builder
			writeAccessLogLine(&message, serverName, formatted, prefix)
			cliEngine().EventStream().Emit(events.Event{
				Type:    events.TypeAccessLine,
				Phase:   events.PhaseLogs,
				Level:   events.LevelInfo,
				Service: service,
				Message: message.String(),
				Data:    map[string]any{"node": serverName, "data": rawLine},
			})
		}
	} else {
		output := &lockedLogWriter{writer: os.Stdout}
		sink = func(serverName string, _ string, formatted string, prefix bool) {
			writeAccessLogLine(output, serverName, formatted, prefix)
		}
	}

	// Print header
	fmt.Fprintln(headerOut)
	fmt.Fprintln(headerOut, formatter.FormatHeader())
	fmt.Fprintln(headerOut)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	results := streamAccessNodesWith(ctx, servers, func(serverName string, server config.ServerConfig, prefix bool) error {
		return streamAccessFromNode(ctx, cfg, serverName, server, formatter, accessService, accessTail, accessFollow, prefix, sink)
	})
	if err := summarizeAccessStreamResults(results); err != nil {
		return err
	}
	return nil
}

// accessLineSink delivers one formatted access-log entry; machine modes emit
// access.line events, text mode writes to stdout.
type accessLineSink func(serverName string, rawLine string, formatted string, prefix bool)

type accessNodeStreamFunc func(serverName string, server config.ServerConfig, prefix bool) error

type accessNodeResult struct {
	index      int
	serverName string
	host       string
	err        error
}

func streamAccessNodesWith(ctx context.Context, servers map[string]config.ServerConfig, stream accessNodeStreamFunc) []accessNodeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	names := sortedAccessServerNames(servers)
	prefix := len(names) > 1
	resultCh := make(chan accessNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		server := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			var err error
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			} else {
				err = stream(serverName, server, prefix)
			}
			resultCh <- accessNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
				err:        err,
			}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)

	results := make([]accessNodeResult, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func streamAccessFromNode(
	ctx context.Context,
	cfg *config.Config,
	serverName string,
	server config.ServerConfig,
	formatter *accesslog.Formatter,
	service string,
	tail int,
	follow bool,
	prefix bool,
	sink accessLineSink,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := connectTakodStreamNodeContext(ctx, server)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}
	defer client.Close()

	if verbose {
		fmt.Printf("→ Fetching access logs from %s (service: %s)\n", serverName, service)
	}

	reader, writer := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		endpoint := takodclient.AccessLogsEndpoint(tail, follow)
		err := takodclient.StreamOutputWithContext(ctx, client, takodSocketFromConfig(cfg), endpoint, writer, writer)
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
			sink(serverName, line, formatted, prefix)
		}
	}

	scanErr := scanner.Err()
	streamErr := <-errCh
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return fmt.Errorf("error reading access logs: %w", scanErr)
	}
	if streamErr != nil {
		return fmt.Errorf("failed to stream access logs: %w", streamErr)
	}
	return nil
}

func writeAccessLogLine(output io.Writer, serverName string, formatted string, prefix bool) {
	if !prefix {
		fmt.Fprintln(output, formatted)
		return
	}
	for _, line := range strings.Split(formatted, "\n") {
		fmt.Fprintf(output, "[%s] %s\n", serverName, line)
	}
}

func summarizeAccessStreamResults(results []accessNodeResult) error {
	var failures []string
	for _, result := range results {
		if result.err == nil {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %v", result.serverName, result.err))
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	if len(failures) == len(results) {
		return fmt.Errorf("failed to stream access logs from all target nodes: %s", strings.Join(failures, "; "))
	}
	return fmt.Errorf("access log streaming completed with %d node error(s): %s", len(failures), strings.Join(failures, "; "))
}

func sortedAccessServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
