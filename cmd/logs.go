package cmd

import (
	"context"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

var (
	logsServer  string
	logsService string
	logsFollow  bool
	logsTail    int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View logs from deployed services",
	Long: `View logs from deployed services across the environment mesh.

If --server is not specified, streams logs from every configured environment node.

Examples:
  tako logs --service web               # View logs from the environment mesh
  tako logs --service web --server prod # View logs from a specific node
  tako logs --service web -f            # Follow logs in real-time`,
	RunE: runLogs,
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().StringVarP(&logsServer, "server", "s", "", "Node to view logs from (default: all environment nodes)")
	logsCmd.Flags().StringVar(&logsService, "service", "", "Service to view logs from (required)")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Number of lines to show")
	logsCmd.MarkFlagRequired("service")
}

func runLogs(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if logsTail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}

	request := engine.LogsRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Service:     logsService,
		Server:      logsServer,
		Tail:        logsTail,
		Follow:      logsFollow,
	}

	result, err := cliEngine().StreamLogs(cmd.Context(), request)
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

// The logs pipeline lives in pkg/engine; the aliases below keep the
// historical cmd-level names for tests that still reference them.

type logNodeStreamFunc func(serverName string, server config.ServerConfig, prefix bool) error

type logNodeResult struct {
	index      int
	serverName string
	host       string
	err        error
}

func streamLogNodesWith(servers map[string]config.ServerConfig, stream logNodeStreamFunc) []logNodeResult {
	results := engine.StreamLogNodesWith(context.Background(), servers, func(serverName string, server config.ServerConfig, prefix bool) error {
		return stream(serverName, server, prefix)
	})
	return logNodeResultsFromEngine(results)
}

func summarizeLogStreamResults(results []logNodeResult) error {
	return engine.SummarizeLogStreamResults(logNodeResultsToEngine(results))
}

func sortedLogServerNames(servers map[string]config.ServerConfig) []string {
	return engine.SortedLogServerNames(servers)
}

func logNodeResultsFromEngine(results []engine.LogNodeResult) []logNodeResult {
	out := make([]logNodeResult, len(results))
	for i, result := range results {
		out[i] = logNodeResult{
			index:      result.Index,
			serverName: result.ServerName,
			host:       result.Host,
			err:        result.Err,
		}
	}
	return out
}

func logNodeResultsToEngine(results []logNodeResult) []engine.LogNodeResult {
	out := make([]engine.LogNodeResult, len(results))
	for i, result := range results {
		out[i] = engine.LogNodeResult{
			Index:      result.index,
			ServerName: result.serverName,
			Host:       result.host,
			Err:        result.err,
		}
	}
	return out
}
