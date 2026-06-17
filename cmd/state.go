package cmd

import (
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
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/mesh"
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

var (
	syncStateCollectDeploymentHistories = collectStateDeploymentHistoriesWithPool
	syncStateRecoverFromMeshActual      = recoverAndSaveStateFromMeshActualWithPool
	syncStateRecoverFromRunningMesh     = recoverAndSaveStateFromRunningMeshWithPool
)

const stateStatusRequestTimeout = 10 * time.Second

func init() {
	rootCmd.AddCommand(stateCmd)
	stateCmd.AddCommand(statePullCmd)
	stateCmd.AddCommand(stateStatusCmd)
	stateCmd.AddCommand(stateRepairCmd)
	stateCmd.AddCommand(stateLeaseCmd)
	stateLeaseCmd.AddCommand(stateLeaseReleaseCmd)

	statePullCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Pull state from a specific server instead of the full mesh")

	stateStatusCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Check status for a specific server instead of the full mesh")

	stateRepairCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Prefer this server when acquiring the repair lease")

	stateLeaseCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Show a specific server lease instead of the full mesh")
	stateLeaseReleaseCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Release a lease on a specific server instead of the full mesh")
	stateLeaseReleaseCmd.Flags().StringVar(&stateLeaseID, "id", "", "Exact remote lease ID to release")
	stateLeaseReleaseCmd.Flags().BoolVar(&stateLeaseForce, "force", false, "Release a matching non-expired lease")
}

func runStatePull(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	histories, err := collectStatePullHistories(cfg, envName, stateServer)
	if err != nil {
		return err
	}
	bestHistory, hasHistory := bestDeploymentHistory(histories)
	if hasHistory {
		fmt.Printf("Selected deployment history from %s\n", bestHistory.source)

		localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
		if err != nil {
			return fmt.Errorf("failed to initialize local state: %w", err)
		}
		synced, err := syncRemoteDeploymentsToLocal(localMgr, bestHistory.history.Deployments, envName)
		if err != nil {
			return err
		}
		fmt.Printf("Synced %d deployment(s) to local .tako directory\n", synced)

		latest := latestDeploymentByTimestamp(bestHistory.history.Deployments)
		if latest != nil {
			fmt.Printf("\nLatest deployment:\n")
			fmt.Printf("  ID:      %s\n", remotestate.FormatDeploymentID(latest.ID))
			fmt.Printf("  Status:  %s\n", latest.Status)
			fmt.Printf("  Time:    %s (%s ago)\n", latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(latest.Timestamp)))
			fmt.Printf("  User:    %s\n", latest.User)
			if latest.GitCommitShort != "" {
				fmt.Printf("  Commit:  %s\n", latest.GitCommitShort)
			}
		}
		fmt.Println("\nLocal state is now synchronized with remote.")
		return nil
	}

	fmt.Println("No remote deployment history found, attempting recovery from mesh runtime state...")
	if err := recoverAndSaveStateFromMeshActual(cfg, envName, stateServer); err == nil {
		return nil
	} else if verbose {
		fmt.Printf("Warning: mesh runtime state recovery failed: %v\n", err)
	}

	fmt.Println("No mesh runtime state found, attempting recovery from running services across reachable nodes...")
	if err := recoverAndSaveStateFromRunningMesh(cfg, envName, stateServer); err == nil {
		return nil
	} else if verbose {
		fmt.Printf("Warning: running service recovery failed: %v\n", err)
	}
	fmt.Println("\nNo remote state or running services found.")
	fmt.Println("This project has not been deployed yet, or state was cleaned up.")
	fmt.Println("\nRun 'tako deploy' to create initial deployment.")
	return nil
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
			client, cleanup, err := connectAndVerifyStateServerWithPool(pool, serverName, server)
			if err != nil {
				result.err = err
				resultCh <- result
				return
			}
			defer cleanup()

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

func statePullServerNames(cfg *config.Config, envName string, requestedServer string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, fmt.Errorf("no servers configured for environment %s", envName)
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
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	fmt.Printf("Project: %s\n", cfg.Project.Name)
	fmt.Printf("Environment: %s\n\n", envName)

	// Check local state
	fmt.Println("=== Local State ===")
	localPath := ".tako"
	localExists := false
	var localCurrent *localstate.DeploymentState

	if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		localExists = true
		fmt.Printf("Directory: %s (exists)\n", localPath)

		// Try to load local state
		localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
		if err != nil {
			fmt.Printf("Status: Error loading state: %v\n", err)
		} else {
			currentDep, err := localMgr.GetCurrentDeployment()
			if err != nil || currentDep == nil {
				fmt.Println("Status: No deployments recorded locally")
			} else {
				localCurrent = currentDep
				fmt.Printf("Last deployment:\n")
				fmt.Printf("  ID:      %s\n", currentDep.DeploymentID)
				fmt.Printf("  Time:    %s (%s ago)\n", currentDep.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(currentDep.Timestamp)))
				fmt.Printf("  Status:  %s\n", currentDep.Status)
				if currentDep.GitCommit != "" {
					fmt.Printf("  Commit:  %s\n", currentDep.GitCommit[:min(7, len(currentDep.GitCommit))])
				}
			}
		}
	} else {
		fmt.Printf("Directory: %s (missing)\n", localPath)
		fmt.Println("Status: No local state")
	}

	fmt.Println()

	// Check remote state across the mesh by default. A requested --server keeps
	// this command useful for focused one-node debugging.
	if stateServer == "" {
		fmt.Println("=== Remote Mesh State ===")
	} else {
		fmt.Println("=== Remote State ===")
	}

	remoteNodes, err := collectStateStatusNodes(cfg, envName, stateServer)
	if err != nil {
		return err
	}
	printStateStatusNodes(remoteNodes, cfg)

	histories, desiredCandidates, actualCandidates, nodeActualCandidates := stateStatusCandidates(remoteNodes)
	bestHistory, hasRemoteHistory := bestDeploymentHistory(histories)
	bestDesired, hasDesired := bestDesiredRevision(desiredCandidates)
	bestActual, hasActual, bestNodeActual := bestStateStatusActual(cfg.Project.Name, envName, actualCandidates, nodeActualCandidates)

	fmt.Println()
	printBestKnownState(bestHistory, hasRemoteHistory, bestDesired, hasDesired, bestActual, hasActual, bestNodeActual)

	fmt.Println()

	// Sync recommendation
	fmt.Println("=== Sync Status ===")
	for _, line := range stateSyncRecommendation(localExists, localCurrent, bestHistory, hasRemoteHistory) {
		fmt.Println(line)
	}

	if stateStatusReachableCount(remoteNodes) == 0 {
		return stateStatusNoReachableError(envName, remoteNodes)
	}

	return nil
}

