package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	nodeLogsTail   int
	nodeLogsFollow bool
	nodeLogsUnit   string
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Inspect takod nodes",
}

var nodeLogsCmd = &cobra.Command{
	Use:   "logs [NODE]",
	Short: "Stream node-local takod logs",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runNodeLogs,
}

func init() {
	rootCmd.AddCommand(nodeCmd)
	nodeCmd.AddCommand(nodeLogsCmd)
	nodeLogsCmd.Flags().IntVarP(&nodeLogsTail, "tail", "n", 100, "Number of lines to show")
	nodeLogsCmd.Flags().BoolVarP(&nodeLogsFollow, "follow", "f", false, "Follow log output")
	nodeLogsCmd.Flags().StringVar(&nodeLogsUnit, "unit", "takod", "Node unit to stream: takod or tako-monitor")
}

func runNodeLogs(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if nodeLogsTail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}

	envName := getEnvironmentName(cfg)
	requestedServer := ""
	if len(args) > 0 {
		requestedServer = args[0]
	}
	servers, err := resolveEnvironmentServerSet(cfg, envName, requestedServer)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	output := &lockedLogWriter{writer: os.Stdout}
	results := streamLogNodesWith(servers, func(serverName string, server config.ServerConfig, prefix bool) error {
		return streamNodeLogsFromNode(cfg, serverName, server, nodeLogsUnit, nodeLogsTail, nodeLogsFollow, prefix, output)
	})
	return summarizeLogStreamResults(results)
}

func streamNodeLogsFromNode(
	cfg *config.Config,
	serverName string,
	server config.ServerConfig,
	unit string,
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

	endpoint := takodclient.NodeLogsEndpoint(unit, tail, follow)
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
