package cmd

import (
	"context"
	"hash/fnv"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	logsServer     string
	logsServices   []string
	logsFollow     bool
	logsTail       int
	logsSince      string
	logsUntil      string
	logsGrep       string
	logsIgnoreCase bool
	logsTimestamps bool
)

var logsCmd = &cobra.Command{
	Use:   "logs [SERVICE...]",
	Short: "View logs from deployed services",
	Long: `View logs from deployed services across the environment mesh.

Services are named as arguments or with --service (repeatable, or
comma-separated). With no service, logs from every long-running service in
the environment are shown. Name a job to see the output of its most recent
recorded run, or follow it with -f to stream its runs as they happen.
Deploy-time run services are not available through tako logs.

If --server is not specified, streams logs from every configured environment
node. When more than one source is shown, each line is prefixed with
service.slot@node; prefixes are colored when stdout is a terminal and
NO_COLOR is unset.

--since and --until accept a duration (10m, 2h; relative to each node's
clock), an RFC3339 timestamp, or Unix seconds. With --since and no --tail,
the whole window is shown (up to 10000 lines per container). --grep keeps
only lines matching an RE2 regular expression; --tail then counts matching
lines.

Examples:
  tako logs                              # All services, last 100 lines each
  tako logs web                          # One service
  tako logs web worker -f                # Follow two services
  tako logs --service web --server prod  # One node only
  tako logs web --since 10m -g 'error|panic' --ignore-case
  tako logs web --since 2026-10-11T09:00:00Z --until 2026-10-11T10:00:00Z -t
  tako logs nightly-report -f            # Follow runs of a job`,
	Args: cobra.ArbitraryArgs,
	RunE: runLogs,
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().StringVarP(&logsServer, "server", "s", "", "Node to view logs from (default: all environment nodes)")
	logsCmd.Flags().StringSliceVar(&logsServices, "service", nil, "Service or job to view logs from (repeatable or comma-separated; default: all services)")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Number of lines to show per container (matching lines with --grep; max 10000)")
	logsCmd.Flags().StringVar(&logsSince, "since", "", "Show logs since a duration (10m), RFC3339 timestamp, or Unix seconds")
	logsCmd.Flags().StringVar(&logsUntil, "until", "", "Show logs before a duration, RFC3339 timestamp, or Unix seconds (not with --follow)")
	logsCmd.Flags().StringVarP(&logsGrep, "grep", "g", "", "Only show lines matching an RE2 regular expression")
	logsCmd.Flags().BoolVar(&logsIgnoreCase, "ignore-case", false, "Match --grep case-insensitively")
	logsCmd.Flags().BoolVarP(&logsTimestamps, "timestamps", "t", false, "Prefix each line with its timestamp")
}

func runLogs(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	request := buildLogsRequest(cmd, args)
	request.Config = cfg
	request.Environment = getEnvironmentName(cfg)

	result, err := cliEngine().StreamLogs(cmd.Context(), request)
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

// buildLogsRequest maps flags and positional service names onto a request.
// Validation (time formats, regex, tail range, unknown services) happens in
// the engine so library callers get the same InvalidRequestError.
func buildLogsRequest(cmd *cobra.Command, args []string) engine.LogsRequest {
	services := make([]string, 0, len(args)+len(logsServices))
	services = append(services, args...)
	services = append(services, logsServices...)
	return engine.LogsRequest{
		Services:     services,
		Server:       logsServer,
		Tail:         logsTail,
		TailExplicit: cmd.Flags().Changed("tail"),
		Follow:       logsFollow,
		Since:        logsSince,
		Until:        logsUntil,
		Grep:         logsGrep,
		IgnoreCase:   logsIgnoreCase,
		Timestamps:   logsTimestamps,
	}
}

// logsPrefixPalette is the fixed set of ANSI foreground colors used for log
// source prefixes (red, green, yellow, blue, magenta, cyan).
var logsPrefixPalette = []string{"31", "32", "33", "34", "35", "36"}

// logsColorEnabled reports whether log source prefixes are colored: only
// when the writer is a terminal, NO_COLOR is unset, and no machine output
// mode is active. There is deliberately no flag for this.
func logsColorEnabled(out *os.File) bool {
	if machineOutputEnabled() {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	return out != nil && term.IsTerminal(int(out.Fd()))
}

// logsPrefixColor maps a source prefix to a stable palette entry.
func logsPrefixColor(prefix string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(prefix))
	return logsPrefixPalette[hash.Sum32()%uint32(len(logsPrefixPalette))]
}

// colorizeLogLinePrefix colors the service.slot@node prefix of a rendered
// log line. Lines rendered without a prefix (single source) are unchanged.
func colorizeLogLinePrefix(event events.Event, message string) string {
	service, _ := event.Data["service"].(string)
	node, _ := event.Data["node"].(string)
	if service == "" || node == "" {
		return message
	}
	prefix := service + "@" + node
	if replica, _ := event.Data["replica"].(string); replica != "" {
		prefix = service + "." + replica + "@" + node
	}
	index := strings.Index(message, prefix)
	if index < 0 || !strings.HasPrefix(strings.TrimLeft(message[index+len(prefix):], " "), "| ") {
		return message
	}
	return message[:index] + "\x1b[" + logsPrefixColor(prefix) + "m" + prefix + "\x1b[0m" + message[index+len(prefix):]
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
