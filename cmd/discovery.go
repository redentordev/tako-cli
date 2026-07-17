package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	discoveryServer          string
	discoveryAllEnvironments bool
	discoveryJSON            bool
)

var discoveryCmd = &cobra.Command{
	Use:          "discovery",
	Short:        "Inspect shared-node service discovery records",
	SilenceUsage: true,
}

var discoveryExportsCmd = &cobra.Command{
	Use:          "exports",
	Short:        "List exported service discovery records",
	SilenceUsage: true,
	Long: `List exported service discovery records from reachable takod nodes.

Tako stores cross-project service discovery records as Docker network labels on
service-scoped export networks. This command reads those records so operators
can audit shared nodes without guessing network names.`,
	Example: `  tako discovery exports
  tako discovery exports --server node-a
  tako discovery exports --all-environments
  tako discovery exports --json`,
	Args: cobra.NoArgs,
	RunE: runDiscoveryExports,
}

func init() {
	rootCmd.AddCommand(discoveryCmd)
	discoveryCmd.AddCommand(discoveryExportsCmd)
	discoveryExportsCmd.Flags().StringVarP(&discoveryServer, "server", "s", "", "Query a specific configured node")
	discoveryExportsCmd.Flags().BoolVar(&discoveryAllEnvironments, "all-environments", false, "List export records from all environments visible on the selected node(s)")
	discoveryExportsCmd.Flags().BoolVar(&discoveryJSON, "json", false, "Print machine-readable JSON")
}

func runDiscoveryExports(cmd *cobra.Command, args []string) error {
	if discoveryJSON && machineOutputEnabled() {
		return &engine.InvalidRequestError{Err: fmt.Errorf("--json conflicts with the global machine output modes; use --output json for the versioned DiscoveryExportsResult document")}
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := ""
	if !discoveryAllEnvironments {
		envName = getEnvironmentName(cfg)
	}
	serverNames, err := discoveryTargetServers(cfg, envName, discoveryServer, discoveryAllEnvironments)
	if err != nil {
		return err
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer factory.CloseIdleConnections()

	results := collectDiscoveryExports(cfg, serverNames, envName, factory)
	if machineOutputEnabled() {
		// Human table renders to stderr; stdout carries the result document.
		_ = printDiscoveryExportsText(os.Stderr, envName, discoveryAllEnvironments, results)
		return emitDiscoveryExportsResult(envName, discoveryAllEnvironments, results)
	}
	if discoveryJSON {
		return printDiscoveryExportsJSON(cmd, envName, discoveryAllEnvironments, results)
	}
	return printDiscoveryExportsText(cmd.OutOrStdout(), envName, discoveryAllEnvironments, results)
}

// emitDiscoveryExportsResult builds and emits the versioned document; all
// nodes failing exits 1, a partial read exits 6.
func emitDiscoveryExportsResult(envName string, allEnvironments bool, results []discoveryNodeResult) error {
	result := engine.DiscoveryExportsResult{
		APIVersion:      takoapi.APIVersionCurrent,
		Kind:            engine.KindDiscoveryExportsResult,
		Environment:     envName,
		AllEnvironments: allEnvironments,
		Nodes:           []engine.DiscoveryNodeExports{},
	}
	failures := 0
	for _, nodeResult := range results {
		node := engine.DiscoveryNodeExports{
			Server:  nodeResult.ServerName,
			Host:    nodeResult.Host,
			Exports: nodeResult.Exports,
		}
		if nodeResult.ConnectErr != nil {
			node.Error = "connect: " + nodeResult.ConnectErr.Error()
			failures++
		} else if nodeResult.ReadErr != nil {
			node.Error = "discovery: " + nodeResult.ReadErr.Error()
			failures++
		}
		result.Nodes = append(result.Nodes, node)
	}

	var err error
	switch {
	case failures == len(result.Nodes) && failures > 0:
		err = fmt.Errorf("no reachable nodes returned discovery records")
		result.Error = err.Error()
	case failures > 0:
		err = &engine.AttentionError{Err: fmt.Errorf("failed to read discovery records from %d of %d node(s)", failures, len(result.Nodes))}
		result.Error = err.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
		err = emitErr
	}
	return err
}

func discoveryTargetServers(cfg *config.Config, envName string, serverName string, allEnvironments bool) ([]string, error) {
	if !allEnvironments {
		return statePullServerNames(cfg, envName, serverName)
	}
	if serverName != "" {
		if _, ok := cfg.Servers[serverName]; !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		return []string{serverName}, nil
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no servers configured")
	}
	return names, nil
}

type discoveryNodeResult struct {
	Index      int
	ServerName string
	Host       string
	Exports    []takod.ExportDiscoveryRecord
	ConnectErr error
	ReadErr    error
}

type discoveryReadFunc func(serverName string, server config.ServerConfig, environment string) (*takod.ExportDiscoveryResponse, error)

func collectDiscoveryExports(cfg *config.Config, serverNames []string, envName string, factory *nodeclient.Factory) []discoveryNodeResult {
	return collectDiscoveryExportsWith(cfg.Servers, serverNames, envName, func(serverName string, server config.ServerConfig, environment string) (*takod.ExportDiscoveryResponse, error) {
		client, _, err := factory.Client(context.Background(), serverName)
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		response, err := readDiscoveryExportsViaTakod(client, cfg, environment)
		if err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		return response, nil
	})
}

func collectDiscoveryExportsWith(servers map[string]config.ServerConfig, serverNames []string, envName string, read discoveryReadFunc) []discoveryNodeResult {
	resultCh := make(chan discoveryNodeResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			resultCh <- discoveryNodeResult{
				Index:      index,
				ServerName: serverName,
				ConnectErr: fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			response, err := read(serverName, server, envName)
			result := discoveryNodeResult{
				Index:      index,
				ServerName: serverName,
				Host:       server.Host,
			}
			if response != nil {
				result.Exports = append([]takod.ExportDiscoveryRecord(nil), response.Exports...)
			}
			if err != nil {
				if strings.HasPrefix(err.Error(), "connect: ") {
					result.ConnectErr = fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "connect: "))
				} else if strings.HasPrefix(err.Error(), "discovery: ") {
					result.ReadErr = fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "discovery: "))
				} else {
					result.ReadErr = err
				}
			}
			resultCh <- result
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]discoveryNodeResult, len(serverNames))
	for result := range resultCh {
		results[result.Index] = result
	}
	return results
}

