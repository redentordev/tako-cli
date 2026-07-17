package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var stateCmd = &cobra.Command{
	Use:          "state",
	Short:        "Manage deployment state synchronization",
	SilenceUsage: true,
	Long: `Manage deployment state between local .tako directory and the takod mesh.

When you clone a project or work on a different machine, local state may be missing.
Use 'tako state pull' to sync remote state to your local machine.`,
}

var statePullCmd = &cobra.Command{
	Use:          "pull",
	Short:        "Pull remote state to local .tako directory",
	SilenceUsage: true,
	Long: `Pull deployment state from the takod mesh to your local .tako directory.

This is useful when:
  - Cloning a project that has already been deployed
  - Working on a different machine
  - Recovering local state after accidental deletion

The command will:
  1. Connect to reachable environment nodes
  2. Read deployment history from takod state
  3. Refresh local deployment records in .tako without touching local secrets`,
	RunE: runStatePull,
}

var stateStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show state synchronization status",
	SilenceUsage: true,
	Long: `Show the current state of local and remote mesh deployment state.

Displays:
  - Whether local .tako directory exists
  - Last local deployment information
  - Per-node remote deployment and runtime state
  - Best known deployment, desired, and actual runtime state
  - Whether remote operation leases are currently held
  - Whether sync is needed`,
	RunE: runStateStatus,
}

var stateRepairCmd = &cobra.Command{
	Use:          "repair",
	Short:        "Repair deployment and runtime state across reachable mesh nodes",
	SilenceUsage: true,
	Long: `Repair deployment and runtime state across reachable nodes in the active environment.

The command reads deployment history, desired revisions, and actual snapshots
from every reachable environment node. It chooses the freshest copies, writes
them back to all reachable nodes under the remote operation lease, and refreshes
local .tako deployment state when deployment history is available.

Use this when nodes disagree, a node was replaced, or a new laptop needs
the best available state before deploying.`,
	RunE: runStateRepair,
}

var stateForgetNodeCmd = &cobra.Command{
	Use:          "forget-node <node>",
	Short:        "Remove a retired node from replicated runtime state",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	Long: `Remove a retired or destroyed node from replicated runtime state.

This command does not edit tako.yaml and does not touch application containers.
It deletes the node-local actual snapshot for the named node and rewrites the
aggregate actual snapshot without that node on every reachable environment node.

Use this after removing a destroyed node from the active environment config, or
pass --force when you intentionally need to forget a node that is still listed
in the environment.`,
	RunE: runStateForgetNode,
}

var stateLeaseCmd = &cobra.Command{
	Use:          "lease",
	Short:        "Show remote operation leases",
	SilenceUsage: true,
	Long: `Show remote operation leases held on reachable takod nodes.

Use this when a mutating command reports that the environment is locked.`,
	RunE: runStateLease,
}

var stateLeaseReleaseCmd = &cobra.Command{
	Use:          "release",
	Short:        "Release a remote operation lease by exact ID",
	SilenceUsage: true,
	Long: `Release a remote operation lease by exact ID.

This command only releases leases whose current remote ID matches --id. It
refuses to release a non-expired lease unless --force is set.`,
	RunE: runStateLeaseRelease,
}

var stateServer string
var stateLeaseID string
var stateLeaseForce bool
var stateForgetNodeYes bool
var stateForgetNodeForce bool

var (
	syncStateCollectDeploymentHistories = collectStateDeploymentHistoriesWithPool
	syncStateRecoverFromMeshActual      = recoverAndSaveStateFromMeshActualWithPool
	syncStateRecoverFromRunningMesh     = recoverAndSaveStateFromRunningMeshWithPool
	collectStateStatusNodesForCommand   = collectStateStatusNodes
)

const stateStatusRequestTimeout = 10 * time.Second

func init() {
	rootCmd.AddCommand(stateCmd)
	stateCmd.AddCommand(statePullCmd)
	stateCmd.AddCommand(stateStatusCmd)
	stateCmd.AddCommand(stateRepairCmd)
	stateCmd.AddCommand(stateForgetNodeCmd)
	stateCmd.AddCommand(stateLeaseCmd)
	stateLeaseCmd.AddCommand(stateLeaseReleaseCmd)

	statePullCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Pull state from a specific server instead of the full mesh")

	stateStatusCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Check status for a specific server instead of the full mesh")

	stateRepairCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Prefer this server when acquiring the repair lease")
	stateForgetNodeCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Write to a specific reachable server instead of the full mesh")
	stateForgetNodeCmd.Flags().BoolVarP(&stateForgetNodeYes, "yes", "y", false, "Skip confirmation prompt")
	stateForgetNodeCmd.Flags().BoolVar(&stateForgetNodeForce, "force", false, "Allow forgetting a node that is still listed in the active environment")

	stateLeaseCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Show a specific server lease instead of the full mesh")
	stateLeaseReleaseCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Release a lease on a specific server instead of the full mesh")
	stateLeaseReleaseCmd.Flags().StringVar(&stateLeaseID, "id", "", "Exact remote lease ID to release")
	stateLeaseReleaseCmd.Flags().BoolVar(&stateLeaseForce, "force", false, "Release a matching non-expired lease")
}

func runStatePull(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	result, err := cliEngine().StatePull(cmd.Context(), engine.StatePullRequest{
		Config:      cfg,
		Environment: envName,
		Server:      stateServer,
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			histories, err := collectStatePullHistoriesForCommand(cfg, envName, stateServer)
			if err != nil {
				return "", nil, err
			}
			best, ok := bestDeploymentHistory(histories)
			if !ok {
				return "", nil, nil
			}
			return best.source, best.history, nil
		},
		SyncDeployments: func(deployments []*remotestate.DeploymentState) (int, error) {
			localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
			if err != nil {
				return 0, fmt.Errorf("failed to initialize local state: %w", err)
			}
			return syncRemoteDeploymentsToLocal(localMgr, deployments, envName)
		},
		RecoverFromMeshActual: func() (engine.StatePullRecoveryResult, error) {
			return recoverStatePullFromMeshActualForCommand(cfg, envName, stateServer)
		},
		RecoverFromRunningMesh: func() (engine.StatePullRecoveryResult, error) {
			return recoverStatePullFromRunningMeshForCommand(cfg, envName, stateServer)
		},
	})
	if result != nil && machineOutputEnabled() {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	if err != nil {
		return err
	}
	if machineOutputEnabled() {
		return nil
	}
	return renderStatePullResult(result)
}

func collectStatePullHistoriesForCommand(cfg *config.Config, envName string, requestedServer string) ([]stateHistoryCandidate, error) {
	if !machineOutputEnabled() {
		return collectStatePullHistories(cfg, envName, requestedServer)
	}
	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()
	return collectStateDeploymentHistories(cfg, envName, requestedServer, true)
}

func recoverStatePullFromMeshActualForCommand(cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	if !machineOutputEnabled() {
		return recoverAndSaveStateFromMeshActualResult(cfg, envName, requestedServer)
	}
	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()
	return recoverAndSaveStateFromMeshActualResult(cfg, envName, requestedServer)
}

func recoverStatePullFromRunningMeshForCommand(cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	if !machineOutputEnabled() {
		return recoverAndSaveStateFromRunningMeshResult(cfg, envName, requestedServer)
	}
	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()
	return recoverAndSaveStateFromRunningMeshResult(cfg, envName, requestedServer)
}

func renderStatePullResult(result *engine.StatePullResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	switch result.Status {
	case engine.StatePullStatusSyncedHistory:
		fmt.Printf("Selected deployment history from %s\n", result.SourceServer)
		fmt.Printf("Synced %d deployment(s) to local .tako directory\n", result.SyncedCount)
		if result.Latest != nil {
			displayID := result.Latest.DisplayID
			if displayID == "" {
				displayID = remotestate.FormatDeploymentID(result.Latest.ID)
			}
			fmt.Printf("\nLatest deployment:\n")
			fmt.Printf("  ID:      %s\n", displayID)
			fmt.Printf("  Status:  %s\n", result.Latest.Status)
			fmt.Printf("  Time:    %s (%s ago)\n", result.Latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(result.Latest.Timestamp)))
			fmt.Printf("  User:    %s\n", result.Latest.User)
			if result.Latest.Commit != "" {
				fmt.Printf("  Commit:  %s\n", result.Latest.Commit)
			}
		}
		fmt.Println("\nLocal state is now synchronized with remote.")
	case engine.StatePullStatusRecoveredActual:
		fmt.Println("No remote deployment history found, attempting recovery from mesh runtime state...")
		printStatePullRecovered(result)
	case engine.StatePullStatusRecoveredRunning:
		fmt.Println("No remote deployment history found, attempting recovery from mesh runtime state...")
		if verbose && result.MeshActualError != "" {
			fmt.Printf("Warning: mesh runtime state recovery failed: %s\n", result.MeshActualError)
		}
		fmt.Println("No mesh runtime state found, attempting recovery from running services across reachable nodes...")
		printStatePullRecovered(result)
	case engine.StatePullStatusNoneFound:
		fmt.Println("No remote deployment history found, attempting recovery from mesh runtime state...")
		if verbose && result.MeshActualError != "" {
			fmt.Printf("Warning: mesh runtime state recovery failed: %s\n", result.MeshActualError)
		}
		fmt.Println("No mesh runtime state found, attempting recovery from running services across reachable nodes...")
		if verbose && result.RunningMeshError != "" {
			fmt.Printf("Warning: running service recovery failed: %s\n", result.RunningMeshError)
		}
		fmt.Println("\nNo remote state or running services found.")
		fmt.Println("This project has not been deployed yet, or state was cleaned up.")
		fmt.Println("\nRun 'tako deploy' to create initial deployment.")
	}
	return nil
}

func printStatePullRecovered(result *engine.StatePullResult) {
	serviceCount := 0
	if result != nil && result.Recovered != nil {
		serviceCount = result.Recovered.ServiceCount
	}
	fmt.Printf("Recovered state from %d running service(s)\n", serviceCount)
}

func collectStatePullHistories(cfg *config.Config, envName string, requestedServer string) ([]stateHistoryCandidate, error) {
	return collectStateDeploymentHistories(cfg, envName, requestedServer, false)
}

func collectStateDeploymentHistories(cfg *config.Config, envName string, requestedServer string, quiet bool) ([]stateHistoryCandidate, error) {
	return collectStateDeploymentHistoriesWithPool(nil, cfg, envName, requestedServer, quiet)
}

func collectStateDeploymentHistoriesWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string, quiet bool) ([]stateHistoryCandidate, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	factory, closeRuntime, err := newStateRuntimeFactory(pool, cfg)
	if err != nil {
		return nil, err
	}
	defer closeRuntime()

	results := make([]stateHistoryReadResult, len(serverNames))
	resultCh := make(chan stateHistoryReadResult, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			result := stateHistoryReadResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
			}
			client, _, err := factory.Client(context.Background(), serverName)
			if err != nil {
				result.err = err
				resultCh <- result
				return
			}
			manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
			result.readAttempted = true
			result.history, result.err = manager.LoadHistory()
			resultCh <- result
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		results[result.index] = result
	}

	return stateHistoryCandidatesFromResults(results, quiet)
}

type stateHistoryReadResult struct {
	index         int
	serverName    string
	host          string
	history       *remotestate.DeploymentHistory
	readAttempted bool
	err           error
}

func stateHistoryCandidatesFromResults(results []stateHistoryReadResult, quiet bool) ([]stateHistoryCandidate, error) {
	histories := make([]stateHistoryCandidate, 0, len(results))
	readErrors := make([]string, 0)
	connectionErrors := make([]string, 0)
	reachable := 0
	for _, result := range results {
		if !quiet {
			fmt.Printf("Checking %s (%s)...\n", result.serverName, result.host)
		}
		if result.err != nil {
			if result.readAttempted {
				reachable++
			}
			if errors.Is(result.err, remotestate.ErrNotFound) {
				if verbose {
					fmt.Printf("No deployment history found on %s\n", result.serverName)
				}
				continue
			}
			if result.readAttempted {
				readErrors = append(readErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			} else {
				connectionErrors = append(connectionErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			}
			if !quiet || verbose {
				fmt.Fprintf(os.Stderr, "Warning: cannot read state from %s: %v\n", result.serverName, result.err)
			}
			continue
		}
		reachable++

		if !historyHasDeployments(result.history) {
			if verbose {
				fmt.Printf("No deployment history found on %s\n", result.serverName)
			}
			continue
		}
		histories = append(histories, stateHistoryCandidate{
			source:  result.serverName,
			history: result.history,
		})
		if !quiet {
			fmt.Printf("  history: %d deployment(s), freshness %s\n",
				deploymentHistoryCount(result.history),
				deploymentHistoryFreshness(result.history).Format(time.RFC3339),
			)
		}
	}
	if len(histories) == 0 && reachable == 0 && len(connectionErrors) > 0 {
		sort.Strings(connectionErrors)
		return nil, fmt.Errorf("failed to reach environment node(s): %s", strings.Join(connectionErrors, "; "))
	}
	if len(histories) == 0 && len(readErrors) > 0 {
		sort.Strings(readErrors)
		return nil, fmt.Errorf("failed to read deployment history from reachable node(s): %s", strings.Join(readErrors, "; "))
	}
	return histories, nil
}

func connectAndVerifyStateServer(serverName string, server config.ServerConfig) (*ssh.Client, error) {
	client, _, err := connectAndVerifyStateServerWithPool(nil, serverName, server)
	return client, err
}

func connectAndVerifyStateServerWithPool(pool *ssh.Pool, serverName string, server config.ServerConfig) (*ssh.Client, func(), error) {
	var client *ssh.Client
	var err error
	cleanup := func() {}
	if pool != nil {
		client, err = pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	} else {
		client, err = ssh.NewClientWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err == nil {
			cleanup = func() { _ = client.Close() }
		}
	}
	if err != nil {
		return nil, cleanup, fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	verifyOutput, verifyErr := client.Execute("echo 'tako-ok'")
	if verifyErr != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("SSH command verification failed: %v", verifyErr)
	}
	if strings.TrimSpace(verifyOutput) != "tako-ok" {
		cleanup()
		return nil, func() {}, fmt.Errorf("SSH command verification returned %q", strings.TrimSpace(verifyOutput))
	}
	return client, cleanup, nil
}

func newStateRuntimeFactory(pool *ssh.Pool, cfg *config.Config) (*nodeclient.Factory, func(), error) {
	ownedPool := false
	if pool == nil {
		pool = ssh.NewPool()
		ownedPool = true
	}
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocketFromConfig(cfg))
	if err != nil {
		if ownedPool {
			pool.CloseAll()
		}
		return nil, func() {}, err
	}
	cleanup := func() {
		factory.CloseIdleConnections()
		if ownedPool {
			pool.CloseAll()
		}
	}
	return factory, cleanup, nil
}

func statePullServerNames(cfg *config.Config, envName string, requestedServer string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, fmt.Errorf("no servers configured for environment %s", envName)
	}
	if requestedServer == "" {
		if authority, authorityErr := engine.AuthoritativeStateServer(cfg, envServers); authorityErr == nil {
			if server := cfg.Servers[authority]; server.ClusterID != "" && server.NodeID != "" {
				return []string{authority}, nil
			}
		}
	}
	if requestedServer == "" {
		return envServers, nil
	}
	if _, ok := cfg.Servers[requestedServer]; !ok {
		return nil, fmt.Errorf("server %s not found in configuration", requestedServer)
	}
	for _, serverName := range envServers {
		if serverName == requestedServer {
			return []string{requestedServer}, nil
		}
	}
	return nil, fmt.Errorf("server %s is not part of environment %s", requestedServer, envName)
}