func stateSyncRecommendation(localExists bool, localCurrent *localstate.DeploymentState, bestHistory stateHistoryCandidate, hasRemoteHistory bool) []string {
	lines := make([]string, 0, 4)
	if !localExists {
		lines = append(lines, "Local state is missing.")
		if hasRemoteHistory {
			lines = append(lines, fmt.Sprintf("Remote deployment history is available from %s.", bestHistory.source))
			lines = append(lines, "Run 'tako state pull' to sync from the freshest reachable node.")
			return lines
		}
		lines = append(lines, "No remote deployment history was found on reachable nodes.")
		return lines
	}

	lines = append(lines, "Local state exists.")
	if localCurrent == nil {
		lines = append(lines, "No current local deployment is recorded.")
		if hasRemoteHistory {
			lines = append(lines, fmt.Sprintf("Remote deployment history is available from %s.", bestHistory.source))
			lines = append(lines, "Run 'tako state pull' to sync from the freshest reachable node.")
		} else {
			lines = append(lines, "No remote deployment history was found on reachable nodes.")
		}
		return lines
	}

	if !hasRemoteHistory {
		lines = append(lines, "No remote deployment history was found on reachable nodes; local deployment records are the best known copy.")
		return lines
	}

	remoteLatest := latestDeploymentByTimestamp(bestHistory.history.Deployments)
	if remoteLatest == nil {
		lines = append(lines, "No remote deployment history was found on reachable nodes; local deployment records are the best known copy.")
		return lines
	}

	if localCurrent.DeploymentID == remoteLatest.ID {
		lines = append(lines, fmt.Sprintf("Local deployment records match the freshest reachable remote deployment from %s.", bestHistory.source))
		lines = append(lines, "No state pull needed.")
		return lines
	}

	if !localCurrent.Timestamp.IsZero() && !remoteLatest.Timestamp.IsZero() {
		if localCurrent.Timestamp.Before(remoteLatest.Timestamp) {
			lines = append(lines, fmt.Sprintf("Remote deployment history from %s is newer than local state.", bestHistory.source))
			lines = append(lines, "Run 'tako state pull' to refresh local deployment records.")
			return lines
		}
		if localCurrent.Timestamp.After(remoteLatest.Timestamp) {
			lines = append(lines, fmt.Sprintf("Local deployment records are newer than the freshest reachable remote history from %s.", bestHistory.source))
			lines = append(lines, "Check mesh reachability before pulling state.")
			return lines
		}
	}

	lines = append(lines, fmt.Sprintf("Local and remote deployment timestamps match, but deployment IDs differ from %s.", bestHistory.source))
	lines = append(lines, "Run 'tako state pull' to normalize local deployment records.")
	return lines
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
	pool := ssh.NewPool()
	defer pool.CloseAll()

	nodes, err := collectStateLeaseNodes(pool, cfg, envName, stateServer)
	if err != nil {
		return err
	}
	fmt.Printf("Project: %s\n", cfg.Project.Name)
	fmt.Printf("Environment: %s\n\n", envName)
	printStateLeaseNodes(nodes)
	return nil
}