func readDiscoveryExportsViaTakod(client any, cfg *config.Config, environment string) (*takod.ExportDiscoveryResponse, error) {
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.DiscoveryExportsEndpoint(environment), nil)
	if err != nil {
		return nil, err
	}
	var response takod.ExportDiscoveryResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

type discoveryExportsJSONOutput struct {
	Environment string                     `json:"environment,omitempty"`
	AllEnvs     bool                       `json:"allEnvironments,omitempty"`
	Nodes       []discoveryExportsJSONNode `json:"nodes"`
}

type discoveryExportsJSONNode struct {
	Node    string                        `json:"node"`
	Exports []takod.ExportDiscoveryRecord `json:"exports,omitempty"`
	Error   string                        `json:"error,omitempty"`
}

func printDiscoveryExportsJSON(cmd *cobra.Command, envName string, allEnvironments bool, results []discoveryNodeResult) error {
	output := discoveryExportsJSONOutput{
		Environment: envName,
		AllEnvs:     allEnvironments,
		Nodes:       make([]discoveryExportsJSONNode, 0, len(results)),
	}
	readableNodes := 0
	for _, result := range results {
		node := discoveryExportsJSONNode{
			Node:    result.ServerName,
			Exports: result.Exports,
		}
		if result.ConnectErr != nil {
			node.Error = "connect: " + result.ConnectErr.Error()
		} else if result.ReadErr != nil {
			node.Error = "discovery: " + result.ReadErr.Error()
		} else {
			readableNodes++
		}
		output.Nodes = append(output.Nodes, node)
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
	if readableNodes == 0 {
		return fmt.Errorf("no reachable nodes returned discovery records")
	}
	return nil
}

func printDiscoveryExportsText(out io.Writer, envName string, allEnvironments bool, results []discoveryNodeResult) error {
	fmt.Fprintln(out, "Export discovery records")
	if allEnvironments {
		fmt.Fprintln(out, "Environment: all")
	} else {
		fmt.Fprintf(out, "Environment: %s\n", envName)
	}
	fmt.Fprintln(out)

	records := make([]discoveryTextRecord, 0)
	var warnings []string
	readableNodes := 0
	for _, result := range results {
		if result.ConnectErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: connect failed - %v", result.ServerName, result.ConnectErr))
			continue
		}
		if result.ReadErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: discovery read failed - %v", result.ServerName, result.ReadErr))
			continue
		}
		readableNodes++
		for _, record := range result.Exports {
			records = append(records, discoveryTextRecord{Node: result.ServerName, ExportDiscoveryRecord: record})
		}
	}

	if len(records) == 0 {
		fmt.Fprintln(out, "No exported services found on readable nodes.")
	} else {
		sort.Slice(records, func(i, j int) bool {
			left := records[i]
			right := records[j]
			for _, cmp := range []int{
				strings.Compare(left.Node, right.Node),
				strings.Compare(left.Environment, right.Environment),
				strings.Compare(left.Project, right.Project),
				strings.Compare(left.Service, right.Service),
			} {
				if cmp < 0 {
					return true
				}
				if cmp > 0 {
					return false
				}
			}
			return false
		})
		fmt.Fprintf(out, "%-16s %-14s %-20s %-16s %-34s %s\n", "NODE", "ENV", "PROJECT", "SERVICE", "ALIAS", "NETWORK")
		for _, record := range records {
			fmt.Fprintf(out, "%-16s %-14s %-20s %-16s %-34s %s\n",
				truncateDiscoveryColumn(record.Node, 16),
				truncateDiscoveryColumn(record.Environment, 14),
				truncateDiscoveryColumn(record.Project, 20),
				truncateDiscoveryColumn(record.Service, 16),
				truncateDiscoveryColumn(record.Alias, 34),
				record.Network,
			)
		}
	}

	if len(warnings) > 0 {
		sort.Strings(warnings)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, warning := range warnings {
			fmt.Fprintf(out, "  - %s\n", warning)
		}
	}

	if readableNodes == 0 {
		return fmt.Errorf("no reachable nodes returned discovery records")
	}
	return nil
}

type discoveryTextRecord struct {
	Node string
	takod.ExportDiscoveryRecord
}

func truncateDiscoveryColumn(value string, width int) string {
	if len(value) <= width {
		return value
	}
	if width <= 1 {
		return value[:width]
	}
	return value[:width-1] + "."
}