func latestDeploymentByTimestamp(deployments []*remotestate.DeploymentState) *remotestate.DeploymentState {
	var latest *remotestate.DeploymentState
	for _, deployment := range deployments {
		if deployment == nil {
			continue
		}
		if latest == nil || deployment.Timestamp.After(latest.Timestamp) {
			latest = deployment
		}
	}
	return latest
}

func runStateStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	localInput := collectStateStatusLocal(cfg, envName)
	remoteNodes, err := collectStateStatusNodesForCommand(cfg, envName, stateServer)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	result, statusErr := cliEngine().StateStatus(ctx, engine.StateStatusRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      stateServer,
		Local:       localInput,
		Nodes:       engineStateStatusNodes(remoteNodes),
	})
	if result != nil && machineOutputEnabled() {
		if emitErr := emitResultDocument(result); emitErr != nil && statusErr == nil {
			statusErr = emitErr
		}
	}
	if machineOutputEnabled() {
		return statusErr
	}
	if result != nil {
		renderStateStatusResult(result, cfg)
	}
	return statusErr
}

func collectStateStatusLocal(cfg *config.Config, envName string) engine.StateStatusLocalInput {
	localPath := ".tako"
	input := engine.StateStatusLocalInput{Path: localPath}
	if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		input.Exists = true
		localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
		if err != nil {
			input.Error = err.Error()
			return input
		}
		currentDep, err := localMgr.GetCurrentDeployment()
		if err == nil && currentDep != nil {
			input.Current = currentDep
		}
	}
	return input
}

func stateSyncRecommendation(localExists bool, localCurrent *localstate.DeploymentState, bestHistory stateHistoryCandidate, hasRemoteHistory bool, unreachableCount int) []string {
	return engine.StateStatusSyncRecommendation(localExists, localCurrent, engine.StateStatusHistoryCandidate{Source: bestHistory.source, History: bestHistory.history}, hasRemoteHistory, unreachableCount)
}

func engineStateStatusNodes(nodes []stateStatusNode) []engine.StateStatusRemoteNodeInput {
	out := make([]engine.StateStatusRemoteNodeInput, 0, len(nodes))
	for _, node := range nodes {
		input := engine.StateStatusRemoteNodeInput{
			Name:       node.name,
			Host:       node.host,
			EnvNodes:   append([]string(nil), node.envNodes...),
			ConnectErr: node.connectErr,
			History:    node.history,
			HistoryErr: node.historyErr,
			Desired:    node.desired,
			DesiredErr: node.desiredErr,
			Actual:     node.actual,
			ActualErr:  node.actualErr,
			Lease:      node.lease,
			LeaseErr:   node.leaseErr,
		}
		if node.agent != nil {
			input.Agent = &engine.StateStatusAgentSummary{
				Runtime:   node.agent.Runtime,
				Version:   node.agent.Version,
				Hostname:  node.agent.Hostname,
				Socket:    node.agent.Socket,
				DataDir:   node.agent.DataDir,
				StartedAt: node.agent.StartedAt,
				Now:       node.agent.Now,
			}
		}
		input.AgentErr = node.agentErr
		if node.mesh != nil {
			input.Mesh = &engine.StateStatusMeshSummary{
				Interface:  node.mesh.Interface,
				Up:         node.mesh.Up,
				ListenPort: node.mesh.ListenPort,
				Peers:      node.mesh.Peers,
				PublicKey:  node.mesh.PublicKey,
			}
		}
		input.MeshErr = node.meshErr
		for _, candidate := range node.nodeActual {
			input.NodeActual = append(input.NodeActual, engine.StateStatusNodeActualCandidate{
				Source: candidate.source,
				Node:   candidate.node,
				Actual: candidate.actual,
			})
		}
		out = append(out, input)
	}
	return out
}

func renderStateStatusResult(result *engine.StateStatusResult, cfg *config.Config) {
	fmt.Printf("Project: %s\n", result.Project)
	fmt.Printf("Environment: %s\n\n", result.Environment)

	fmt.Println("=== Local State ===")
	renderStateStatusLocal(result.Local)
	fmt.Println()

	fmt.Printf("=== %s ===\n", result.Remote.Title)
	renderStateStatusNodes(result.Remote.Nodes, cfg)
	if len(result.Remote.UnreachableGuidance) > 0 {
		fmt.Println()
		for _, line := range result.Remote.UnreachableGuidance {
			fmt.Println(line)
		}
	}

	fmt.Println()
	renderBestKnownState(result.BestKnown)
	fmt.Println()

	fmt.Println("=== Sync Status ===")
	for _, line := range result.Sync.Recommendations {
		fmt.Println(line)
	}
}

func renderStateStatusLocal(local engine.StateStatusLocalSummary) {
	if local.Exists {
		fmt.Printf("Directory: %s (exists)\n", local.Path)
		if local.Error != "" {
			fmt.Printf("Status: Error loading state: %s\n", local.Error)
			return
		}
		if local.LastDeployment == nil {
			fmt.Println("Status: No deployments recorded locally")
			return
		}
		dep := local.LastDeployment
		fmt.Printf("Last deployment:\n")
		fmt.Printf("  ID:      %s\n", dep.ID)
		fmt.Printf("  Time:    %s (%s ago)\n", dep.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(dep.Timestamp)))
		fmt.Printf("  Status:  %s\n", dep.Status)
		if dep.Commit != "" {
			fmt.Printf("  Commit:  %s\n", dep.Commit[:min(7, len(dep.Commit))])
		}
		return
	}
	fmt.Printf("Directory: %s (missing)\n", local.Path)
	fmt.Println("Status: No local state")
}

func renderStateStatusNodes(nodes []engine.StateStatusNodeResult, cfg *config.Config) {
	fmt.Printf("Nodes: %d configured, %d reachable\n", len(nodes), engineStateStatusReachableCount(nodes))
	for _, node := range nodes {
		fmt.Printf("\nNode: %s (%s)\n", node.Name, node.Host)
		if !node.Reachable {
			fmt.Printf("Status: unreachable - %s\n", node.Error)
			continue
		}
		fmt.Println("Status: reachable")
		renderStateStatusAgent(node.Agent)
		renderStateStatusMesh(node.Mesh, cfg)
		renderStateStatusHistory(node.History)
		renderStateStatusDesired(node.Desired)
		renderStateStatusActual(node.Actual)
		renderStateStatusLease(node.Lease)
	}
}

func engineStateStatusReachableCount(nodes []engine.StateStatusNodeResult) int {
	reachable := 0
	for _, node := range nodes {
		if node.Reachable {
			reachable++
		}
	}
	return reachable
}

func renderStateStatusAgent(status *engine.StateStatusAgentSummary) {
	if status == nil || status.Error != "" {
		errText := "<nil>"
		if status != nil && status.Error != "" {
			errText = status.Error
		}
		fmt.Printf("Agent: unavailable - %s\n", errText)
		return
	}
	fmt.Printf("Agent: %s %s on %s\n", status.Runtime, status.Version, status.Hostname)
	if !status.StartedAt.IsZero() {
		fmt.Printf("  Started: %s (%s ago)\n", status.StartedAt.Format(time.RFC3339), formatStateDuration(status.Now.Sub(status.StartedAt)))
	}
}

func renderStateStatusMesh(status *engine.StateStatusMeshSummary, cfg *config.Config) {
	if cfg.Mesh == nil {
		return
	}
	if status == nil || status.Error != "" {
		errText := "<nil>"
		if status != nil && status.Error != "" {
			errText = status.Error
		}
		fmt.Printf("Mesh: unavailable - %s\n", errText)
		return
	}
	if !status.Up {
		fmt.Printf("Mesh: %s is down\n", status.Interface)
		return
	}
	fmt.Printf("Mesh: %s is up, peers %d\n", status.Interface, status.Peers)
}

func renderStateStatusHistory(history engine.StateStatusHistorySummary) {
	if !history.Recorded {
		if history.Missing {
			fmt.Println("History: not recorded")
		} else if history.Error != "" {
			fmt.Printf("History: unavailable - %s\n", history.Error)
		} else {
			fmt.Println("History: empty")
		}
		return
	}
	fmt.Printf("History: %d deployment(s), freshness %s\n", history.Count, history.Freshness.Format(time.RFC3339))
	if history.Latest != nil {
		fmt.Printf("  Latest: %s, %s, %s (%s ago)\n", history.Latest.DisplayID, history.Latest.Status, history.Latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(history.Latest.Timestamp)))
	}
}

func renderStateStatusDesired(desired engine.StateStatusDesiredSummary) {
	if !desired.Recorded {
		if desired.Error != "" {
			fmt.Printf("Desired: error - %s\n", desired.Error)
		} else {
			fmt.Println("Desired: not recorded")
		}
		return
	}
	fmt.Printf("Desired: %s, %d service(s), freshness %s\n", desired.RevisionID, desired.ServiceCount, desired.Freshness.Format(time.RFC3339))
}

func renderStateStatusActual(actual engine.StateStatusActualSummary) {
	if !actual.Recorded {
		if actual.Error != "" {
			fmt.Printf("Actual: error - %s\n", actual.Error)
		} else {
			fmt.Println("Actual: not recorded")
		}
	} else {
		fmt.Printf("Actual: %d service(s), freshness %s\n", actual.ServiceCount, actual.Freshness.Format(time.RFC3339))
	}
	if actual.NodeActualCount > 0 {
		fmt.Printf("Node actual: %d snapshot(s)\n", actual.NodeActualCount)
	}
}

func renderStateStatusLease(lease engine.StateStatusLeaseSummary) {
	if lease.Error != "" {
		fmt.Printf("Lease: error - %s\n", lease.Error)
		return
	}
	if lease.Lease == nil {
		fmt.Println("Lease: free")
		return
	}
	fmt.Printf("Lease: held by %s\n", lease.Lease.Who)
	fmt.Printf("  ID:        %s\n", lease.Lease.ID)
	fmt.Printf("  Operation: %s\n", lease.Lease.Operation)
	fmt.Printf("  Created:   %s (%s ago)\n", lease.Lease.CreatedAt.Format(time.RFC3339), formatStateDuration(time.Since(lease.Lease.CreatedAt)))
	fmt.Printf("  Expires:   %s (in %s)\n", lease.Lease.ExpiresAt.Format(time.RFC3339), time.Until(lease.Lease.ExpiresAt).Round(time.Second))
}

func renderBestKnownState(best engine.StateStatusBestKnown) {
	fmt.Println("=== Best Known State ===")
	if best.History != nil {
		fmt.Printf("Deployment history: %s (%d deployment(s), freshness %s)\n", best.History.Source, best.History.Count, best.History.Freshness.Format(time.RFC3339))
		if best.History.Latest != nil {
			fmt.Printf("  Latest: %s, %s, %s (%s ago)\n", best.History.Latest.DisplayID, best.History.Latest.Status, best.History.Latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(best.History.Latest.Timestamp)))
		}
	} else {
		fmt.Println("Deployment history: not found on reachable nodes")
	}
	if best.Desired != nil {
		fmt.Printf("Desired runtime: %s (%s, freshness %s)\n", best.Desired.Source, best.Desired.RevisionID, best.Desired.Freshness.Format(time.RFC3339))
	} else {
		fmt.Println("Desired runtime: not found on reachable nodes")
	}
	if best.Actual != nil {
		fmt.Printf("Actual runtime: %s (%d service(s), freshness %s)\n", best.Actual.Source, best.Actual.ServiceCount, best.Actual.Freshness.Format(time.RFC3339))
	} else {
		fmt.Println("Actual runtime: not found on reachable nodes")
	}
	if len(best.NodeActual) > 0 {
		fmt.Printf("Node actual: %d node(s)\n", len(best.NodeActual))
		for _, node := range best.NodeActual {
			fmt.Printf("  %s: %s, freshness %s\n", node.Node, node.Source, node.Freshness.Format(time.RFC3339))
		}
	}
}

func deploymentsEquivalentExceptID(localCurrent *localstate.DeploymentState, remoteLatest *remotestate.DeploymentState) bool {
	if localCurrent == nil || remoteLatest == nil {
		return false
	}
	if !deploymentTimestampsEquivalent(localCurrent.Timestamp, remoteLatest.Timestamp) {
		return false
	}
	if !deploymentStatusesEquivalent(localCurrent.Status, remoteLatest.Status) {
		return false
	}
	if !deploymentCommitsEquivalent(localCurrent.GitCommit, remoteLatest.GitCommit, remoteLatest.GitCommitShort) {
		return false
	}
	return deploymentServicesEquivalent(localCurrent.Services, remoteLatest.Services)
}

