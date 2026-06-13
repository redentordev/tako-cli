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
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var stateCmd = &cobra.Command{
	Use:   "state",
	Short: "Manage deployment state synchronization",
	Long: `Manage deployment state between local .tako directory and remote server.

When you clone a project or work on a different machine, local state may be missing.
Use 'tako state pull' to sync remote state to your local machine.`,
}

var statePullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull remote state to local .tako directory",
	Long: `Pull deployment state from the remote server to your local .tako directory.

This is useful when:
  - Cloning a project that has already been deployed
  - Working on a different machine
  - Recovering local state after accidental deletion

The command will:
  1. Connect to the remote server
  2. Read deployment history from takod state
  3. Refresh local deployment records in .tako without touching local secrets`,
	RunE: runStatePull,
}

var stateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show state synchronization status",
	Long: `Show the current state of local and remote deployment state.

Displays:
  - Whether local .tako directory exists
  - Last local deployment information
  - Last remote deployment information
  - Whether a remote operation lease is currently held
  - Whether sync is needed`,
	RunE: runStateStatus,
}

var stateRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Repair deployment and runtime state across reachable mesh nodes",
	Long: `Repair deployment and runtime state across reachable nodes in the active environment.

The command reads deployment history, desired revisions, and actual snapshots
from every reachable environment node. It chooses the freshest copies, writes
them back to all reachable nodes under the remote operation lease, and refreshes
local .tako deployment state when deployment history is available.

Use this when nodes disagree, a primary node was replaced, or a new laptop needs
the best available state before deploying.`,
	RunE: runStateRepair,
}

var stateServer string

func init() {
	rootCmd.AddCommand(stateCmd)
	stateCmd.AddCommand(statePullCmd)
	stateCmd.AddCommand(stateStatusCmd)
	stateCmd.AddCommand(stateRepairCmd)

	statePullCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Pull state from specific server")

	stateStatusCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Check status for specific server")

	stateRepairCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Prefer this server when acquiring the repair lease")
}