func runStateLeaseRelease(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)
	pool := ssh.NewPool()
	defer pool.CloseAll()

	nodes, err := collectStateLeaseNodes(pool, cfg, envName, stateServer)
	if err != nil {
		return err
	}
	released, err := releaseStateLeaseByID(nodes, stateLeaseID, stateLeaseForce, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("Released lease %s on %d node(s): %s\n", strings.TrimSpace(stateLeaseID), len(released), strings.Join(released, ", "))
	return nil
}

func collectStateLeaseNodes(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) ([]stateLeaseNode, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	nodes := make([]stateLeaseNode, len(serverNames))
	resultCh := make(chan struct {
		index int
		node  stateLeaseNode
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
			node := stateLeaseNode{name: serverName, host: server.Host}
			client, cleanup, err := connectAndVerifyStateServerWithPool(pool, serverName, server)
			if err != nil {
				node.err = err
				resultCh <- struct {
					index int
					node  stateLeaseNode
				}{index: index, node: node}
				return
			}
			defer cleanup()

			manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
			node.manager = manager
			node.lease, node.err = manager.ReadLease()
			resultCh <- struct {
				index int
				node  stateLeaseNode
			}{index: index, node: node}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		nodes[result.index] = result.node
	}
	return nodes, nil
}

func printStateLeaseNodes(nodes []stateLeaseNode) {
	for _, node := range nodes {
		fmt.Printf("Node: %s (%s)\n", node.name, node.host)
		printStateStatusLease(node.lease, node.err)
		fmt.Println()
	}
}

func releaseStateLeaseByID(nodes []stateLeaseNode, leaseID string, force bool, now time.Time) ([]string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return nil, fmt.Errorf("--id is required")
	}

	var released []string
	var releaseErrors []string
	var nodeErrors []string
	found := false
	for _, node := range nodes {
		if node.err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", node.name, node.err))
			continue
		}
		if node.lease == nil || node.lease.ID != leaseID {
			continue
		}
		found = true
		if node.manager == nil {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: lease manager unavailable", node.name))
			continue
		}
		if !force && now.Before(node.lease.ExpiresAt) {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: lease has not expired yet; use --force to release it", node.name))
			continue
		}
		if err := node.manager.ReleaseLease(node.lease); err != nil {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: %v", node.name, err))
			continue
		}
		released = append(released, node.name)
	}
	if len(releaseErrors) > 0 {
		sort.Strings(releaseErrors)
		return released, fmt.Errorf("failed to release lease %s: %s", leaseID, strings.Join(releaseErrors, "; "))
	}
	if !found {
		if len(nodeErrors) > 0 {
			sort.Strings(nodeErrors)
			return nil, fmt.Errorf("lease %s not found on reachable nodes; unreachable nodes: %s", leaseID, strings.Join(nodeErrors, "; "))
		}
		return nil, fmt.Errorf("lease %s not found on reachable nodes", leaseID)
	}
	if len(released) == 0 {
		return nil, fmt.Errorf("lease %s was found but not released", leaseID)
	}
	return released, nil
}