func deploymentTimestampsEquivalent(localTime time.Time, remoteTime time.Time) bool {
	if localTime.IsZero() || remoteTime.IsZero() {
		return false
	}
	return localTime.UTC().Truncate(time.Second).Equal(remoteTime.UTC().Truncate(time.Second))
}

func deploymentStatusesEquivalent(localStatus string, remoteStatus remotestate.DeploymentStatus) bool {
	return strings.EqualFold(strings.TrimSpace(localStatus), strings.TrimSpace(string(remoteStatus)))
}

func deploymentCommitsEquivalent(localCommit string, remoteCommit string, remoteShort string) bool {
	localCommit = strings.TrimSpace(localCommit)
	remoteCommit = strings.TrimSpace(remoteCommit)
	remoteShort = strings.TrimSpace(remoteShort)
	if localCommit == "" || (remoteCommit == "" && remoteShort == "") {
		return true
	}
	if remoteCommit != "" && localCommit == remoteCommit {
		return true
	}
	if remoteShort != "" && strings.HasPrefix(localCommit, remoteShort) {
		return true
	}
	if remoteCommit != "" && strings.HasPrefix(remoteCommit, localCommit) {
		return true
	}
	return false
}

func deploymentServicesEquivalent(localServices map[string]*localstate.ServiceDeploy, remoteServices map[string]remotestate.ServiceState) bool {
	if len(localServices) == 0 || len(remoteServices) == 0 {
		return true
	}
	if len(localServices) != len(remoteServices) {
		return false
	}
	for name, localService := range localServices {
		remoteService, ok := remoteServices[name]
		if !ok {
			return false
		}
		if localService == nil {
			return false
		}
		if strings.TrimSpace(localService.Image) != "" && strings.TrimSpace(remoteService.Image) != "" && localService.Image != remoteService.Image {
			return false
		}
		if localService.Replicas != 0 && remoteService.Replicas != 0 && localService.Replicas != remoteService.Replicas {
			return false
		}
	}
	return true
}

type stateLeaseNode struct {
	name    string
	host    string
	manager remoteLeaseManager
	lease   *remotestate.LeaseInfo
	err     error
}

func runStateLease(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	result, err := cliEngine().StateLease(cmd.Context(), engine.StateLeaseRequest{
		Config:      cfg,
		Environment: envName,
		Server:      stateServer,
	})
	if err != nil {
		return err
	}
	return renderStateLeaseResult(result)
}

func runStateLeaseRelease(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	result, err := cliEngine().ReleaseStateLease(cmd.Context(), engine.StateLeaseReleaseRequest{
		Config:      cfg,
		Environment: envName,
		Server:      stateServer,
		ID:          stateLeaseID,
		Force:       stateLeaseForce,
	})
	if result != nil && machineOutputEnabled() {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	if err != nil {
		return err
	}
	if machineOutputEnabled() {
		return nil
	}
	return renderStateLeaseReleaseResult(result)
}

func renderStateLeaseResult(result *engine.StateLeaseResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	fmt.Printf("Project: %s\n", result.Project)
	fmt.Printf("Environment: %s\n\n", result.Environment)
	printStateLeaseNodes(stateLeaseNodesFromEngine(result.Nodes))
	return nil
}

func renderStateLeaseReleaseResult(result *engine.StateLeaseReleaseResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	fmt.Printf("Released lease %s on %d node(s): %s\n", result.LeaseID, result.ReleasedCount, strings.Join(result.Released, ", "))
	return nil
}

func collectStateLeaseNodes(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) ([]stateLeaseNode, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	nodes, err := engine.CollectStateLeaseNodes(context.Background(), pool, cfg, envName, serverNames)
	if err != nil {
		return nil, err
	}
	return stateLeaseNodesFromEngine(nodes), nil
}

func printStateLeaseNodes(nodes []stateLeaseNode) {
	for _, node := range nodes {
		fmt.Printf("Node: %s (%s)\n", node.name, node.host)
		printStateStatusLease(node.lease, node.err)
		fmt.Println()
	}
}

func stateLeaseNodesFromEngine(nodes []engine.StateLeaseNodeResult) []stateLeaseNode {
	out := make([]stateLeaseNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, stateLeaseNode{
			name:    node.Name,
			host:    node.Host,
			manager: node.Manager,
			lease:   node.Lease,
			err:     node.Err,
		})
	}
	return out
}

func engineStateLeaseNodes(nodes []stateLeaseNode) []engine.StateLeaseNodeResult {
	out := make([]engine.StateLeaseNodeResult, 0, len(nodes))
	for _, node := range nodes {
		result := engine.StateLeaseNodeResult{
			Name:    node.name,
			Host:    node.host,
			Manager: node.manager,
			Lease:   node.lease,
			Err:     node.err,
		}
		if node.err != nil {
			result.Error = node.err.Error()
		}
		out = append(out, result)
	}
	return out
}

func releaseStateLeaseByID(nodes []stateLeaseNode, leaseID string, force bool, now time.Time) ([]string, error) {
	return engine.ReleaseStateLeaseByID(engineStateLeaseNodes(nodes), leaseID, force, now)
}

func runStateRepair(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)
	if !machineOutputEnabled() {
		fmt.Printf("Project: %s\n", cfg.Project.Name)
		fmt.Printf("Environment: %s\n\n", envName)
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	repair, err := collectStateRepairNodesForCommand(pool, cfg, envName, stateServer)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}

	var repairLeases []stateRepairLease
	result, repairErr := cliEngine().StateRepair(ctx, engine.StateRepairRequest{
		Config:      cfg,
		Environment: envName,
		Server:      stateServer,
		Nodes:       engineStateRepairNodes(repair.nodes),
		Histories:   engineStateRepairHistories(repair.histories),
		Desired:     engineStateRepairDesired(repair.desired),
		Actual:      engineStateRepairActual(repair.actual),
		NodeActual:  engineStateRepairNodeActual(repair.nodeActual),
		BeforeWrite: func(ctx context.Context, result *engine.StateRepairResult) error {
			if !machineOutputEnabled() {
				renderStateRepairSources(result)
			}
			leases, err := acquireStateRepairLeasesForCommand(repair.nodes, envName)
			if err != nil {
				return err
			}
			repairLeases = leases
			if verbose && !machineOutputEnabled() {
				fmt.Printf("Acquired state repair leases: %s\n", stateRepairLeaseSummary(repairLeases))
			}
			return nil
		},
	})
	if len(repairLeases) > 0 {
		defer releaseStateRepairLeases(repairLeases, verbose)
	}

	if result != nil && !machineOutputEnabled() {
		renderStateRepairWarnings(result)
	}
	if repairErr == nil && result != nil && result.Sources.HasHistory {
		synced, syncErr := syncStateRepairHistoryToLocalForCommand(cfg, envName, result.SelectedHistory)
		if syncErr != nil {
			repairErr = syncErr
			result.Status = engine.StateRepairStatusFailed
			result.Error = syncErr.Error()
			result.Local.Status = engine.StateRepairLocalSyncStatusFailed
			result.Local.Error = syncErr.Error()
		} else {
			result.Local.Status = engine.StateRepairLocalSyncStatusSynced
			result.Local.Count = synced
			if !machineOutputEnabled() {
				fmt.Printf("Synced %d deployment(s) to local .tako directory\n", synced)
			}
		}
	}
	if result != nil && machineOutputEnabled() {
		if emitErr := emitResultDocument(result); emitErr != nil && repairErr == nil {
			repairErr = emitErr
		}
	}
	if repairErr != nil {
		return repairErr
	}
	if result != nil && !machineOutputEnabled() {
		renderStateRepairWriteSummary(result)
	}
	return nil
}

func runStateForgetNode(cmd *cobra.Command, args []string) error {
	nodeName := strings.TrimSpace(args[0])
	if err := validateStateForgetNodeName(nodeName); err != nil {
		return err
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	if stateNodeNameInList(envServers, nodeName) && !stateForgetNodeForce {
		return fmt.Errorf("node %s is still listed in environment %s; remove it from tako.yaml first or rerun with --force", nodeName, envName)
	}

	confirmationReason := fmt.Sprintf("forget-node mutates replicated runtime state for node %q", nodeName)
	if !stateForgetNodeYes {
		if machineOutputEnabled() {
			if err := emitResultDocument(newStateForgetNodeConfirmationRequiredDocument(confirmationReason, cfg.Project.Name, envName, nodeName)); err != nil {
				return err
			}
			return &engine.ConfirmationRequiredError{Reason: confirmationReason}
		}
		confirmed, err := confirmDeployAction(
			fmt.Sprintf("Forget node %q from replicated runtime state for %s? (y/N): ", nodeName, envName),
			confirmationReason,
		)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("State cleanup cancelled.")
			return nil
		}
	}

	if !machineOutputEnabled() {
		fmt.Printf("Project: %s\n", cfg.Project.Name)
		fmt.Printf("Environment: %s\n", envName)
		fmt.Printf("Retired node: %s\n\n", nodeName)
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	repair, err := collectStateRepairNodesWithPool(pool, cfg, envName, stateServer)
	if err != nil {
		return err
	}
	if len(repair.nodes) == 0 {
		return fmt.Errorf("no reachable environment nodes found")
	}

	repairLeases, err := acquireStateForgetNodeLeases(repair.nodes, envName)
	if err != nil {
		return err
	}
	defer releaseStateRepairLeases(repairLeases, verbose)
	if verbose && !machineOutputEnabled() {
		fmt.Printf("Acquired state cleanup leases: %s\n", stateRepairLeaseSummary(repairLeases))
	}

	result, err := cliEngine().StateForgetNode(cmd.Context(), engine.StateForgetNodeRequest{
		Config:      cfg,
		Environment: envName,
		Server:      stateServer,
		NodeName:    nodeName,
		Force:       stateForgetNodeForce,
		Nodes:       stateForgetNodeEngineNodes(repair.nodes),
	})
	if result != nil && machineOutputEnabled() {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	if err != nil {
		return err
	}
	if machineOutputEnabled() {
		return nil
	}
	return renderStateForgetNodeResult(result)
}

func forgetNodeFromRepairNodes(nodes []stateRepairNode, project string, envName string, nodeName string) ([]stateForgetNodeResult, error) {
	results, err := engine.ForgetNodeFromRuntimeNodes(stateForgetNodeEngineNodes(nodes), project, envName, nodeName)
	return stateForgetNodeResultsFromEngine(results), err
}

func forgetNodeOnRepairNode(node stateRepairNode, project string, envName string, nodeName string) stateForgetNodeResult {
	return stateForgetNodeResultFromEngine(engine.ForgetNodeOnRuntimeNode(context.Background(), stateForgetNodeEngineNode(node), project, envName, nodeName))
}

func stateForgetNodeEngineNode(node stateRepairNode) engine.StateForgetNodeNode {
	return engine.StateForgetNodeNode{Name: node.name, Runtime: node.runtime}
}

func stateForgetNodeEngineNodes(nodes []stateRepairNode) []engine.StateForgetNodeNode {
	out := make([]engine.StateForgetNodeNode, len(nodes))
	for i, node := range nodes {
		out[i] = stateForgetNodeEngineNode(node)
	}
	return out
}

func stateForgetNodeResultFromEngine(result engine.StateForgetNodeNodeResult) stateForgetNodeResult {
	return stateForgetNodeResult{
		nodeName:          result.Name,
		nodeActualExisted: result.NodeActualExisted,
		aggregatePruned:   result.AggregatePruned,
		warnings:          append([]string(nil), result.Warnings...),
		err:               result.Err,
	}
}

func stateForgetNodeResultsFromEngine(results []engine.StateForgetNodeNodeResult) []stateForgetNodeResult {
	out := make([]stateForgetNodeResult, len(results))
	for i, result := range results {
		out[i] = stateForgetNodeResultFromEngine(result)
	}
	return out
}

func actualSnapshotWithoutNode(snapshot *takodstate.ActualSnapshot, nodeName string) (*takodstate.ActualSnapshot, bool) {
	return engine.ActualSnapshotWithoutNode(snapshot, nodeName)
}

func removeStateNodeName(nodes []string, nodeName string) ([]string, bool) {
	return engine.RemoveStateNodeName(nodes, nodeName)
}

func removeActualEmbeddedNode(nodes map[string]takodstate.ActualNodeSnapshot, nodeName string) (map[string]takodstate.ActualNodeSnapshot, bool) {
	return engine.RemoveActualEmbeddedNode(nodes, nodeName)
}

func copyActualServices(services map[string]takodstate.ActualService) map[string]takodstate.ActualService {
	return engine.CopyActualServices(services)
}

func copyActualNodeSnapshots(nodes map[string]takodstate.ActualNodeSnapshot) map[string]takodstate.ActualNodeSnapshot {
	return engine.CopyActualNodeSnapshots(nodes)
}

func validateStateForgetNodeName(nodeName string) error {
	return engine.ValidateStateForgetNodeName(nodeName)
}

func renderStateForgetNodeResult(result *engine.StateForgetNodeResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	printStateForgetNodeSummary(result.RetiredNode, stateForgetNodeResultsFromEngine(result.Nodes))
	return nil
}

func printStateForgetNodeSummary(nodeName string, results []stateForgetNodeResult) {
	pruned := 0
	existed := 0
	for _, result := range results {
		if result.aggregatePruned {
			pruned++
		}
		if result.nodeActualExisted {
			existed++
		}
	}
	fmt.Printf("\nForgot node %s from replicated state on %d reachable node(s).\n", nodeName, len(results))
	fmt.Printf("Standalone node snapshot existed on %d node(s).\n", existed)
	fmt.Printf("Aggregate actual state pruned on %d node(s).\n", pruned)
	for _, result := range results {
		for _, warning := range result.warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s: %s\n", result.nodeName, warning)
		}
	}
	fmt.Println("\nNext:")
	fmt.Println("  tako state status")
	fmt.Println("  tako deploy --yes")
}

type takodRemoteStatus struct {
	Runtime   string    `json:"runtime"`
	Version   string    `json:"version"`
	Hostname  string    `json:"hostname"`
	Socket    string    `json:"socket"`
	DataDir   string    `json:"dataDir"`
	StartedAt time.Time `json:"startedAt"`
	Now       time.Time `json:"now"`
}

func printTakodAgentStatus(client any, cfg *config.Config) {
	output, err := takodclient.RequestJSON(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		"/v1/status",
		nil,
	)
	if err != nil || strings.TrimSpace(output) == "" {
		fmt.Println("Agent: not running")
		return
	}

	var status takodRemoteStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		fmt.Printf("Agent: running but returned unreadable status - %v\n", err)
		return
	}

	fmt.Printf("Agent: %s %s on %s\n", status.Runtime, status.Version, status.Hostname)
	fmt.Printf("  Started: %s (%s ago)\n", status.StartedAt.Format(time.RFC3339), formatStateDuration(status.Now.Sub(status.StartedAt)))
}