func runStatePull(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	firstServerName, firstServer, err := resolveStateServer(cfg, envName, stateServer)
	if err != nil {
		return err
	}

	fmt.Printf("Connecting to %s (%s)...\n", firstServerName, firstServer.Host)

	// Create SSH client
	client, err := ssh.NewClientWithAuth(firstServer.Host, firstServer.Port, firstServer.User, firstServer.SSHKey, firstServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w\n\nTroubleshooting:\n  - Check SSH key exists: ls -la %s\n  - Test manually: ssh -i %s %s@%s -p %d\n  - Verify server is reachable: ping %s",
			err, firstServer.SSHKey, firstServer.SSHKey, firstServer.User, firstServer.Host, firstServer.Port, firstServer.Host)
	}
	defer client.Close()

	// Verify SSH connectivity
	verifyOutput, verifyErr := client.Execute("echo 'tako-ok'")
	if verifyErr != nil || strings.TrimSpace(verifyOutput) != "tako-ok" {
		return fmt.Errorf("SSH connection established but command execution failed: %v\n\nTroubleshooting:\n  - Check SSH key: %s\n  - Test manually: ssh %s@%s -p %d echo ok",
			verifyErr, firstServer.SSHKey, firstServer.User, firstServer.Host, firstServer.Port)
	}

	if verbose {
		fmt.Println("SSH connectivity verified")
	}

	remoteMgr := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, firstServer.Host, takodSocketFromConfig(cfg))
	history, historyErr := remoteMgr.LoadHistory()

	if historyErr != nil || history == nil || len(history.Deployments) == 0 {
		// Try recovering from mesh peers if multi-server
		if cfg.IsMultiServer() {
			replicaPool := ssh.NewPool()
			defer replicaPool.CloseAll()
			envName := getEnvironmentName(cfg)
			replicator := remotestate.NewStateReplicator(replicaPool, cfg, envName, cfg.Project.Name, verbose)
			if history, source, _ := replicator.RecoverStateFromPeers(); history != nil && len(history.Deployments) > 0 {
				fmt.Printf("State recovered from node: %s\n", source)

				// Restore to primary node
				primaryMgr := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, client.Host(), takodSocketFromConfig(cfg))
				_ = primaryMgr.SaveHistory(history)

				// Now sync to local
				localMgr, lErr := localstate.NewManager(".", cfg.Project.Name, envName)
				if lErr == nil {
					if _, syncErr := syncRemoteDeploymentsToLocal(localMgr, history.Deployments, envName); syncErr != nil {
						return fmt.Errorf("failed to sync recovered state locally: %w", syncErr)
					}
				}

				fmt.Printf("Recovered and synced %d deployment(s)\n", len(history.Deployments))
				return nil
			}
		}

		// No remote state and peer recovery failed; try reconciling from running services.
		fmt.Println("No remote state found, attempting recovery from running services...")
		if err := reconcileAndSave(client, cfg, envName); err == nil {
			return nil
		}
		fmt.Println("\nNo remote state or running services found.")
		fmt.Println("This project has not been deployed yet, or state was cleaned up.")
		fmt.Println("\nRun 'tako deploy' to create initial deployment.")
		return nil
	}

	fmt.Printf("Found takod deployment history on %s\n", firstServerName)

	// Initialize local state manager
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return fmt.Errorf("failed to initialize local state: %w", err)
	}

	// Get remote history
	fmt.Println("Fetching remote deployment history...")
	remoteDeployments, err := remoteMgr.ListDeployments(&remotestate.HistoryOptions{
		Limit:         20,
		IncludeFailed: true,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch remote history: %w", err)
	}

	if len(remoteDeployments) == 0 {
		fmt.Println("No deployments found in remote state, attempting recovery from running services...")
		return reconcileAndSave(client, cfg, envName)
	}

	fmt.Printf("Found %d deployment(s) in remote state\n\n", len(remoteDeployments))

	synced, err := syncRemoteDeploymentsToLocal(localMgr, remoteDeployments, envName)
	if err != nil {
		return err
	}

	fmt.Printf("Synced %d deployment(s) to local .tako directory\n", synced)

	// Show latest deployment info
	latest := remoteDeployments[0]
	fmt.Printf("\nLatest deployment:\n")
	fmt.Printf("  ID:      %s\n", remotestate.FormatDeploymentID(latest.ID))
	fmt.Printf("  Status:  %s\n", latest.Status)
	fmt.Printf("  Time:    %s (%s ago)\n", latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(latest.Timestamp)))
	fmt.Printf("  User:    %s\n", latest.User)
	if latest.GitCommitShort != "" {
		fmt.Printf("  Commit:  %s\n", latest.GitCommitShort)
	}

	fmt.Println("\nLocal state is now synchronized with remote.")
	return nil
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

	// Check remote state
	fmt.Println("=== Remote State ===")

	firstServerName, firstServer, err := resolveStateServer(cfg, envName, stateServer)
	if err != nil {
		fmt.Printf("Status: %v\n", err)
		return nil
	}

	fmt.Printf("Server: %s (%s)\n", firstServerName, firstServer.Host)

	// Create SSH client
	client, err := ssh.NewClientWithAuth(firstServer.Host, firstServer.Port, firstServer.User, firstServer.SSHKey, firstServer.Password)
	if err != nil {
		fmt.Printf("Status: Cannot connect - %v\n", err)
		return nil
	}
	defer client.Close()

	remoteMgr := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, firstServer.Host, takodSocketFromConfig(cfg))

	remoteDeployments, err := remoteMgr.ListDeployments(&remotestate.HistoryOptions{
		Limit:         1,
		IncludeFailed: true,
	})
	if err != nil || len(remoteDeployments) == 0 {
		fmt.Println("Status: No remote state (project not deployed yet)")
	} else {
		fmt.Println("Remote state: takod history found")
		latest := remoteDeployments[0]
		fmt.Printf("Last deployment:\n")
		fmt.Printf("  ID:      %s\n", remotestate.FormatDeploymentID(latest.ID))
		fmt.Printf("  Time:    %s (%s ago)\n", latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(latest.Timestamp)))
		fmt.Printf("  Status:  %s\n", latest.Status)
		if latest.GitCommitShort != "" {
			fmt.Printf("  Commit:  %s\n", latest.GitCommitShort)
		}
	}

	fmt.Println()
	printTakodAgentStatus(client, cfg)
	printTakodRuntimeStatus(takodstate.NewManager(client, cfg, envName))
	printMeshRuntimeStatus(client, cfg)

	lease, err := remoteMgr.ReadLease()
	if err != nil {
		fmt.Printf("Lease: Error reading lease - %v\n", err)
	} else if lease == nil {
		fmt.Println("Lease: free")
	} else {
		fmt.Printf("Lease: held by %s\n", lease.Who)
		fmt.Printf("  Operation: %s\n", lease.Operation)
		fmt.Printf("  Created:   %s (%s ago)\n", lease.CreatedAt.Format(time.RFC3339), formatStateDuration(time.Since(lease.CreatedAt)))
		fmt.Printf("  Expires:   %s (in %s)\n", lease.ExpiresAt.Format(time.RFC3339), time.Until(lease.ExpiresAt).Round(time.Second))
	}

	fmt.Println()

	// Sync recommendation
	fmt.Println("=== Sync Status ===")
	if !localExists {
		fmt.Println("Local state is missing.")
		fmt.Println("Run 'tako state pull' to sync from remote server.")
	} else {
		fmt.Println("Local state exists.")
		fmt.Println("Run 'tako state pull' to refresh local deployment records from remote state.")
	}

	return nil
}

