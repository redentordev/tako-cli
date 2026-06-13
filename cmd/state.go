package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
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
  2. Read deployment history from /var/lib/tako-cli/<project>
  3. Sync state to local .tako directory`,
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

var (
	stateForce  bool
	stateServer string
)

func init() {
	rootCmd.AddCommand(stateCmd)
	stateCmd.AddCommand(statePullCmd)
	stateCmd.AddCommand(stateStatusCmd)

	statePullCmd.Flags().BoolVarP(&stateForce, "force", "f", false, "Overwrite local state even if it exists")
	statePullCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Pull state from specific server")

	stateStatusCmd.Flags().StringVarP(&stateServer, "server", "s", "", "Check status for specific server")
}

func runStatePull(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Check if local state exists
	localPath := ".tako"
	localExists := false
	if _, err := os.Stat(localPath); err == nil {
		localExists = true
	}

	if localExists && !stateForce {
		fmt.Println("Local state already exists in .tako/")
		fmt.Println("Use --force to overwrite local state")
		fmt.Println("\nRun 'tako state status' to compare local and remote state")
		return nil
	}

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

	// Check if remote state exists
	remotePath := fmt.Sprintf("%s/%s", remotestate.StateDir, cfg.Project.Name)
	checkCmd := fmt.Sprintf("test -d %s && echo 'exists' || echo 'missing'", remotePath)
	output, err := client.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check remote state: %w", err)
	}

	if strings.TrimSpace(output) == "missing" {
		// Try recovering from mesh peers if multi-server
		if cfg.IsMultiServer() {
			replicaPool := ssh.NewPool()
			defer replicaPool.CloseAll()
			envName := getEnvironmentName(cfg)
			replicator := remotestate.NewStateReplicator(replicaPool, cfg, envName, cfg.Project.Name, verbose)
			if history, source, _ := replicator.RecoverStateFromPeers(); history != nil && len(history.Deployments) > 0 {
				fmt.Printf("State recovered from node: %s\n", source)

				// Restore to primary node
				primaryMgr := remotestate.NewStateManager(client, cfg.Project.Name, client.Host())
				primaryMgr.Initialize()
				for _, dep := range history.Deployments {
					primaryMgr.SaveDeployment(dep)
				}

				// Now sync to local
				localMgr, lErr := localstate.NewManager(".", cfg.Project.Name, envName)
				if lErr == nil {
					for _, dep := range history.Deployments {
						localDep := convertRemoteToLocal(dep, envName)
						localMgr.SaveDeployment(localDep)
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

	fmt.Printf("Found remote state at %s\n", remotePath)

	// Initialize local state manager
	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		return fmt.Errorf("failed to initialize local state: %w", err)
	}

	// Create remote state manager
	remoteMgr := remotestate.NewStateManager(client, cfg.Project.Name, firstServer.Host)

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

	// Sync each deployment to local state
	synced := 0
	for _, remoteDep := range remoteDeployments {
		// Convert remote deployment to local format
		localDep := convertRemoteToLocal(remoteDep, envName)

		// Save to local state
		if err := localMgr.SaveDeployment(localDep); err != nil {
			if verbose {
				fmt.Printf("Warning: failed to save deployment %s: %v\n", remoteDep.ID, err)
			}
			continue
		}
		synced++
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

	remoteMgr := remotestate.NewStateManager(client, cfg.Project.Name, firstServer.Host)

	// Check if remote state exists
	remotePath := fmt.Sprintf("%s/%s", remotestate.StateDir, cfg.Project.Name)
	checkCmd := fmt.Sprintf("test -d %s && echo 'exists' || echo 'missing'", remotePath)
	output, err := client.Execute(checkCmd)
	if err != nil {
		fmt.Printf("Status: Error checking - %v\n", err)
		return nil
	}

	if strings.TrimSpace(output) == "missing" {
		fmt.Printf("Directory: %s (missing)\n", remotePath)
		fmt.Println("Status: No remote state (project not deployed yet)")
	} else {
		fmt.Printf("Directory: %s (exists)\n", remotePath)

		// Get remote deployment info
		remoteDeployments, err := remoteMgr.ListDeployments(&remotestate.HistoryOptions{
			Limit:         1,
			IncludeFailed: true,
		})
		if err != nil {
			fmt.Printf("Status: Error reading - %v\n", err)
		} else if len(remoteDeployments) == 0 {
			fmt.Println("Status: No deployments recorded")
		} else {
			latest := remoteDeployments[0]
			fmt.Printf("Last deployment:\n")
			fmt.Printf("  ID:      %s\n", remotestate.FormatDeploymentID(latest.ID))
			fmt.Printf("  Time:    %s (%s ago)\n", latest.Timestamp.Format(time.RFC3339), formatStateDuration(time.Since(latest.Timestamp)))
			fmt.Printf("  Status:  %s\n", latest.Status)
			if latest.GitCommitShort != "" {
				fmt.Printf("  Commit:  %s\n", latest.GitCommitShort)
			}
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
		fmt.Println("Run 'tako state pull --force' to overwrite with remote state.")
	}

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

// SyncStateOnDeploy attempts to sync state when local state is missing
// This is called automatically during deploy when .tako doesn't exist
func SyncStateOnDeploy(cfg *config.Config, client *ssh.Client, envName string) error {
	localPath := ".tako"

	// Check if local state already exists
	if _, err := os.Stat(localPath); err == nil {
		return nil // Local state exists, no sync needed
	}

	if verbose {
		fmt.Println("Local state missing, checking remote...")
	}

	// Check if remote state exists
	remotePath := fmt.Sprintf("%s/%s", remotestate.StateDir, cfg.Project.Name)
	checkCmd := fmt.Sprintf("test -d %s && echo 'exists' || echo 'missing'", remotePath)
	output, err := client.Execute(checkCmd)
	if err != nil {
		return nil // Ignore error, continue without sync
	}

	if strings.TrimSpace(output) == "missing" {
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
				primaryMgr := remotestate.NewStateManager(client, cfg.Project.Name, client.Host())
				primaryMgr.Initialize()
				for _, dep := range history.Deployments {
					primaryMgr.SaveDeployment(dep)
				}

				// Sync to local
				localMgr, lErr := localstate.NewManager(".", cfg.Project.Name, envName)
				if lErr == nil {
					for _, dep := range history.Deployments {
						localDep := convertRemoteToLocal(dep, envName)
						localMgr.SaveDeployment(localDep)
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

	// Create remote state manager
	remoteMgr := remotestate.NewStateManager(client, cfg.Project.Name, client.Host())

	// Get latest deployment
	remoteDeployments, err := remoteMgr.ListDeployments(&remotestate.HistoryOptions{
		Limit:         5,
		IncludeFailed: false,
	})
	if err != nil || len(remoteDeployments) == 0 {
		return nil // Ignore error, continue without sync
	}

	// Sync recent deployments
	for _, remoteDep := range remoteDeployments {
		localDep := convertRemoteToLocal(remoteDep, envName)
		localMgr.SaveDeployment(localDep)
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