func printMeshRuntimeStatus(client any, cfg *config.Config) {
	if cfg.Mesh == nil {
		return
	}

	output, err := takodclient.RequestJSON(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		"/v1/mesh/status?interface="+url.QueryEscape(cfg.Mesh.Interface),
		nil,
	)
	if err != nil {
		fmt.Printf("Mesh: Error reading WireGuard status - %v\n", err)
		return
	}
	var status mesh.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		fmt.Printf("Mesh: Error parsing WireGuard status - %v\n", err)
		return
	}
	if !status.Up {
		fmt.Printf("Mesh: %s is down\n", status.Interface)
		return
	}

	publicKey := status.PublicKey
	if len(publicKey) > 16 {
		publicKey = publicKey[:16] + "..."
	}
	fmt.Printf("Mesh: %s is up, listen port %s, peers %d\n", status.Interface, status.ListenPort, status.Peers)
	if publicKey != "" {
		fmt.Printf("  Public key: %s\n", publicKey)
	}
}

type stateStatusNode struct {
	name       string
	host       string
	envNodes   []string
	connectErr error

	history    *remotestate.DeploymentHistory
	historyErr error
	desired    *takodstate.DesiredRevision
	desiredErr error
	actual     *takodstate.ActualSnapshot
	actualErr  error
	nodeActual []stateNodeActualCandidate

	agent    *takodRemoteStatus
	agentErr error
	mesh     *mesh.Status
	meshErr  error
	lease    *remotestate.LeaseInfo
	leaseErr error
}

func collectStateStatusNodes(cfg *config.Config, envName string, requestedServer string) ([]stateStatusNode, error) {
	return collectStateStatusNodesWithPool(nil, cfg, envName, requestedServer)
}

func collectStateStatusNodesWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) ([]stateStatusNode, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	factory, closeRuntime, err := newStateRuntimeFactory(pool, cfg)
	if err != nil {
		return nil, err
	}
	defer closeRuntime()
	envServerNames, err := statePullServerNames(cfg, envName, "")
	if err != nil {
		return nil, err
	}

	nodes := make([]stateStatusNode, len(serverNames))
	resultCh := make(chan struct {
		index int
		node  stateStatusNode
	}, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			resultCh <- struct {
				index int
				node  stateStatusNode
			}{
				index: index,
				node:  collectStateStatusNode(factory, cfg, envName, serverName, server, envServerNames),
			}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		nodes[result.index] = result.node
	}
	return nodes, nil
}

func collectStateStatusNode(factory *nodeclient.Factory, cfg *config.Config, envName string, serverName string, server config.ServerConfig, envServerNames []string) stateStatusNode {
	node := stateStatusNode{
		name:     serverName,
		host:     server.Host,
		envNodes: append([]string(nil), envServerNames...),
	}

	client, _, err := factory.Client(context.Background(), serverName)
	if err != nil {
		node.connectErr = err
		return node
	}

	manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg)).
		WithRequestTimeout(stateStatusRequestTimeout)
	node.history, node.historyErr = manager.LoadHistory()

	runtime := takodstate.NewManager(client, cfg, envName).
		WithRequestTimeout(stateStatusRequestTimeout)
	node.desired, node.desiredErr = runtime.ReadDesired()
	node.actual, node.actualErr = runtime.ReadActual()
	for _, actualNodeName := range envServerNames {
		nodeActual, err := runtime.ReadNodeActual(actualNodeName)
		if err == nil && nodeActualSnapshotRepairable(nodeActual, actualNodeName) {
			node.nodeActual = append(node.nodeActual, stateNodeActualCandidate{
				source: serverName,
				node:   actualNodeName,
				actual: nodeActual,
			})
		}
	}

	node.agent, node.agentErr = readTakodAgentStatus(client, cfg)
	if cfg.Mesh != nil {
		node.mesh, node.meshErr = readMeshRuntimeStatus(client, cfg)
	}
	node.lease, node.leaseErr = manager.ReadLease()
	return node
}

func stateStatusCandidates(nodes []stateStatusNode) ([]stateHistoryCandidate, []stateDesiredCandidate, []stateActualCandidate, []stateNodeActualCandidate) {
	histories := make([]stateHistoryCandidate, 0, len(nodes))
	desired := make([]stateDesiredCandidate, 0, len(nodes))
	actual := make([]stateActualCandidate, 0, len(nodes))
	nodeActual := make([]stateNodeActualCandidate, 0, len(nodes))
	configuredNodes := stateStatusConfiguredNodeSet(nodes)

	for _, node := range nodes {
		if historyHasDeployments(node.history) {
			histories = append(histories, stateHistoryCandidate{
				source:  node.name,
				history: node.history,
			})
		}
		if desiredRevisionRepairable(node.desired) {
			desired = append(desired, stateDesiredCandidate{
				source:  node.name,
				desired: node.desired,
			})
		}
		if actualSnapshotRepairable(node.actual) {
			actual = append(actual, stateActualCandidate{
				source: node.name,
				actual: node.actual,
			})
			for nodeName, embedded := range node.actual.Nodes {
				if !stateNodeConfigured(configuredNodes, nodeName) {
					continue
				}
				nodeActual = append(nodeActual, stateNodeActualCandidate{
					source: node.name + " aggregate",
					node:   nodeName,
					actual: actualSnapshotFromEmbeddedNode(node.actual.Project, node.actual.Environment, embedded),
				})
			}
		}
		nodeActual = append(nodeActual, node.nodeActual...)
	}
	return histories, desired, actual, nodeActual
}

func stateStatusConfiguredNodeSet(nodes []stateStatusNode) map[string]struct{} {
	configured := make(map[string]struct{})
	for _, node := range nodes {
		for _, nodeName := range node.envNodes {
			if nodeName != "" {
				configured[nodeName] = struct{}{}
			}
		}
	}
	if len(configured) > 0 {
		return configured
	}
	for _, node := range nodes {
		if node.name != "" {
			configured[node.name] = struct{}{}
		}
	}
	return configured
}

func stateNodeConfigured(configured map[string]struct{}, nodeName string) bool {
	if len(configured) == 0 {
		return true
	}
	_, ok := configured[nodeName]
	return ok
}

func bestStateStatusActual(project string, envName string, actualCandidates []stateActualCandidate, nodeActualCandidates []stateNodeActualCandidate) (stateActualCandidate, bool, map[string]stateNodeActualCandidate) {
	bestActual, hasActual := bestActualSnapshot(actualCandidates)
	bestNodeActual := bestNodeActualSnapshots(nodeActualCandidates)
	if hasActual && len(bestNodeActual) > 0 {
		bestActual.actual = actualSnapshotWithNodeSnapshots(bestActual.actual, bestNodeActual)
	} else if !hasActual && len(bestNodeActual) > 0 {
		bestActual = stateActualCandidate{
			source: "node actual snapshots",
			actual: aggregateActualSnapshotFromNodeSnapshots(project, envName, bestNodeActual),
		}
		hasActual = actualSnapshotRepairable(bestActual.actual)
	}
	return bestActual, hasActual, bestNodeActual
}

func printStateStatusNodes(nodes []stateStatusNode, cfg *config.Config) {
	fmt.Printf("Nodes: %d configured, %d reachable\n", len(nodes), stateStatusReachableCount(nodes))

	for _, node := range nodes {
		fmt.Printf("\nNode: %s (%s)\n", node.name, node.host)
		if node.connectErr != nil {
			fmt.Printf("Status: unreachable - %v\n", node.connectErr)
			continue
		}

		fmt.Println("Status: reachable")
		printStateStatusAgent(node.agent, node.agentErr)
		printStateStatusMesh(node.mesh, node.meshErr, cfg)
		printStateStatusHistory(node.history, node.historyErr)
		printStateStatusDesired(node.desired, node.desiredErr)
		printStateStatusActual(node.actual, node.actualErr, node.nodeActual)
		printStateStatusLease(node.lease, node.leaseErr)
	}
}

func printStateStatusUnreachableGuidance(nodes []stateStatusNode) {
	lines := stateStatusUnreachableGuidance(nodes)
	if len(lines) == 0 {
		return
	}
	fmt.Println()
	for _, line := range lines {
		fmt.Println(line)
	}
}

func stateStatusUnreachableGuidance(nodes []stateStatusNode) []string {
	names := make([]string, 0)
	for _, node := range nodes {
		if node.connectErr != nil {
			names = append(names, node.name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	nodeList := strings.Join(names, ", ")
	if len(names) == 1 {
		return []string{
			fmt.Sprintf("Unreachable node: %s", nodeList),
			fmt.Sprintf("  Destroyed node: remove %s from tako.yaml, then run 'tako state forget-node %s --yes' and 'tako deploy --yes'.", names[0], names[0]),
			fmt.Sprintf("  Rebuilt same-name node: keep %s in tako.yaml, then run 'tako setup --server %s', 'tako upgrade servers --server %s', 'tako state repair', and 'tako deploy --yes'.", names[0], names[0], names[0]),
		}
	}
	return []string{
		fmt.Sprintf("Unreachable nodes: %s", nodeList),
		"  Destroyed nodes: remove them from tako.yaml, then run 'tako state forget-node <node> --yes' for each removed node and 'tako deploy --yes'.",
		"  Rebuilt same-name nodes: keep them in tako.yaml, then run 'tako setup --server <node>', 'tako upgrade servers --server <node>', 'tako state repair', and 'tako deploy --yes'.",
	}
}

func stateStatusReachableCount(nodes []stateStatusNode) int {
	reachable := 0
	for _, node := range nodes {
		if node.connectErr == nil {
			reachable++
		}
	}
	return reachable
}

func stateStatusUnreachableCount(nodes []stateStatusNode) int {
	return len(nodes) - stateStatusReachableCount(nodes)
}

func stateStatusNoReachableError(envName string, nodes []stateStatusNode) error {
	details := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.connectErr != nil {
			details = append(details, fmt.Sprintf("%s: %v", node.name, node.connectErr))
		}
	}
	sort.Strings(details)
	message := fmt.Sprintf("no reachable environment nodes for %s; deploy will fail closed until SSH/network is restored or the environment config is updated", envName)
	if len(details) == 0 {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %s", message, strings.Join(details, "; "))
}

func printStateStatusAgent(status *takodRemoteStatus, err error) {
	if err != nil || status == nil {
		fmt.Printf("Agent: unavailable - %v\n", err)
		return
	}
	fmt.Printf("Agent: %s %s on %s\n", status.Runtime, status.Version, status.Hostname)
	if !status.StartedAt.IsZero() {
		fmt.Printf("  Started: %s (%s ago)\n", status.StartedAt.Format(time.RFC3339), formatStateDuration(status.Now.Sub(status.StartedAt)))
	}
}

func printStateStatusMesh(status *mesh.Status, err error, cfg *config.Config) {
	if cfg.Mesh == nil {
		return
	}
	if err != nil || status == nil {
		fmt.Printf("Mesh: unavailable - %v\n", err)
		return
	}
	if !status.Up {
		fmt.Printf("Mesh: %s is down\n", status.Interface)
		return
	}
	fmt.Printf("Mesh: %s is up, peers %d\n", status.Interface, status.Peers)
}

func printStateStatusHistory(history *remotestate.DeploymentHistory, err error) {
	if !historyHasDeployments(history) {
		if errors.Is(err, remotestate.ErrNotFound) {
			fmt.Println("History: not recorded")
		} else if err != nil {
			fmt.Printf("History: unavailable - %v\n", err)
		} else {
			fmt.Println("History: empty")
		}
		return
	}
	latest := latestDeploymentByTimestamp(history.Deployments)
	fmt.Printf("History: %d deployment(s), freshness %s\n",
		deploymentHistoryCount(history),
		deploymentHistoryFreshness(history).Format(time.RFC3339),
	)
	if latest != nil {
		fmt.Printf("  Latest: %s, %s, %s (%s ago)\n",
			remotestate.FormatDeploymentID(latest.ID),
			latest.Status,
			latest.Timestamp.Format(time.RFC3339),
			formatStateDuration(time.Since(latest.Timestamp)),
		)
	}
}

func printStateStatusDesired(desired *takodstate.DesiredRevision, err error) {
	if !desiredRevisionRepairable(desired) {
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Desired: error - %v\n", err)
		} else {
			fmt.Println("Desired: not recorded")
		}
		return
	}
	fmt.Printf("Desired: %s, %d service(s), freshness %s\n",
		desired.RevisionID,
		len(desired.Services),
		desiredRevisionFreshness(desired).Format(time.RFC3339),
	)
}

func printStateStatusActual(actual *takodstate.ActualSnapshot, err error, nodeActual []stateNodeActualCandidate) {
	if !actualSnapshotRepairable(actual) {
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Actual: error - %v\n", err)
		} else {
			fmt.Println("Actual: not recorded")
		}
	} else {
		fmt.Printf("Actual: %d service(s), freshness %s\n",
			actualSnapshotServiceCount(actual),
			actualSnapshotFreshness(actual).Format(time.RFC3339),
		)
	}
	if len(nodeActual) > 0 {
		fmt.Printf("Node actual: %d snapshot(s)\n", len(nodeActual))
	}
}

func printStateStatusLease(lease *remotestate.LeaseInfo, err error) {
	if err != nil {
		fmt.Printf("Lease: error - %v\n", err)
		return
	}
	if lease == nil {
		fmt.Println("Lease: free")
		return
	}
	fmt.Printf("Lease: held by %s\n", lease.Who)
	fmt.Printf("  ID:        %s\n", lease.ID)
	fmt.Printf("  Operation: %s\n", lease.Operation)
	fmt.Printf("  Created:   %s (%s ago)\n", lease.CreatedAt.Format(time.RFC3339), formatStateDuration(time.Since(lease.CreatedAt)))
	fmt.Printf("  Expires:   %s (in %s)\n", lease.ExpiresAt.Format(time.RFC3339), time.Until(lease.ExpiresAt).Round(time.Second))
}

func printBestKnownState(history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool, nodeActual map[string]stateNodeActualCandidate) {
	fmt.Println("=== Best Known State ===")
	if hasHistory {
		fmt.Printf("Deployment history: %s (%d deployment(s), freshness %s)\n",
			history.source,
			deploymentHistoryCount(history.history),
			deploymentHistoryFreshness(history.history).Format(time.RFC3339),
		)
		latest := latestDeploymentByTimestamp(history.history.Deployments)
		if latest != nil {
			fmt.Printf("  Latest: %s, %s, %s (%s ago)\n",
				remotestate.FormatDeploymentID(latest.ID),
				latest.Status,
				latest.Timestamp.Format(time.RFC3339),
				formatStateDuration(time.Since(latest.Timestamp)),
			)
		}
	} else {
		fmt.Println("Deployment history: not found on reachable nodes")
	}
	if hasDesired {
		fmt.Printf("Desired runtime: %s (%s, freshness %s)\n",
			desired.source,
			desired.desired.RevisionID,
			desiredRevisionFreshness(desired.desired).Format(time.RFC3339),
		)
	} else {
		fmt.Println("Desired runtime: not found on reachable nodes")
	}
	if hasActual {
		fmt.Printf("Actual runtime: %s (%d service(s), freshness %s)\n",
			actual.source,
			actualSnapshotServiceCount(actual.actual),
			actualSnapshotFreshness(actual.actual).Format(time.RFC3339),
		)
	} else {
		fmt.Println("Actual runtime: not found on reachable nodes")
	}
	if len(nodeActual) > 0 {
		nodes := sortedStateNodeActualNames(nodeActual)
		fmt.Printf("Node actual: %d node(s)\n", len(nodes))
		for _, nodeName := range nodes {
			candidate := nodeActual[nodeName]
			fmt.Printf("  %s: %s, freshness %s\n",
				nodeName,
				candidate.source,
				actualSnapshotFreshness(candidate.actual).Format(time.RFC3339),
			)
		}
	}
}

func readTakodAgentStatus(client any, cfg *config.Config) (*takodRemoteStatus, error) {
	output, err := takodclient.RequestJSONWithTimeout(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		"/v1/status",
		nil,
		stateStatusRequestTimeout,
	)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(output) == "" {
		return nil, fmt.Errorf("empty takod status response")
	}

	var status takodRemoteStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return nil, fmt.Errorf("failed to parse takod status: %w", err)
	}
	return &status, nil
}