func runStateRepair(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)
	fmt.Printf("Project: %s\n", cfg.Project.Name)
	fmt.Printf("Environment: %s\n\n", envName)

	repair, err := collectStateRepairNodes(cfg, envName, stateServer)
	if err != nil {
		return err
	}
	defer closeStateRepairNodes(repair.nodes)

	if len(repair.nodes) == 0 {
		return fmt.Errorf("no reachable environment nodes found")
	}

	bestHistory, hasHistory := bestDeploymentHistory(repair.histories)
	bestDesired, hasDesired := bestDesiredRevision(repair.desired)
	bestActual, hasActual := bestActualSnapshot(repair.actual)
	if !hasHistory && !hasDesired && !hasActual {
		return fmt.Errorf("no deployment history or runtime state found on reachable nodes")
	}

	printStateRepairSource(hasHistory, bestHistory, hasDesired, bestDesired, hasActual, bestActual)

	repairLeases, err := acquireStateRepairLeases(repair.nodes, envName)
	if err != nil {
		return err
	}
	defer releaseStateRepairLeases(repairLeases, verbose)
	if verbose {
		fmt.Printf("Acquired state repair leases: %s\n", stateRepairLeaseSummary(repairLeases))
	}

	historyWritten, desiredWritten, actualWritten, err := writeStateRepairDocuments(repair.nodes, bestHistory, hasHistory, bestDesired, hasDesired, bestActual, hasActual)
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

	printStateRepairWriteSummary(len(repair.nodes), hasHistory, historyWritten, hasDesired, desiredWritten, hasActual, actualWritten)
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

func sortedServiceNames[T any](services map[string]T) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveStateServer(cfg *config.Config, envName string, requestedServer string) (string, config.ServerConfig, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return "", config.ServerConfig{}, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return "", config.ServerConfig{}, fmt.Errorf("no servers configured for environment %s", envName)
	}

	if requestedServer == "" {
		serverName := envServers[0]
		server, ok := cfg.Servers[serverName]
		if !ok {
			return "", config.ServerConfig{}, fmt.Errorf("server %s not found in configuration", serverName)
		}
		return serverName, server, nil
	}

	server, ok := cfg.Servers[requestedServer]
	if !ok {
		return "", config.ServerConfig{}, fmt.Errorf("server %s not found in configuration", requestedServer)
	}
	for _, serverName := range envServers {
		if serverName == requestedServer {
			return requestedServer, server, nil
		}
	}

	return "", config.ServerConfig{}, fmt.Errorf("server %s is not part of environment %s", requestedServer, envName)
}

type stateRepairNode struct {
	name    string
	client  *ssh.Client
	manager *remotestate.StateManager
	runtime *takodstate.Manager
}

type stateRepairLease struct {
	serverName string
	manager    *remotestate.StateManager
	lease      *remotestate.LeaseInfo
}