func runStateRepair(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)
	fmt.Printf("Project: %s\n", cfg.Project.Name)
	fmt.Printf("Environment: %s\n\n", envName)

	pool := ssh.NewPool()
	defer pool.CloseAll()

	repair, err := collectStateRepairNodesWithPool(pool, cfg, envName, stateServer)
	if err != nil {
		return err
	}

	if len(repair.nodes) == 0 {
		return fmt.Errorf("no reachable environment nodes found")
	}

	bestHistory, hasHistory := bestDeploymentHistory(repair.histories)
	bestDesired, hasDesired := bestDesiredRevision(repair.desired)
	bestActual, hasActual := bestActualSnapshot(repair.actual)
	bestNodeActual := bestNodeActualSnapshots(repair.nodeActual)
	hasNodeActual := len(bestNodeActual) > 0
	if hasActual && hasNodeActual {
		bestActual.actual = actualSnapshotWithNodeSnapshots(bestActual.actual, bestNodeActual)
	} else if !hasActual && hasNodeActual {
		bestActual = stateActualCandidate{
			source: "node actual snapshots",
			actual: aggregateActualSnapshotFromNodeSnapshots(cfg.Project.Name, envName, bestNodeActual),
		}
		hasActual = actualSnapshotRepairable(bestActual.actual)
	}
	if !hasHistory && !hasDesired && !hasActual && !hasNodeActual {
		return fmt.Errorf("no deployment history or runtime state found on reachable nodes")
	}

	printStateRepairSource(hasHistory, bestHistory, hasDesired, bestDesired, hasActual, bestActual, bestNodeActual)

	repairLeases, err := acquireStateRepairLeases(repair.nodes, envName)
	if err != nil {
		return err
	}
	defer releaseStateRepairLeases(repairLeases, verbose)
	if verbose {
		fmt.Printf("Acquired state repair leases: %s\n", stateRepairLeaseSummary(repairLeases))
	}

	historyWritten, desiredWritten, actualWritten, nodeActualWritten, err := writeStateRepairDocuments(repair.nodes, bestHistory, hasHistory, bestDesired, hasDesired, bestActual, hasActual, bestNodeActual)
	if err != nil {
		return err
	}

	if hasHistory {
		localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
		if err != nil {
			return fmt.Errorf("remote state repaired, but local state initialization failed: %w", err)
		}
		synced, err := syncRemoteDeploymentsToLocal(localMgr, bestHistory.history.Deployments, envName)
		if err != nil {
			return fmt.Errorf("remote state repaired, but local state sync failed: %w", err)
		}
		fmt.Printf("Synced %d deployment(s) to local .tako directory\n", synced)
	}

	printStateRepairWriteSummary(len(repair.nodes), hasHistory, historyWritten, hasDesired, desiredWritten, hasActual, actualWritten, hasNodeActual, nodeActualWritten)
	return nil
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