func readMeshRuntimeStatus(client any, cfg *config.Config) (*mesh.Status, error) {
	if cfg.Mesh == nil {
		return nil, nil
	}
	output, err := takodclient.RequestJSONWithTimeout(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		"/v1/mesh/status?interface="+url.QueryEscape(cfg.Mesh.Interface),
		nil,
		stateStatusRequestTimeout,
	)
	if err != nil {
		return nil, err
	}
	var status mesh.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return nil, fmt.Errorf("failed to parse WireGuard status: %w", err)
	}
	return &status, nil
}

func printTakodRuntimeStatus(manager *takodstate.Manager) {
	fmt.Println("=== Takod Runtime State ===")

	desired, desiredErr := manager.ReadDesired()
	actual, actualErr := manager.ReadActual()
	if errors.Is(desiredErr, takodstate.ErrNotFound) && errors.Is(actualErr, takodstate.ErrNotFound) {
		fmt.Println("Status: No desired or actual runtime state recorded")
		return
	}

	if desiredErr != nil && !errors.Is(desiredErr, takodstate.ErrNotFound) {
		fmt.Printf("Desired: Error reading - %v\n", desiredErr)
	} else if desired != nil {
		fmt.Println("Desired revision:")
		fmt.Printf("  ID:      %s\n", desired.RevisionID)
		fmt.Printf("  Source:  %s\n", desired.Source)
		fmt.Printf("  Time:    %s (%s ago)\n", desired.CreatedAt.Format(time.RFC3339), formatStateDuration(time.Since(desired.CreatedAt)))
		if desired.Git.CommitShort != "" {
			fmt.Printf("  Commit:  %s\n", desired.Git.CommitShort)
		}
		if len(desired.TargetNodes) > 0 {
			fmt.Printf("  Nodes:   %s\n", strings.Join(desired.TargetNodes, ", "))
		}
		printDesiredRuntimeServices(desired.Services)
	} else {
		fmt.Println("Desired: Not recorded")
	}

	if actualErr != nil && !errors.Is(actualErr, takodstate.ErrNotFound) {
		fmt.Printf("Actual: Error reading - %v\n", actualErr)
	} else if actual != nil {
		fmt.Println("Actual snapshot:")
		fmt.Printf("  Time:    %s (%s ago)\n", actual.CapturedAt.Format(time.RFC3339), formatStateDuration(time.Since(actual.CapturedAt)))
		if len(actual.TargetNodes) > 0 {
			fmt.Printf("  Nodes:   %s\n", strings.Join(actual.TargetNodes, ", "))
		}
		printActualRuntimeServices(actual.Services)
		printActualNodeRuntimeServices(actual.Nodes)
	} else {
		fmt.Println("Actual: Not recorded")
	}
}

func printDesiredRuntimeServices(services map[string]takodstate.DesiredService) {
	if len(services) == 0 {
		fmt.Println("  Services: none")
		return
	}

	names := sortedServiceNames(services)
	fmt.Printf("  Services: %d\n", len(names))
	for _, name := range names {
		service := services[name]
		image := service.Image
		if image == "" {
			image = service.Build
		}
		if image == "" {
			image = "<none>"
		}
		fmt.Printf("    - %s: %d replica(s), %s\n", name, service.Replicas, image)
	}
}

func printActualRuntimeServices(services map[string]takodstate.ActualService) {
	if len(services) == 0 {
		fmt.Println("  Services: none")
		return
	}

	names := sortedServiceNames(services)
	fmt.Printf("  Services: %d\n", len(names))
	for _, name := range names {
		service := services[name]
		image := service.Image
		if image == "" {
			image = "<unknown>"
		}
		details := []string{fmt.Sprintf("%d replica(s)", service.Replicas), image}
		if service.CurrentRevision != "" {
			details = append(details, "revision "+service.CurrentRevision)
		}
		if service.DeployStrategy != "" {
			details = append(details, "strategy "+service.DeployStrategy)
		}
		if warming := len(service.WarmingContainers); warming > 0 {
			details = append(details, fmt.Sprintf("%d warming", warming))
		}
		fmt.Printf("    - %s: %s\n", name, strings.Join(details, ", "))
	}
}

func printActualNodeRuntimeServices(nodes map[string]takodstate.ActualNodeSnapshot) {
	if len(nodes) == 0 {
		return
	}

	nodeNames := make([]string, 0, len(nodes))
	for node := range nodes {
		nodeNames = append(nodeNames, node)
	}
	sort.Strings(nodeNames)

	fmt.Printf("  Node snapshots: %d\n", len(nodeNames))
	for _, nodeName := range nodeNames {
		snapshot := nodes[nodeName]
		fmt.Printf("    - %s: %d service(s), freshness %s\n",
			nodeName,
			len(snapshot.Services),
			snapshot.CapturedAt.Format(time.RFC3339),
		)
	}
}

func sortedServiceNames[T any](services map[string]T) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type stateRepairNode struct {
	name    string
	client  any
	cleanup func()
	manager stateRepairStateManager
	runtime stateRepairRuntimeManager
}

type stateRepairLease struct {
	serverName string
	manager    stateRepairStateManager
	lease      *remotestate.LeaseInfo
}

type stateRepairStateManager interface {
	LoadHistory() (*remotestate.DeploymentHistory, error)
	SaveHistory(*remotestate.DeploymentHistory) error
	AcquireLease(operation string, environment string, ttl time.Duration) (*remotestate.LeaseInfo, error)
	ReleaseLease(*remotestate.LeaseInfo) error
}

type stateRepairRuntimeManager interface {
	ReadActual() (*takodstate.ActualSnapshot, error)
	ReadNodeActual(string) (*takodstate.ActualSnapshot, error)
	WriteDesired(*takodstate.DesiredRevision) error
	WriteActual(*takodstate.ActualSnapshot) error
	WriteNodeActual(string, *takodstate.ActualSnapshot) error
	DeleteNodeActual(string) error
	AppendEvent(takodstate.Event) error
}

type stateRepairInventory struct {
	nodes      []stateRepairNode
	histories  []stateHistoryCandidate
	desired    []stateDesiredCandidate
	actual     []stateActualCandidate
	nodeActual []stateNodeActualCandidate
}

var collectStateRepairNodesForCommand = collectStateRepairNodesWithPool
var acquireStateRepairLeasesForCommand = acquireStateRepairLeases
var syncStateRepairHistoryToLocalForCommand = syncStateRepairHistoryToLocal

type stateHistoryCandidate struct {
	source  string
	history *remotestate.DeploymentHistory
}

type stateDesiredCandidate struct {
	source  string
	desired *takodstate.DesiredRevision
}

type stateActualCandidate struct {
	source string
	actual *takodstate.ActualSnapshot
}

type stateNodeActualCandidate struct {
	source string
	node   string
	actual *takodstate.ActualSnapshot
}

type stateForgetNodeResult struct {
	nodeName          string
	nodeActualExisted bool
	aggregatePruned   bool
	warnings          []string
	err               error
}

type stateForgetNodeIndexedResult struct {
	index  int
	result stateForgetNodeResult
}

func collectStateRepairNodes(cfg *config.Config, envName string, preferredServer string) (*stateRepairInventory, error) {
	return collectStateRepairNodesWithPool(nil, cfg, envName, preferredServer)
}

func collectStateRepairNodesWithPool(pool *ssh.Pool, cfg *config.Config, envName string, preferredServer string) (*stateRepairInventory, error) {
	serverNames, err := orderedStateServerNames(cfg, envName, preferredServer)
	if err != nil {
		return nil, err
	}
	factory, closeRuntime, err := newStateRuntimeFactory(pool, cfg)
	if err != nil {
		return nil, err
	}
	var cleanupOnce sync.Once
	sharedCleanup := func() { cleanupOnce.Do(closeRuntime) }
	keepRuntime := false
	defer func() {
		if !keepRuntime {
			sharedCleanup()
		}
	}()

	repair := &stateRepairInventory{
		nodes:      make([]stateRepairNode, 0, len(serverNames)),
		histories:  make([]stateHistoryCandidate, 0, len(serverNames)),
		desired:    make([]stateDesiredCandidate, 0, len(serverNames)),
		actual:     make([]stateActualCandidate, 0, len(serverNames)),
		nodeActual: make([]stateNodeActualCandidate, 0, len(serverNames)),
	}
	quiet := machineOutputEnabled()

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			closeStateRepairNodes(repair.nodes)
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		if !quiet {
			fmt.Printf("Checking %s (%s)...\n", serverName, server.Host)
		}
		client, _, err := factory.Client(context.Background(), serverName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot connect to %s: %v\n", serverName, err)
			continue
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
		runtime := takodstate.NewManager(client, cfg, envName)
		repair.nodes = append(repair.nodes, stateRepairNode{
			name:    serverName,
			client:  client,
			cleanup: sharedCleanup,
			manager: manager,
			runtime: runtime,
		})

		history, err := manager.LoadHistory()
		if errors.Is(err, remotestate.ErrNotFound) || !historyHasDeployments(history) {
			if verbose && !quiet {
				fmt.Printf("No deployment history found on %s\n", serverName)
			}
		} else if err != nil {
			if verbose && !quiet {
				fmt.Printf("Unable to read deployment history on %s: %v\n", serverName, err)
			}
		} else {
			repair.histories = append(repair.histories, stateHistoryCandidate{
				source:  serverName,
				history: history,
			})
			if !quiet {
				fmt.Printf("  history: %d deployment(s), freshness %s\n",
					deploymentHistoryCount(history),
					deploymentHistoryFreshness(history).Format(time.RFC3339),
				)
			}
		}

		desired, err := runtime.ReadDesired()
		if err == nil && desiredRevisionRepairable(desired) {
			repair.desired = append(repair.desired, stateDesiredCandidate{
				source:  serverName,
				desired: desired,
			})
			if !quiet {
				fmt.Printf("  desired: %s, freshness %s\n",
					desired.RevisionID,
					desiredRevisionFreshness(desired).Format(time.RFC3339),
				)
			}
		} else if verbose && !quiet && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Unable to read desired runtime state on %s: %v\n", serverName, err)
		}

		actual, err := runtime.ReadActual()
		if err == nil && actualSnapshotRepairable(actual) {
			repair.actual = append(repair.actual, stateActualCandidate{
				source: serverName,
				actual: actual,
			})
			if !quiet {
				fmt.Printf("  actual: %d service(s), freshness %s\n",
					actualSnapshotServiceCount(actual),
					actualSnapshotFreshness(actual).Format(time.RFC3339),
				)
			}
			for nodeName, nodeSnapshot := range actual.Nodes {
				if !stateNodeNameInList(serverNames, nodeName) {
					continue
				}
				repair.nodeActual = append(repair.nodeActual, stateNodeActualCandidate{
					source: serverName + " aggregate",
					node:   nodeName,
					actual: actualSnapshotFromEmbeddedNode(actual.Project, actual.Environment, nodeSnapshot),
				})
			}
		} else if verbose && !quiet && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Unable to read actual runtime state on %s: %v\n", serverName, err)
		}

		nodeActualCount := 0
		for _, nodeName := range serverNames {
			nodeActual, err := runtime.ReadNodeActual(nodeName)
			if err == nil && nodeActualSnapshotRepairable(nodeActual, nodeName) {
				repair.nodeActual = append(repair.nodeActual, stateNodeActualCandidate{
					source: serverName,
					node:   nodeName,
					actual: nodeActual,
				})
				nodeActualCount++
			} else if verbose && !quiet && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
				fmt.Printf("Unable to read node actual runtime state for %s on %s: %v\n", nodeName, serverName, err)
			}
		}
		if nodeActualCount > 0 && !quiet {
			fmt.Printf("  node actual: %d snapshot(s)\n", nodeActualCount)
		}
	}

	keepRuntime = len(repair.nodes) > 0
	return repair, nil
}