type stateRepairInventory struct {
	nodes     []stateRepairNode
	histories []stateHistoryCandidate
	desired   []stateDesiredCandidate
	actual    []stateActualCandidate
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

func collectStateRepairNodes(cfg *config.Config, envName string, preferredServer string) (*stateRepairInventory, error) {
	serverNames, err := orderedStateServerNames(cfg, envName, preferredServer)
	if err != nil {
		return nil, err
	}

	repair := &stateRepairInventory{
		nodes:     make([]stateRepairNode, 0, len(serverNames)),
		histories: make([]stateHistoryCandidate, 0, len(serverNames)),
		desired:   make([]stateDesiredCandidate, 0, len(serverNames)),
		actual:    make([]stateActualCandidate, 0, len(serverNames)),
	}

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			closeStateRepairNodes(repair.nodes)
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		fmt.Printf("Checking %s (%s)...\n", serverName, server.Host)
		client, err := ssh.NewClientWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot connect to %s: %v\n", serverName, err)
			continue
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
		runtime := takodstate.NewManager(client, cfg, envName)
		repair.nodes = append(repair.nodes, stateRepairNode{
			name:    serverName,
			client:  client,
			manager: manager,
			runtime: runtime,
		})

		history, err := manager.LoadHistory()
		if err != nil || !historyHasDeployments(history) {
			if verbose {
				fmt.Printf("No deployment history found on %s\n", serverName)
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
		} else if verbose && err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			fmt.Printf("Unable to read actual runtime state on %s: %v\n", serverName, err)
		}
	}

	return repair, nil
}

func closeStateRepairNodes(nodes []stateRepairNode) {
	for _, node := range nodes {
		if node.client != nil {
			_ = node.client.Close()
		}
	}
}

func acquireStateRepairLeases(nodes []stateRepairNode, envName string) ([]stateRepairLease, error) {
	leases := make([]stateRepairLease, 0, len(nodes))
	for _, node := range nodes {
		lease, err := node.manager.AcquireLease("state-repair", envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			releaseStateRepairLeases(leases, false)
			return nil, fmt.Errorf("failed to acquire repair lease on %s: %w", node.name, err)
		}
		leases = append(leases, stateRepairLease{
			serverName: node.name,
			manager:    node.manager,
			lease:      lease,
		})
	}
	return leases, nil
}

func releaseStateRepairLeases(leases []stateRepairLease, verbose bool) {
	for i := len(leases) - 1; i >= 0; i-- {
		lease := leases[i]
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

func printStateRepairSource(hasHistory bool, history stateHistoryCandidate, hasDesired bool, desired stateDesiredCandidate, hasActual bool, actual stateActualCandidate) {
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
}

func writeStateRepairDocuments(nodes []stateRepairNode, history stateHistoryCandidate, hasHistory bool, desired stateDesiredCandidate, hasDesired bool, actual stateActualCandidate, hasActual bool) (int, int, int, error) {
	historyWritten := 0
	desiredWritten := 0
	actualWritten := 0

	for _, node := range nodes {
		if hasHistory {
			historyCopy, err := cloneRemoteDeploymentHistory(history.history)
			if err != nil {
				return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to prepare history for repair: %w", err)
			}
			if err := node.manager.SaveHistory(historyCopy); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to repair deployment history on %s: %v\n", node.name, err)
			} else {
				historyWritten++
			}
		}

		if hasDesired {
			desiredCopy, err := cloneDesiredRevision(desired.desired)
			if err != nil {
				return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to prepare desired runtime state for repair: %w", err)
			}
			if err := node.runtime.WriteDesired(desiredCopy); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to repair desired runtime state on %s: %v\n", node.name, err)
			} else {
				desiredWritten++
			}
		}

		if hasActual {
			actualCopy, err := cloneActualSnapshot(actual.actual)
			if err != nil {
				return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to prepare actual runtime state for repair: %w", err)
			}
			if err := node.runtime.WriteActual(actualCopy); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to repair actual runtime state on %s: %v\n", node.name, err)
			} else {
				actualWritten++
			}
		}
	}

	if hasHistory && historyWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to write repaired deployment history to any reachable node")
	}
	if hasDesired && desiredWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to write repaired desired runtime state to any reachable node")
	}
	if hasActual && actualWritten == 0 {
		return historyWritten, desiredWritten, actualWritten, fmt.Errorf("failed to write repaired actual runtime state to any reachable node")
	}
	return historyWritten, desiredWritten, actualWritten, nil
}