func printTakodAgentStatus(client *ssh.Client, cfg *config.Config) {
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

func printMeshRuntimeStatus(client *ssh.Client, cfg *config.Config) {
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
				node:  collectStateStatusNode(pool, cfg, envName, serverName, server, envServerNames),
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

func collectStateStatusNode(pool *ssh.Pool, cfg *config.Config, envName string, serverName string, server config.ServerConfig, envServerNames []string) stateStatusNode {
	node := stateStatusNode{
		name:     serverName,
		host:     server.Host,
		envNodes: append([]string(nil), envServerNames...),
	}

	client, cleanup, err := connectAndVerifyStateServerWithPool(pool, serverName, server)
	if err != nil {
		node.connectErr = err
		return node
	}
	defer cleanup()

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

func stateStatusReachableCount(nodes []stateStatusNode) int {
	reachable := 0
	for _, node := range nodes {
		if node.connectErr == nil {
			reachable++
		}
	}
	return reachable
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

func readTakodAgentStatus(client *ssh.Client, cfg *config.Config) (*takodRemoteStatus, error) {
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

func readMeshRuntimeStatus(client *ssh.Client, cfg *config.Config) (*mesh.Status, error) {
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
		fmt.Printf("    - %s: %d replica(s), %s\n", name, service.Replicas, image)
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
	client  *ssh.Client
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
	WriteDesired(*takodstate.DesiredRevision) error
	WriteActual(*takodstate.ActualSnapshot) error
	WriteNodeActual(string, *takodstate.ActualSnapshot) error
	DeleteNodeActual(string) error
}

type stateRepairInventory struct {
	nodes      []stateRepairNode
	histories  []stateHistoryCandidate
	desired    []stateDesiredCandidate
	actual     []stateActualCandidate
	nodeActual []stateNodeActualCandidate
}

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

func collectStateRepairNodes(cfg *config.Config, envName string, preferredServer string) (*stateRepairInventory, error) {
	return collectStateRepairNodesWithPool(nil, cfg, envName, preferredServer)
}

func collectStateRepairNodesWithPool(pool *ssh.Pool, cfg *config.Config, envName string, preferredServer string) (*stateRepairInventory, error) {
	serverNames, err := orderedStateServerNames(cfg, envName, preferredServer)
	if err != nil {
		return nil, err
	}

	repair := &stateRepairInventory{
		nodes:      make([]stateRepairNode, 0, len(serverNames)),
		histories:  make([]stateHistoryCandidate, 0, len(serverNames)),
		desired:    make([]stateDesiredCandidate, 0, len(serverNames)),
		actual:     make([]stateActualCandidate, 0, len(serverNames)),
		nodeActual: make([]stateNodeActualCandidate, 0, len(serverNames)),
	}

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			closeStateRepairNodes(repair.nodes)
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		fmt.Printf("Checking %s (%s)...\n", serverName, server.Host)
		client, cleanup, err := connectAndVerifyStateServerWithPool(pool, serverName, server)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot connect to %s: %v\n", serverName, err)
			continue
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
		runtime := takodstate.NewManager(client, cfg, envName)
		repair.nodes = append(repair.nodes, stateRepairNode{
			name:    serverName,
			client:  client,
			cleanup: cleanup,
			manager: manager,
			runtime: runtime,
		})

		history, err := manager.LoadHistory()
		if errors.Is(err, remotestate.ErrNotFound) || !historyHasDeployments(history) {
			if verbose {
				fmt.Printf("No deployment history found on %s\n", serverName)
			}
		} else if err != nil {
			if verbose {
				fmt.Printf("Unable to read deployment history on %s: %v\n", serverName, err)
			}
		} else {
			repair.histories = append(repair.histories, stateHistoryCandidate{
				source:  serverName,
				history: history,
			})
			fmt.Printf("  history: %d deployment(s), freshness %s\n",
				deploymentHistoryCount(history),
				deploymentHistoryFreshness(history).Format(time.RFC3339),
			)
		}

		desired, err := runtime.ReadDesired()
		if err == nil && desiredRevisionRepairable(desired) {
			repair.desired = append(repair.desired, stateDesiredCandidate{
				source:  serverName,
				desired: desired,
			})
			fmt.Printf("  desired: %s, freshness %s\n",
				desired.RevisionID,
				desiredRevisionFreshness(desired).Format(time.RFC3339),
			)
		} else if verbose && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Unable to read desired runtime state on %s: %v\n", serverName, err)
		}

		actual, err := runtime.ReadActual()
		if err == nil && actualSnapshotRepairable(actual) {
			repair.actual = append(repair.actual, stateActualCandidate{
				source: serverName,
				actual: actual,
			})
			fmt.Printf("  actual: %d service(s), freshness %s\n",
				actualSnapshotServiceCount(actual),
				actualSnapshotFreshness(actual).Format(time.RFC3339),
			)
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
		} else if verbose && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
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
			} else if verbose && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
				fmt.Printf("Unable to read node actual runtime state for %s on %s: %v\n", nodeName, serverName, err)
			}
		}
		if nodeActualCount > 0 {
			fmt.Printf("  node actual: %d snapshot(s)\n", nodeActualCount)
		}
	}

	return repair, nil
}

func closeStateRepairNodes(nodes []stateRepairNode) {
	for _, node := range nodes {
		if node.cleanup != nil {
			node.cleanup()
		} else if node.client != nil {
			_ = node.client.Close()
		}
	}
}