func closeStateRepairNodes(nodes []stateRepairNode) {
	for _, node := range nodes {
		if node.cleanup != nil {
			node.cleanup()
		} else if client, ok := node.client.(*ssh.Client); ok && client != nil {
			_ = client.Close()
		}
	}
}

func engineStateRepairNodes(nodes []stateRepairNode) []engine.StateRepairNode {
	out := make([]engine.StateRepairNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, engine.StateRepairNode{Name: node.name, HistoryManager: node.manager, Runtime: node.runtime})
	}
	return out
}

func engineStateRepairHistories(candidates []stateHistoryCandidate) []engine.StateRepairHistoryCandidate {
	out := make([]engine.StateRepairHistoryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, engine.StateRepairHistoryCandidate{Source: candidate.source, History: candidate.history})
	}
	return out
}

func engineStateRepairDesired(candidates []stateDesiredCandidate) []engine.StateRepairDesiredCandidate {
	out := make([]engine.StateRepairDesiredCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, engine.StateRepairDesiredCandidate{Source: candidate.source, Desired: candidate.desired})
	}
	return out
}

func engineStateRepairActual(candidates []stateActualCandidate) []engine.StateRepairActualCandidate {
	out := make([]engine.StateRepairActualCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, engine.StateRepairActualCandidate{Source: candidate.source, Actual: candidate.actual})
	}
	return out
}

func engineStateRepairNodeActual(candidates []stateNodeActualCandidate) []engine.StateRepairNodeActualCandidate {
	out := make([]engine.StateRepairNodeActualCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, engine.StateRepairNodeActualCandidate{Source: candidate.source, Node: candidate.node, Actual: candidate.actual})
	}
	return out
}

func acquireStateRepairLeases(nodes []stateRepairNode, envName string) ([]stateRepairLease, error) {
	return acquireStateOperationLeases(nodes, envName, "state-repair")
}

func acquireStateForgetNodeLeases(nodes []stateRepairNode, envName string) ([]stateRepairLease, error) {
	return acquireStateOperationLeases(nodes, envName, "state-forget-node")
}

func acquireStateOperationLeases(nodes []stateRepairNode, envName string, operation string) ([]stateRepairLease, error) {
	return acquireStateRepairLeasesWith(nodes, func(node stateRepairNode) (stateRepairLease, error) {
		lease, err := node.manager.AcquireLease(operation, envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			return stateRepairLease{}, fmt.Errorf("failed to acquire %s lease on %s: %w", operation, node.name, err)
		}
		return stateRepairLease{
			serverName: node.name,
			manager:    node.manager,
			lease:      lease,
		}, nil
	})
}

type stateRepairLeaseAcquireFunc func(stateRepairNode) (stateRepairLease, error)

type stateRepairLeaseResult struct {
	index int
	lease stateRepairLease
	err   error
}

func acquireStateRepairLeasesWith(nodes []stateRepairNode, acquire stateRepairLeaseAcquireFunc) ([]stateRepairLease, error) {
	resultCh := make(chan stateRepairLeaseResult, len(nodes))
	var wg sync.WaitGroup

	for index, node := range nodes {
		wg.Add(1)
		go func(index int, node stateRepairNode) {
			defer wg.Done()
			lease, err := acquire(node)
			resultCh <- stateRepairLeaseResult{
				index: index,
				lease: lease,
				err:   err,
			}
		}(index, node)
	}

	wg.Wait()
	close(resultCh)

	ordered := make([]stateRepairLease, len(nodes))
	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, result.err.Error())
			continue
		}
		ordered[result.index] = result.lease
	}

	leases := make([]stateRepairLease, 0, len(nodes))
	for _, lease := range ordered {
		if lease.lease == nil {
			continue
		}
		leases = append(leases, lease)
	}

	if len(errors) > 0 {
		releaseStateRepairLeases(leases, false)
		sort.Strings(errors)
		return nil, fmt.Errorf("failed to acquire repair leases: %s", strings.Join(errors, "; "))
	}
	return leases, nil
}

func releaseStateRepairLeases(leases []stateRepairLease, verbose bool) {
	for i := len(leases) - 1; i >= 0; i-- {
		lease := leases[i]
		if lease.manager == nil || lease.lease == nil {
			continue
		}
		if err := lease.manager.ReleaseLease(lease.lease); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: failed to release repair lease on %s: %v\n", lease.serverName, err)
		}
	}
}

func stateRepairLeaseSummary(leases []stateRepairLease) string {
	parts := make([]string, 0, len(leases))
	for _, lease := range leases {
		parts = append(parts, fmt.Sprintf("%s:%s", lease.serverName, lease.lease.ID))
	}
	return strings.Join(parts, ", ")
}

func printStateRepairSource(hasHistory bool, history stateHistoryCandidate, hasDesired bool, desired stateDesiredCandidate, hasActual bool, actual stateActualCandidate, nodeActual map[string]stateNodeActualCandidate) {
	if hasHistory {
		fmt.Printf("Deployment history source: %s (%d deployment(s), freshness %s)\n",
			history.source,
			deploymentHistoryCount(history.history),
			deploymentHistoryFreshness(history.history).Format(time.RFC3339),
		)
	}
	if hasDesired {
		fmt.Printf("Desired runtime source: %s (%s, freshness %s)\n",
			desired.source,
			desired.desired.RevisionID,
			desiredRevisionFreshness(desired.desired).Format(time.RFC3339),
		)
	}
	if hasActual {
		fmt.Printf("Actual runtime source: %s (%d service(s), freshness %s)\n",
			actual.source,
			actualSnapshotServiceCount(actual.actual),
			actualSnapshotFreshness(actual.actual).Format(time.RFC3339),
		)
	}
	if len(nodeActual) > 0 {
		nodes := sortedStateNodeActualNames(nodeActual)
		fmt.Printf("Node actual sources: %d node(s)\n", len(nodes))
		for _, nodeName := range nodes {
			candidate := nodeActual[nodeName]
			fmt.Printf("  %s: %s (%d service(s), freshness %s)\n",
				nodeName,
				candidate.source,
				actualSnapshotServiceCount(candidate.actual),
				actualSnapshotFreshness(candidate.actual).Format(time.RFC3339),
			)
		}
	}
}

func renderStateRepairSources(result *engine.StateRepairResult) {
	if result == nil {
		return
	}
	if history := result.Sources.History; history != nil {
		fmt.Printf("Deployment history source: %s (%d deployment(s), freshness %s)\n", history.Source, history.Count, history.Freshness.Format(time.RFC3339))
	}
	if desired := result.Sources.Desired; desired != nil {
		fmt.Printf("Desired runtime source: %s (%s, freshness %s)\n", desired.Source, desired.RevisionID, desired.Freshness.Format(time.RFC3339))
	}
	if actual := result.Sources.Actual; actual != nil {
		fmt.Printf("Actual runtime source: %s (%d service(s), freshness %s)\n", actual.Source, actual.ServiceCount, actual.Freshness.Format(time.RFC3339))
	}
	if len(result.Sources.NodeActual) > 0 {
		fmt.Printf("Node actual sources: %d node(s)\n", len(result.Sources.NodeActual))
		for _, candidate := range result.Sources.NodeActual {
			fmt.Printf("  %s: %s (%d service(s), freshness %s)\n", candidate.Node, candidate.Source, candidate.ServiceCount, candidate.Freshness.Format(time.RFC3339))
		}
	}
}

func renderStateRepairWarnings(result *engine.StateRepairResult) {
	if result == nil || machineOutputEnabled() {
		return
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}
}

func renderStateRepairWriteSummary(result *engine.StateRepairResult) {
	if result == nil {
		return
	}
	counts := result.Writes.Counts
	nodeCount := result.Counts.ReachableNodes
	if result.Sources.HasHistory {
		fmt.Printf("Repaired deployment history on %d/%d reachable node(s)\n", counts.History, nodeCount)
	}
	if result.Sources.HasDesired {
		fmt.Printf("Repaired desired runtime state on %d/%d reachable node(s)\n", counts.Desired, nodeCount)
	}
	if result.Sources.HasActual {
		fmt.Printf("Repaired actual runtime state on %d/%d reachable node(s)\n", counts.Actual, nodeCount)
	}
	if result.Sources.HasNodeActual {
		fmt.Printf("Repaired node actual runtime state with %d document write(s)\n", counts.NodeActual)
	}
}

func renderStateRepairResult(result *engine.StateRepairResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	renderStateRepairSources(result)
	renderStateRepairWarnings(result)
	if result.Status == engine.StateRepairStatusSuccess {
		renderStateRepairWriteSummary(result)
	}
	return nil
}

func writeStateRepairDocuments(nodes []stateRepairNode, history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool, nodeActual map[string]stateNodeActualCandidate) (int, int, int, int, error) {
	project, envName := stateRepairDocumentProjectEnvironment(history, hasHistory, desired, hasDesired, actual, hasActual, nodeActual)
	req := engine.StateRepairRequest{
		Config:      &config.Config{Project: config.ProjectConfig{Name: project}},
		Environment: envName,
		Nodes:       engineStateRepairNodes(nodes),
	}
	if hasHistory {
		req.Histories = []engine.StateRepairHistoryCandidate{{Source: history.source, History: history.history}}
	}
	if hasDesired {
		req.Desired = []engine.StateRepairDesiredCandidate{{Source: desired.source, Desired: desired.desired}}
	}
	if hasActual {
		req.Actual = []engine.StateRepairActualCandidate{{Source: actual.source, Actual: actual.actual}}
	}
	for _, nodeName := range sortedStateNodeActualNames(nodeActual) {
		candidate := nodeActual[nodeName]
		req.NodeActual = append(req.NodeActual, engine.StateRepairNodeActualCandidate{Source: candidate.source, Node: candidate.node, Actual: candidate.actual})
	}

	result, err := cliEngine().StateRepair(context.Background(), req)
	if result == nil {
		return 0, 0, 0, 0, err
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}
	counts := result.Writes.Counts
	return counts.History, counts.Desired, counts.Actual, counts.NodeActual, err
}

func stateRepairDocumentProjectEnvironment(history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool, nodeActual map[string]stateNodeActualCandidate) (string, string) {
	if hasHistory && history.history != nil {
		return history.history.ProjectName, history.history.Environment
	}
	if hasDesired && desired.desired != nil {
		return desired.desired.Project, desired.desired.Environment
	}
	if hasActual && actual.actual != nil {
		return actual.actual.Project, actual.actual.Environment
	}
	for _, candidate := range nodeActual {
		if candidate.actual != nil {
			return candidate.actual.Project, candidate.actual.Environment
		}
	}
	return "", ""
}

type stateRepairWriteCounts struct {
	history    int
	desired    int
	actual     int
	nodeActual int
}

type stateRepairWriteResult struct {
	nodeName string
	counts   stateRepairWriteCounts
	warnings []string
	err      error
}

func writeStateRepairDocumentsToNode(node stateRepairNode, history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool, nodeActual map[string]stateNodeActualCandidate) stateRepairWriteResult {
	result := stateRepairWriteResult{nodeName: node.name}
	var previousActual *takodstate.ActualSnapshot
	if hasActual || len(nodeActual) > 0 {
		var err error
		previousActual, err = node.runtime.ReadActual()
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to read previous actual runtime state on %s before pruning stale node state: %v", node.name, err))
		}
	}

	if hasHistory {
		historyCopy, err := cloneRemoteDeploymentHistory(history.history)
		if err != nil {
			result.err = fmt.Errorf("failed to prepare history for repair: %w", err)
			return result
		}
		if err := node.manager.SaveHistory(historyCopy); err != nil {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to repair deployment history on %s: %v", node.name, err))
		} else {
			result.counts.history++
		}
	}

	if hasDesired {
		desiredCopy, err := cloneDesiredRevision(desired.desired)
		if err != nil {
			result.err = fmt.Errorf("failed to prepare desired runtime state for repair: %w", err)
			return result
		}
		if err := node.runtime.WriteDesired(desiredCopy); err != nil {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to repair desired runtime state on %s: %v", node.name, err))
		} else {
			result.counts.desired++
		}
	}

	if hasActual {
		actualCopy, err := cloneActualSnapshot(actual.actual)
		if err != nil {
			result.err = fmt.Errorf("failed to prepare actual runtime state for repair: %w", err)
			return result
		}
		if err := node.runtime.WriteActual(actualCopy); err != nil {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to repair actual runtime state on %s: %v", node.name, err))
		} else {
			result.counts.actual++
		}
	}

	nodeNames := sortedStateNodeActualNames(nodeActual)
	for _, nodeName := range nodeNames {
		candidate := nodeActual[nodeName]
		actualCopy, err := cloneActualSnapshot(candidate.actual)
		if err != nil {
			result.err = fmt.Errorf("failed to prepare node actual runtime state for repair: %w", err)
			return result
		}
		if err := node.runtime.WriteNodeActual(nodeName, actualCopy); err != nil {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to repair node actual runtime state for %s on %s: %v", nodeName, node.name, err))
		} else {
			result.counts.nodeActual++
		}
	}

	for _, staleNode := range takodstate.StaleNodeActualNames(previousActual, actual.actual, stateRepairNodeActualSnapshots(nodeActual)) {
		if err := node.runtime.DeleteNodeActual(staleNode); err != nil {
			result.warnings = append(result.warnings, fmt.Sprintf("failed to delete stale node actual runtime state for %s on %s: %v", staleNode, node.name, err))
		}
	}

	return result
}

