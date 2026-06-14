package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	if _, exists := services[logsService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", logsService, envName)
	}

	servers, err := resolveEnvironmentServerSet(cfg, envName, logsServer)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	fmt.Printf("Streaming logs from %s on %d takod node(s)...\n\n", logsService, len(servers))

	output := &lockedLogWriter{writer: os.Stdout}
	results := streamLogNodesWith(servers, func(serverName string, server config.ServerConfig, prefix bool) error {
		return streamLogsFromNode(cfg, envName, serverName, server, logsService, logsTail, logsFollow, prefix, output)
	})
	if err := summarizeLogStreamResults(results); err != nil {
		return err
	}

	return nil
}

type lockedLogWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

type logNodeStreamFunc func(serverName string, server config.ServerConfig, prefix bool) error

type logNodeResult struct {
	index      int
	serverName string
	host       string
	err        error
}

func streamLogNodesWith(servers map[string]config.ServerConfig, stream logNodeStreamFunc) []logNodeResult {
	names := sortedLogServerNames(servers)
	prefix := len(names) > 1
	resultCh := make(chan logNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		server := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			resultCh <- logNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
				err:        stream(serverName, server, prefix),
			}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)

	results := make([]logNodeResult, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func streamLogsFromNode(
	cfg *config.Config,
	envName string,
	serverName string,
	server config.ServerConfig,
	service string,
	tail int,
	follow bool,
	prefix bool,
	output io.Writer,
) error {
	client, err := connectTakodStreamNode(server)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}
	defer client.Close()
	if verbose {
		fmt.Printf("Using node: %s (%s)\n", serverName, server.Host)
	}

	endpoint := takodclient.LogsEndpoint(cfg.Project.Name, envName, service, tail, follow)
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamOutput(client, takodSocketFromConfig(cfg), endpoint, writer, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		streamDone <- err
	}()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if prefix {
			fmt.Fprintf(output, "[%s] %s\n", serverName, line)
		} else {
			fmt.Fprintln(output, line)
		}
	}
	scanErr := scanner.Err()
	streamErr := <-streamDone
	if streamErr != nil {
		return streamErr
	}
	if scanErr != nil {
		return scanErr
	}
	return nil
}

func connectTakodStreamNode(server config.ServerConfig) (*ssh.Client, error) {
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Connect(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func summarizeLogStreamResults(results []logNodeResult) error {
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
		return fmt.Errorf("failed to stream logs from all target nodes: %s", strings.Join(failures, "; "))
	}
	return fmt.Errorf("log streaming completed with %d node error(s): %s", len(failures), strings.Join(failures, "; "))
}

func sortedLogServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