func acquireStateRepairLeases(nodes []stateRepairNode, envName string) ([]stateRepairLease, error) {
	return acquireStateRepairLeasesWith(nodes, func(node stateRepairNode) (stateRepairLease, error) {
		lease, err := node.manager.AcquireLease("state-repair", envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			return stateRepairLease{}, fmt.Errorf("failed to acquire repair lease on %s: %w", node.name, err)
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

func writeStateRepairDocuments(nodes []stateRepairNode, history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool, nodeActual map[string]stateNodeActualCandidate) (int, int, int, int, error) {
	historyWritten := 0
	desiredWritten := 0
	actualWritten := 0
	nodeActualWritten := 0

	resultCh := make(chan stateRepairWriteResult, len(nodes))
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node stateRepairNode) {
			defer wg.Done()
			resultCh <- writeStateRepairDocumentsToNode(node, history, hasHistory, desired, hasDesired, actual, hasActual, nodeActual)
		}(node)
	}

	wg.Wait()
	close(resultCh)

	var warnings []string
	var fatalErrors []string
	for result := range resultCh {
		historyWritten += result.counts.history
		desiredWritten += result.counts.desired
		actualWritten += result.counts.actual
		nodeActualWritten += result.counts.nodeActual
		warnings = append(warnings, result.warnings...)
		if result.err != nil {
			fatalErrors = append(fatalErrors, fmt.Sprintf("%s: %v", result.nodeName, result.err))
		}
	}

	sort.Strings(warnings)
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}

	if len(fatalErrors) > 0 {
		sort.Strings(fatalErrors)
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("failed to prepare state repair documents: %s", strings.Join(fatalErrors, "; "))
	}

	if hasHistory && historyWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("failed to write repaired deployment history to any reachable node")
	}
	if hasDesired && desiredWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("failed to write repaired desired runtime state to any reachable node")
	}
	if hasActual && actualWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("failed to write repaired actual runtime state to any reachable node")
	}
	if len(nodeActual) > 0 && nodeActualWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("failed to write repaired node actual runtime state to any reachable node")
	}
	if len(warnings) > 0 {
		return historyWritten, desiredWritten, actualWritten, nodeActualWritten, fmt.Errorf("state repair incomplete: %s", strings.Join(warnings, "; "))
	}
	return historyWritten, desiredWritten, actualWritten, nodeActualWritten, nil
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
				snapshot.Services[serviceName] = existing
				continue
			}
			snapshot.Services[serviceName] = takodstate.ActualService{
				Name:       service.Name,
				Image:      service.Image,
				Replicas:   service.Replicas,
				Containers: append([]string(nil), service.Containers...),
				ConfigHash: service.ConfigHash,
				RuntimeID:  service.RuntimeID,
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
	nodes, err := collectStateStatusNodesWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return err
	}
	_, _, actualCandidates, nodeActualCandidates := stateStatusCandidates(nodes)
	bestActual, hasActual, _ := bestStateStatusActual(cfg.Project.Name, envName, actualCandidates, nodeActualCandidates)
	if !hasActual {
		return fmt.Errorf("no mesh actual state found")
	}

	deployment, err := ReconcileStateFromActualSnapshot(cfg, envName, bestActual.actual, "State recovered from replicated takod actual state")
	if err != nil {
		return err
	}
	return saveRecoveredDeployment(cfg, envName, deployment)
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
	candidates, err := collectRunningActualNodeSnapshotsWithPool(pool, cfg, envName, requestedServer)
	if err != nil {
		return err
	}
	bestNodeActual := bestNodeActualSnapshots(candidates)
	if len(bestNodeActual) == 0 {
		return fmt.Errorf("no running takod containers found on reachable mesh nodes")
	}
	aggregate := aggregateActualSnapshotFromNodeSnapshots(cfg.Project.Name, envName, bestNodeActual)
	deployment, err := ReconcileStateFromActualSnapshot(cfg, envName, aggregate, "State recovered from running takod containers across the mesh")
	if err != nil {
		return err
	}
	return saveRecoveredDeployment(cfg, envName, deployment)
}

func collectRunningActualNodeSnapshots(cfg *config.Config, envName string, requestedServer string) ([]stateNodeActualCandidate, error) {
	return collectRunningActualNodeSnapshotsWithPool(nil, cfg, envName, requestedServer)
}

func collectRunningActualNodeSnapshotsWithPool(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) ([]stateNodeActualCandidate, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}

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

			client, cleanup, err := connectAndVerifyStateServerWithPool(pool, serverName, server)
			if err != nil {
				result.err = err
				resultCh <- result
				return
			}
			defer cleanup()

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
			Name:       name,
			Image:      service.Image,
			Replicas:   replicas,
			Containers: append([]string(nil), service.Containers...),
			ConfigHash: service.ConfigHash,
			RuntimeID:  service.RuntimeID,
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
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return fmt.Errorf("failed to initialize local state: %w", err)
	}

	if err := localMgr.SaveDeployment(deployment); err != nil {
		return fmt.Errorf("failed to save recovered state: %w", err)
	}

	fmt.Printf("Recovered state from %d running service(s)\n", len(deployment.Services))

	return nil
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