func printStateRepairWriteSummary(nodeCount int, hasHistory bool, historyWritten int, hasDesired bool, desiredWritten int, hasActual bool, actualWritten int, hasNodeActual bool, nodeActualWritten int) {
	if hasHistory {
		fmt.Printf("Repaired deployment history on %d/%d reachable node(s)\n", historyWritten, nodeCount)
	}
	if hasDesired {
		fmt.Printf("Repaired desired runtime state on %d/%d reachable node(s)\n", desiredWritten, nodeCount)
	}
	if hasActual {
		fmt.Printf("Repaired actual runtime state on %d/%d reachable node(s)\n", actualWritten, nodeCount)
	}
	if hasNodeActual {
		fmt.Printf("Repaired node actual runtime state with %d document write(s)\n", nodeActualWritten)
	}
}

func orderedStateServerNames(cfg *config.Config, envName string, preferredServer string) ([]string, error) {
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(serverNames) == 0 {
		return nil, fmt.Errorf("no servers configured for environment %s", envName)
	}
	if preferredServer == "" {
		return serverNames, nil
	}

	ordered := make([]string, 0, len(serverNames))
	found := false
	for _, serverName := range serverNames {
		if serverName == preferredServer {
			found = true
			ordered = append(ordered, serverName)
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("server %s is not part of environment %s", preferredServer, envName)
	}
	for _, serverName := range serverNames {
		if serverName != preferredServer {
			ordered = append(ordered, serverName)
		}
	}
	return ordered, nil
}

func bestDeploymentHistory(candidates []stateHistoryCandidate) (stateHistoryCandidate, bool) {
	var best stateHistoryCandidate
	ok := false
	for _, candidate := range candidates {
		if !historyHasDeployments(candidate.history) {
			continue
		}
		if !ok || deploymentHistoryBetter(candidate.history, best.history) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestDesiredRevision(candidates []stateDesiredCandidate) (stateDesiredCandidate, bool) {
	var best stateDesiredCandidate
	ok := false
	for _, candidate := range candidates {
		if !desiredRevisionRepairable(candidate.desired) {
			continue
		}
		if !ok || desiredRevisionBetter(candidate.desired, best.desired) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestActualSnapshot(candidates []stateActualCandidate) (stateActualCandidate, bool) {
	var best stateActualCandidate
	ok := false
	for _, candidate := range candidates {
		if !actualSnapshotRepairable(candidate.actual) {
			continue
		}
		if !ok || actualSnapshotBetter(candidate.actual, best.actual) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestNodeActualSnapshots(candidates []stateNodeActualCandidate) map[string]stateNodeActualCandidate {
	best := make(map[string]stateNodeActualCandidate)
	for _, candidate := range candidates {
		if !nodeActualSnapshotRepairable(candidate.actual, candidate.node) {
			continue
		}
		current, ok := best[candidate.node]
		if !ok || actualSnapshotBetter(candidate.actual, current.actual) {
			best[candidate.node] = candidate
		}
	}
	return best
}

func actualSnapshotWithNodeSnapshots(snapshot *takodstate.ActualSnapshot, nodeActual map[string]stateNodeActualCandidate) *takodstate.ActualSnapshot {
	if snapshot == nil {
		return nil
	}
	if len(nodeActual) == 0 {
		return snapshot
	}
	return aggregateActualSnapshotFromNodeSnapshots(snapshot.Project, snapshot.Environment, nodeActual)
}

func aggregateActualSnapshotFromNodeSnapshots(project string, environment string, nodeActual map[string]stateNodeActualCandidate) *takodstate.ActualSnapshot {
	nodes := sortedStateNodeActualNames(nodeActual)
	snapshot := &takodstate.ActualSnapshot{
		SchemaVersion: takodstate.SchemaVersion,
		Project:       project,
		Environment:   environment,
		TargetNodes:   nodes,
		Services:      make(map[string]takodstate.ActualService),
		Nodes:         actualNodeSnapshotMap(nodeActual),
	}

	var newest time.Time
	for _, nodeName := range nodes {
		candidate := nodeActual[nodeName]
		if candidate.actual == nil {
			continue
		}
		if candidate.actual.CapturedAt.After(newest) {
			newest = candidate.actual.CapturedAt
		}
		for serviceName, service := range candidate.actual.Services {
			if existing, ok := snapshot.Services[serviceName]; ok {
				existing.Replicas += service.Replicas
				existing.Containers = append(existing.Containers, service.Containers...)
				if existing.Image == "" {
					existing.Image = service.Image
				}
				if existing.ConfigHash == "" {
					existing.ConfigHash = service.ConfigHash
				} else if service.ConfigHash != "" && existing.ConfigHash != service.ConfigHash {
					existing.ConfigHash = ""
				}
				existing.RuntimeID = mergeActualRuntimeID(existing.RuntimeID, service.RuntimeID)
				existing.Persistent = existing.Persistent || service.Persistent
				existing.CurrentRevision = mergeActualOptionalLabel(existing.CurrentRevision, service.CurrentRevision)
				existing.PreviousRevision = mergeActualOptionalLabel(existing.PreviousRevision, service.PreviousRevision)
				existing.DeployStrategy = mergeActualOptionalLabel(existing.DeployStrategy, service.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, service.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, service.WarmingContainers...)
				snapshot.Services[serviceName] = existing
				continue
			}
			snapshot.Services[serviceName] = takodstate.ActualService{
				Name:              service.Name,
				Image:             service.Image,
				Replicas:          service.Replicas,
				Containers:        append([]string(nil), service.Containers...),
				ConfigHash:        service.ConfigHash,
				RuntimeID:         service.RuntimeID,
				Persistent:        service.Persistent,
				CurrentRevision:   service.CurrentRevision,
				PreviousRevision:  service.PreviousRevision,
				DeployStrategy:    service.DeployStrategy,
				ActiveContainers:  append([]string(nil), service.ActiveContainers...),
				WarmingContainers: append([]string(nil), service.WarmingContainers...),
			}
		}
	}
	if newest.IsZero() {
		newest = time.Now().UTC()
	}
	snapshot.CapturedAt = newest
	return snapshot
}

func actualNodeSnapshotMap(nodeActual map[string]stateNodeActualCandidate) map[string]takodstate.ActualNodeSnapshot {
	if len(nodeActual) == 0 {
		return nil
	}
	out := make(map[string]takodstate.ActualNodeSnapshot, len(nodeActual))
	for _, nodeName := range sortedStateNodeActualNames(nodeActual) {
		candidate := nodeActual[nodeName]
		if candidate.actual == nil {
			continue
		}
		out[nodeName] = takodstate.ActualNodeSnapshot{
			Node:       nodeName,
			Services:   cloneActualServices(candidate.actual.Services),
			CapturedAt: candidate.actual.CapturedAt,
		}
	}
	return out
}

func actualSnapshotFromEmbeddedNode(project string, environment string, snapshot takodstate.ActualNodeSnapshot) *takodstate.ActualSnapshot {
	return &takodstate.ActualSnapshot{
		SchemaVersion: takodstate.SchemaVersion,
		Project:       project,
		Environment:   environment,
		Node:          snapshot.Node,
		Services:      cloneActualServices(snapshot.Services),
		CapturedAt:    snapshot.CapturedAt,
	}
}

func cloneActualServices(services map[string]takodstate.ActualService) map[string]takodstate.ActualService {
	if len(services) == 0 {
		return nil
	}
	out := make(map[string]takodstate.ActualService, len(services))
	for name, service := range services {
		service.Containers = append([]string(nil), service.Containers...)
		out[name] = service
	}
	return out
}

func sortedStateNodeActualNames(nodeActual map[string]stateNodeActualCandidate) []string {
	nodes := make([]string, 0, len(nodeActual))
	for node := range nodeActual {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

func stateRepairNodeActualSnapshots(nodeActual map[string]stateNodeActualCandidate) map[string]*takodstate.ActualSnapshot {
	if len(nodeActual) == 0 {
		return nil
	}
	out := make(map[string]*takodstate.ActualSnapshot, len(nodeActual))
	for nodeName, candidate := range nodeActual {
		if candidate.actual != nil {
			out[nodeName] = candidate.actual
		}
	}
	return out
}

func stateNodeNameInList(nodes []string, nodeName string) bool {
	for _, node := range nodes {
		if node == nodeName {
			return true
		}
	}
	return false
}

func deploymentHistoryBetter(candidate *remotestate.DeploymentHistory, current *remotestate.DeploymentHistory) bool {
	candidateFreshness := deploymentHistoryFreshness(candidate)
	currentFreshness := deploymentHistoryFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	return deploymentHistoryCount(candidate) > deploymentHistoryCount(current)
}

func desiredRevisionBetter(candidate *takodstate.DesiredRevision, current *takodstate.DesiredRevision) bool {
	candidateFreshness := desiredRevisionFreshness(candidate)
	currentFreshness := desiredRevisionFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	if candidate == nil || current == nil {
		return candidate != nil
	}
	return candidate.RevisionID > current.RevisionID
}

func actualSnapshotBetter(candidate *takodstate.ActualSnapshot, current *takodstate.ActualSnapshot) bool {
	candidateFreshness := actualSnapshotFreshness(candidate)
	currentFreshness := actualSnapshotFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	return actualSnapshotServiceCount(candidate) > actualSnapshotServiceCount(current)
}

func historyHasDeployments(history *remotestate.DeploymentHistory) bool {
	return deploymentHistoryCount(history) > 0
}

func desiredRevisionRepairable(revision *takodstate.DesiredRevision) bool {
	return revision != nil && revision.RevisionID != "" && !revision.CreatedAt.IsZero()
}

func actualSnapshotRepairable(snapshot *takodstate.ActualSnapshot) bool {
	return snapshot != nil && !snapshot.CapturedAt.IsZero()
}

func nodeActualSnapshotRepairable(snapshot *takodstate.ActualSnapshot, node string) bool {
	if !actualSnapshotRepairable(snapshot) {
		return false
	}
	return snapshot.Node == "" || snapshot.Node == node
}

func deploymentHistoryCount(history *remotestate.DeploymentHistory) int {
	if history == nil {
		return 0
	}
	count := 0
	for _, deployment := range history.Deployments {
		if deployment != nil {
			count++
		}
	}
	return count
}

func deploymentHistoryFreshness(history *remotestate.DeploymentHistory) time.Time {
	if history == nil {
		return time.Time{}
	}
	freshness := history.LastUpdated
	for _, deployment := range history.Deployments {
		if deployment != nil && deployment.Timestamp.After(freshness) {
			freshness = deployment.Timestamp
		}
	}
	return freshness
}

func desiredRevisionFreshness(revision *takodstate.DesiredRevision) time.Time {
	if revision == nil {
		return time.Time{}
	}
	return revision.CreatedAt
}

func actualSnapshotFreshness(snapshot *takodstate.ActualSnapshot) time.Time {
	if snapshot == nil {
		return time.Time{}
	}
	return snapshot.CapturedAt
}

func actualSnapshotServiceCount(snapshot *takodstate.ActualSnapshot) int {
	if snapshot == nil {
		return 0
	}
	return len(snapshot.Services)
}

func cloneRemoteDeploymentHistory(history *remotestate.DeploymentHistory) (*remotestate.DeploymentHistory, error) {
	if history == nil {
		return nil, fmt.Errorf("history is nil")
	}
	data, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}
	var out remotestate.DeploymentHistory
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneDesiredRevision(revision *takodstate.DesiredRevision) (*takodstate.DesiredRevision, error) {
	if revision == nil {
		return nil, fmt.Errorf("desired revision is nil")
	}
	data, err := json.Marshal(revision)
	if err != nil {
		return nil, err
	}
	var out takodstate.DesiredRevision
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneActualSnapshot(snapshot *takodstate.ActualSnapshot) (*takodstate.ActualSnapshot, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("actual snapshot is nil")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var out takodstate.ActualSnapshot
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// convertRemoteToLocal converts a remote DeploymentState to local format
func convertRemoteToLocal(remote *remotestate.DeploymentState, env string) *localstate.DeploymentState {
	local := &localstate.DeploymentState{
		DeploymentID:    remote.ID,
		Timestamp:       remote.Timestamp,
		Environment:     env,
		Mode:            config.RuntimeModeTakod,
		Status:          string(remote.Status),
		DurationSeconds: int(remote.Duration.Seconds()),
		GitCommit:       remote.GitCommit,
		TriggeredBy:     remote.User,
		Notes:           remote.Message,
		Services:        make(map[string]*localstate.ServiceDeploy),
	}

	// Convert services
	for name, svc := range remote.Services {
		local.Services[name] = &localstate.ServiceDeploy{
			Image:    svc.Image,
			ImageID:  svc.ImageID,
			Replicas: svc.Replicas,
			Ports:    []int{svc.Port},
			Health:   boolToHealth(svc.HealthCheck.Healthy),
		}
	}

	return local
}

func boolToHealth(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unknown"
}

func syncStateRepairHistoryToLocal(cfg *config.Config, envName string, history *remotestate.DeploymentHistory) (int, error) {
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return 0, fmt.Errorf("remote state repaired, but local state initialization failed: %w", err)
	}
	var deployments []*remotestate.DeploymentState
	if history != nil {
		deployments = history.Deployments
	}
	synced, err := syncRemoteDeploymentsToLocal(localMgr, deployments, envName)
	if err != nil {
		return synced, fmt.Errorf("remote state repaired, but local state sync failed: %w", err)
	}
	return synced, nil
}

func syncRemoteDeploymentsToLocal(localMgr *localstate.Manager, remoteDeployments []*remotestate.DeploymentState, envName string) (int, error) {
	ordered := append([]*remotestate.DeploymentState(nil), remoteDeployments...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i] == nil {
			return false
		}
		if ordered[j] == nil {
			return true
		}
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	synced := 0
	for _, remoteDep := range ordered {
		if remoteDep == nil {
			continue
		}
		localDep := convertRemoteToLocal(remoteDep, envName)
		if err := localMgr.SaveDeployment(localDep); err != nil {
			return synced, fmt.Errorf("failed to save deployment %s: %w", remoteDep.ID, err)
		}
		synced++
	}
	return synced, nil
}

func syncBestDeploymentHistoryToLocal(cfg *config.Config, envName string, histories []stateHistoryCandidate) (string, int, bool, error) {
	best, ok := bestDeploymentHistory(histories)
	if !ok {
		return "", 0, false, nil
	}
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return best.source, 0, true, err
	}
	synced, err := syncRemoteDeploymentsToLocal(localMgr, best.history.Deployments, envName)
	return best.source, synced, true, err
}

func localDeploymentStateExists(envName string) bool {
	currentPath := filepath.Join(".tako", "deployments", envName, "current.json")
	info, err := os.Stat(currentPath)
	return err == nil && !info.IsDir()
}

func formatStateDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	return fmt.Sprintf("%dd", days)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SyncStateOnDeploy refreshes local deployment state from the remote mesh before
// deploy. When no remote deployment history exists, runtime recovery is used
// only for missing local state so an existing local cache is not overwritten by
// weaker inferred state.
func SyncStateOnDeploy(cfg *config.Config, envName string) error {
	return SyncStateOnDeployWithPool(nil, cfg, envName)
}

func SyncStateOnDeployWithPool(pool *ssh.Pool, cfg *config.Config, envName string) error {
	localExists := localDeploymentStateExists(envName)

	if verbose {
		if localExists {
			fmt.Println("Refreshing local deployment state from remote mesh...")
		} else {
			fmt.Println("Local state missing, checking remote mesh...")
		}
	}

	histories, err := syncStateCollectDeploymentHistories(pool, cfg, envName, "", true)
	if err != nil {
		return nil // Ignore auto-sync discovery errors and continue deployment.
	}

	source, synced, ok, err := syncBestDeploymentHistoryToLocal(cfg, envName, histories)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to sync remote state locally: %v\n", err)
		}
		return nil
	}
	if !ok {
		if verbose {
			fmt.Println("No remote deployment history found during auto-sync")
		}
		if localExists {
			return nil
		}
		if err := syncStateRecoverFromMeshActual(pool, cfg, envName, ""); err == nil {
			if verbose {
				fmt.Println("Recovered local state from replicated takod runtime state")
			}
			return nil
		} else if verbose {
			fmt.Printf("Warning: failed to recover local state from replicated takod runtime state: %v\n", err)
		}
		if err := syncStateRecoverFromRunningMesh(pool, cfg, envName, ""); err != nil && verbose {
			fmt.Printf("Warning: failed to recover local state from running mesh containers: %v\n", err)
		}
		return nil
	}

	if verbose {
		fmt.Printf("Synced %d deployment(s) from %s\n", synced, source)
	}

	return nil
}

func recoverAndSaveStateFromMeshActual(cfg *config.Config, envName string, requestedServer string) error {
	return recoverAndSaveStateFromMeshActualWithPool(nil, cfg, envName, requestedServer)
}

func recoverAndSaveStateFromMeshActualWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) error {
	result, err := recoverAndSaveStateFromMeshActualResultWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return err
	}
	fmt.Printf("Recovered state from %d running service(s)\n", result.ServiceCount)
	return nil
}

func recoverAndSaveStateFromMeshActualResult(cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	return recoverAndSaveStateFromMeshActualResultWithPool(nil, cfg, envName, requestedServer)
}

func recoverAndSaveStateFromMeshActualResultWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	nodes, err := collectStateStatusNodesWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return engine.StatePullRecoveryResult{}, err
	}
	_, _, actualCandidates, nodeActualCandidates := stateStatusCandidates(nodes)
	bestActual, hasActual, _ := bestStateStatusActual(cfg.Project.Name, envName, actualCandidates, nodeActualCandidates)
	if !hasActual {
		return engine.StatePullRecoveryResult{}, fmt.Errorf("no mesh actual state found")
	}

	deployment, err := ReconcileStateFromActualSnapshot(cfg, envName, bestActual.actual, "State recovered from replicated takod actual state")
	if err != nil {
		return engine.StatePullRecoveryResult{}, err
	}
	return saveRecoveredDeploymentResult(cfg, envName, deployment)
}

type runningActualNodeResult struct {
	index      int
	serverName string
	host       string
	candidate  stateNodeActualCandidate
	err        error
}

func recoverAndSaveStateFromRunningMesh(cfg *config.Config, envName string, requestedServer string) error {
	return recoverAndSaveStateFromRunningMeshWithPool(nil, cfg, envName, requestedServer)
}

func recoverAndSaveStateFromRunningMeshWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) error {
	result, err := recoverAndSaveStateFromRunningMeshResultWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return err
	}
	fmt.Printf("Recovered state from %d running service(s)\n", result.ServiceCount)
	return nil
}

func recoverAndSaveStateFromRunningMeshResult(cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	return recoverAndSaveStateFromRunningMeshResultWithPool(nil, cfg, envName, requestedServer)
}

func recoverAndSaveStateFromRunningMeshResultWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) (engine.StatePullRecoveryResult, error) {
	candidates, err := collectRunningActualNodeSnapshotsWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return engine.StatePullRecoveryResult{}, err
	}
	bestNodeActual := bestNodeActualSnapshots(candidates)
	if len(bestNodeActual) == 0 {
		return engine.StatePullRecoveryResult{}, fmt.Errorf("no running takod containers found on reachable mesh nodes")
	}
	aggregate := aggregateActualSnapshotFromNodeSnapshots(cfg.Project.Name, envName, bestNodeActual)
	deployment, err := ReconcileStateFromActualSnapshot(cfg, envName, aggregate, "State recovered from running takod containers across the mesh")
	if err != nil {
		return engine.StatePullRecoveryResult{}, err
	}
	return saveRecoveredDeploymentResult(cfg, envName, deployment)
}