func printStateRepairWriteSummary(nodeCount int, hasHistory bool, historyWritten int, hasDesired bool, desiredWritten int, hasActual bool, actualWritten int) {
	if hasHistory {
		fmt.Printf("Repaired deployment history on %d/%d reachable node(s)\n", historyWritten, nodeCount)
	}
	if hasDesired {
		fmt.Printf("Repaired desired runtime state on %d/%d reachable node(s)\n", desiredWritten, nodeCount)
	}
	if hasActual {
		fmt.Printf("Repaired actual runtime state on %d/%d reachable node(s)\n", actualWritten, nodeCount)
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

// SyncStateOnDeploy attempts to sync state when local deployment state is missing.
// This is called automatically during deploy when the local cache has no current deployment.
func SyncStateOnDeploy(cfg *config.Config, client *ssh.Client, envName string) error {
	if localDeploymentStateExists(envName) {
		return nil // Local deployment cache exists, no sync needed
	}

	if verbose {
		fmt.Println("Local state missing, checking remote...")
	}

	remoteMgr := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, client.Host(), takodSocketFromConfig(cfg))
	remoteDeployments, historyErr := remoteMgr.ListDeployments(&remotestate.HistoryOptions{
		Limit:         5,
		IncludeFailed: false,
	})

	if historyErr != nil || len(remoteDeployments) == 0 {
		// Try recovering from mesh peers if multi-server
		if cfg.IsMultiServer() {
			replicaPool := ssh.NewPool()
			defer replicaPool.CloseAll()
			replicator := remotestate.NewStateReplicator(replicaPool, cfg, envName, cfg.Project.Name, verbose)
			if history, source, _ := replicator.RecoverStateFromPeers(); history != nil && len(history.Deployments) > 0 {
				if verbose {
					fmt.Printf("Recovering state from node: %s\n", source)
				}

				// Restore to primary node
				primaryMgr := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, client.Host(), takodSocketFromConfig(cfg))
				_ = primaryMgr.SaveHistory(history)

				// Sync to local
				localMgr, lErr := localstate.NewManager(".", cfg.Project.Name, envName)
				if lErr == nil {
					if _, syncErr := syncRemoteDeploymentsToLocal(localMgr, history.Deployments, envName); syncErr != nil && verbose {
						fmt.Printf("Warning: failed to sync recovered state locally: %v\n", syncErr)
					}
				}

				if verbose {
					fmt.Printf("Recovered %d deployment(s) from node %s\n", len(history.Deployments), source)
				}
				return nil
			}
		}

		return nil // No remote state, nothing to sync
	}

	if verbose {
		fmt.Println("Found remote state, syncing to local...")
	}

	// Initialize local state manager
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return nil // Ignore error, continue without sync
	}

	// Sync recent deployments
	if _, err := syncRemoteDeploymentsToLocal(localMgr, remoteDeployments, envName); err != nil && verbose {
		fmt.Printf("Warning: failed to sync remote state locally: %v\n", err)
	}

	if verbose {
		fmt.Printf("Synced %d deployment(s) from remote\n", len(remoteDeployments))
	}

	return nil
}

// ReconcileStateFromRunning reconstructs state from running takod containers.
// This is useful when local state is lost but containers are still running.
func ReconcileStateFromRunning(client *ssh.Client, cfg *config.Config, envName string) (*localstate.DeploymentState, error) {
	actual, err := actualStateViaTakod(client, cfg, envName)
	if err != nil {
		return nil, fmt.Errorf("failed to read actual state from takod: %w", err)
	}

	if len(actual.Services) == 0 {
		return nil, fmt.Errorf("no running containers found for %s", cfg.Project.Name)
	}

	deployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("recovered-%d", time.Now().Unix()),
		Timestamp:       time.Now(),
		Environment:     envName,
		Mode:            config.RuntimeModeTakod,
		Status:          "recovered",
		DurationSeconds: 0,
		Services:        make(map[string]*localstate.ServiceDeploy),
		Notes:           "State recovered from running takod containers",
	}

	for serviceName, service := range actual.Services {
		if service == nil {
			continue
		}
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

// reconcileAndSave uses ReconcileStateFromRunning to rebuild state from running services
func reconcileAndSave(client *ssh.Client, cfg *config.Config, envName string) error {
	deployment, err := ReconcileStateFromRunning(client, cfg, envName)
	if err != nil {
		return err
	}

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
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
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