func collectRunningActualNodeSnapshots(cfg *config.Config, envName string, requestedServer string) ([]stateNodeActualCandidate, error) {
	return collectRunningActualNodeSnapshotsWithPool(nil, cfg, envName, requestedServer)
}

func collectRunningActualNodeSnapshotsWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) ([]stateNodeActualCandidate, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	factory, closeRuntime, err := newStateRuntimeFactory(pool, cfg)
	if err != nil {
		return nil, err
	}
	defer closeRuntime()

	results := make([]runningActualNodeResult, len(serverNames))
	resultCh := make(chan runningActualNodeResult, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			result := runningActualNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
			}

			client, _, err := factory.Client(context.Background(), serverName)
			if err != nil {
				result.err = err
				resultCh <- result
				return
			}
			actual, err := actualStateViaTakod(client, cfg, envName)
			if err != nil {
				result.err = err
				resultCh <- result
				return
			}

			snapshot := actualSnapshotFromTakodActual(cfg.Project.Name, envName, serverName, actual, time.Now().UTC())
			if len(snapshot.Services) > 0 {
				result.candidate = stateNodeActualCandidate{
					source: serverName,
					node:   serverName,
					actual: snapshot,
				}
			}
			resultCh <- result
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		results[result.index] = result
	}

	readErrors := make([]string, 0)
	candidates := make([]stateNodeActualCandidate, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			readErrors = append(readErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			if verbose {
				fmt.Fprintf(os.Stderr, "Warning: cannot read running state from %s: %v\n", result.serverName, result.err)
			}
			continue
		}
		if result.candidate.actual != nil {
			candidates = append(candidates, result.candidate)
			if verbose {
				fmt.Printf("Running state: %s (%s) has %d service(s)\n", result.serverName, result.host, len(result.candidate.actual.Services))
			}
		} else if verbose {
			fmt.Printf("Running state: %s (%s) has no services\n", result.serverName, result.host)
		}
	}
	if len(candidates) == 0 && len(readErrors) > 0 {
		sort.Strings(readErrors)
		return nil, fmt.Errorf("failed to read running state from reachable node(s): %s", strings.Join(readErrors, "; "))
	}
	return candidates, nil
}

func actualSnapshotFromTakodActual(project string, environment string, node string, actual *takod.ActualStateResponse, capturedAt time.Time) *takodstate.ActualSnapshot {
	snapshot := &takodstate.ActualSnapshot{
		SchemaVersion: takodstate.SchemaVersion,
		Project:       project,
		Environment:   environment,
		Node:          node,
		Services:      make(map[string]takodstate.ActualService),
		CapturedAt:    capturedAt,
	}
	if actual == nil {
		return snapshot
	}

	for serviceName, service := range actual.Services {
		if service == nil {
			continue
		}
		name := service.Name
		if name == "" {
			name = serviceName
		}
		if name == "" {
			continue
		}
		key := serviceName
		if key == "" {
			key = name
		}
		replicas := service.Replicas
		if replicas == 0 && len(service.Containers) > 0 {
			replicas = len(service.Containers)
		}
		snapshot.Services[key] = takodstate.ActualService{
			Name:              name,
			Image:             service.Image,
			Replicas:          replicas,
			Containers:        append([]string(nil), service.Containers...),
			ConfigHash:        service.ConfigHash,
			RuntimeID:         service.RuntimeID,
			Persistent:        service.Persistent,
			CurrentRevision:   service.CurrentRevision,
			PreviousRevision:  service.PreviousRevision,
			DeployStrategy:    service.DeployStrategy,
			ActiveContainers:  append([]string(nil), service.ActiveContainers...),
			WarmingContainers: append([]string(nil), service.WarmingContainers...),
		}
	}
	return snapshot
}

func mergeActualRuntimeID(existing string, incoming string) string {
	if existing == incoming {
		return existing
	}
	return ""
}

func mergeActualOptionalLabel(existing string, incoming string) string {
	if incoming == "" {
		return existing
	}
	if existing == "" {
		return incoming
	}
	if existing == incoming {
		return existing
	}
	return ""
}

// ReconcileStateFromActualSnapshot reconstructs local state from a replicated
// takod actual snapshot. This is useful on a fresh machine when deployment
// history was lost but mesh runtime snapshots are still available.
func ReconcileStateFromActualSnapshot(cfg *config.Config, envName string, actual *takodstate.ActualSnapshot, notes string) (*localstate.DeploymentState, error) {
	if actual == nil || len(actual.Services) == 0 {
		return nil, fmt.Errorf("no actual services found for %s", cfg.Project.Name)
	}
	deployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("recovered-%d", time.Now().Unix()),
		Timestamp:       time.Now(),
		Environment:     envName,
		Mode:            config.RuntimeModeTakod,
		Status:          "recovered",
		DurationSeconds: 0,
		Services:        make(map[string]*localstate.ServiceDeploy),
		Notes:           notes,
	}

	for serviceName, service := range actual.Services {
		deployment.Services[serviceName] = &localstate.ServiceDeploy{
			Image:    service.Image,
			Replicas: service.Replicas,
			Health:   "unknown",
		}
	}

	if len(deployment.Services) == 0 {
		return nil, fmt.Errorf("no services found in takod actual state for %s", cfg.Project.Name)
	}

	return deployment, nil
}

func saveRecoveredDeployment(cfg *config.Config, envName string, deployment *localstate.DeploymentState) error {
	result, err := saveRecoveredDeploymentResult(cfg, envName, deployment)
	if err != nil {
		return err
	}
	fmt.Printf("Recovered state from %d running service(s)\n", result.ServiceCount)
	return nil
}

func saveRecoveredDeploymentResult(cfg *config.Config, envName string, deployment *localstate.DeploymentState) (engine.StatePullRecoveryResult, error) {
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return engine.StatePullRecoveryResult{}, fmt.Errorf("failed to initialize local state: %w", err)
	}

	if err := localMgr.SaveDeployment(deployment); err != nil {
		return engine.StatePullRecoveryResult{}, fmt.Errorf("failed to save recovered state: %w", err)
	}

	return engine.StatePullRecoveryResult{ServiceCount: len(deployment.Services)}, nil
}

// ExportState exports current state to a JSON file
func ExportState(outputPath string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return fmt.Errorf("failed to initialize local state: %w", err)
	}

	// Get current deployment
	currentDep, err := localMgr.GetCurrentDeployment()
	if err != nil {
		return fmt.Errorf("failed to get current deployment: %w", err)
	}

	if currentDep == nil {
		return fmt.Errorf("no deployment state found")
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(currentDep, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write to file or stdout
	if outputPath == "-" {
		fmt.Println(string(data))
	} else {
		if err := fileutil.WriteFileAtomic(outputPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
		fmt.Printf("State exported to %s\n", outputPath)
	}

	return nil
}

// ImportState imports state from a JSON file
func ImportState(inputPath string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Read file
	var data []byte
	if inputPath == "-" {
		// Read from stdin
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(inputPath)
	}
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Unmarshal
	var deployment localstate.DeploymentState
	if err := json.Unmarshal(data, &deployment); err != nil {
		return fmt.Errorf("failed to parse state: %w", err)
	}

	// Initialize local state manager
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return fmt.Errorf("failed to initialize local state: %w", err)
	}

	// Save deployment
	if err := localMgr.SaveDeployment(&deployment); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	fmt.Printf("State imported from %s\n", inputPath)
	return nil
}
