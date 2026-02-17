package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/swarm"
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

	// Determine which server to connect to
	servers := cfg.Servers
	if stateServer != "" {
		server, exists := cfg.Servers[stateServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", stateServer)
		}
		servers = map[string]config.ServerConfig{stateServer: server}
	}

	// Get first server
	var firstServerName string
	var firstServer config.ServerConfig
	for name, srv := range servers {
		firstServerName = name
		firstServer = srv
		break
	}

	if firstServerName == "" {
		return fmt.Errorf("no servers configured")
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
		// Try recovering from worker nodes if multi-server
		if cfg.IsMultiServer() {
			replicaPool := ssh.NewPool()
			defer replicaPool.CloseAll()
			envName := getEnvironmentName(cfg)
			replicator := remotestate.NewStateReplicator(replicaPool, cfg, envName, cfg.Project.Name, verbose)
			if history, source, _ := replicator.RecoverStateFromWorkers(); history != nil && len(history.Deployments) > 0 {
				fmt.Printf("State recovered from worker: %s\n", source)

				// Restore to manager
				managerMgr := remotestate.NewStateManager(client, cfg.Project.Name, client.Host())
				managerMgr.Initialize()
				for _, dep := range history.Deployments {
					managerMgr.SaveDeployment(dep)
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

		// No remote state and worker recovery failed — try reconciling from running services
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

	// Attempt to recover swarm tokens
	recoverSwarmTokens(client, cfg, envName)

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

	// Determine which server to connect to
	servers := cfg.Servers
	if stateServer != "" {
		server, exists := cfg.Servers[stateServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", stateServer)
		}
		servers = map[string]config.ServerConfig{stateServer: server}
	}

	// Get first server
	var firstServerName string
	var firstServer config.ServerConfig
	for name, srv := range servers {
		firstServerName = name
		firstServer = srv
		break
	}

	if firstServerName == "" {
		fmt.Println("No servers configured")
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
		remoteMgr := remotestate.NewStateManager(client, cfg.Project.Name, firstServer.Host)
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

// convertRemoteToLocal converts a remote DeploymentState to local format
func convertRemoteToLocal(remote *remotestate.DeploymentState, env string) *localstate.DeploymentState {
	local := &localstate.DeploymentState{
		DeploymentID:    remote.ID,
		Timestamp:       remote.Timestamp,
		Environment:     env,
		Mode:            "swarm",
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
		// Try recovering from worker nodes if multi-server
		if cfg.IsMultiServer() {
			replicaPool := ssh.NewPool()
			defer replicaPool.CloseAll()
			replicator := remotestate.NewStateReplicator(replicaPool, cfg, envName, cfg.Project.Name, verbose)
			if history, source, _ := replicator.RecoverStateFromWorkers(); history != nil && len(history.Deployments) > 0 {
				if verbose {
					fmt.Printf("Recovering state from worker: %s\n", source)
				}

				// Restore to manager
				managerMgr := remotestate.NewStateManager(client, cfg.Project.Name, client.Host())
				managerMgr.Initialize()
				for _, dep := range history.Deployments {
					managerMgr.SaveDeployment(dep)
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
					fmt.Printf("Recovered %d deployment(s) from worker %s\n", len(history.Deployments), source)
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

// ReconcileStateFromRunning reconstructs state from running Docker services
// This is useful when state is completely lost but services are still running
func ReconcileStateFromRunning(client *ssh.Client, projectName, envName string) (*localstate.DeploymentState, error) {
	// List running services for this project
	prefix := fmt.Sprintf("%s_%s_", projectName, envName)
	listCmd := fmt.Sprintf("docker service ls --filter 'name=%s' --format '{{.Name}}\\t{{.Image}}\\t{{.Replicas}}'", prefix)

	output, err := client.Execute(listCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		return nil, fmt.Errorf("no running services found for %s", projectName)
	}

	// Parse services
	deployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("recovered-%d", time.Now().Unix()),
		Timestamp:       time.Now(),
		Environment:     envName,
		Mode:            "swarm",
		Status:          "recovered",
		DurationSeconds: 0,
		Services:        make(map[string]*localstate.ServiceDeploy),
		Notes:           "State recovered from running services",
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}

		fullName := parts[0]
		image := parts[1]
		replicas := parts[2]

		// Extract service name from full name (project_env_service)
		serviceName := strings.TrimPrefix(fullName, prefix)
		if serviceName == fullName {
			continue // Didn't match prefix
		}

		// Parse replicas (format: "1/1")
		var replicaCount int
		fmt.Sscanf(replicas, "%d/", &replicaCount)

		deployment.Services[serviceName] = &localstate.ServiceDeploy{
			Image:    image,
			Replicas: replicaCount,
			Health:   "unknown",
		}
	}

	if len(deployment.Services) == 0 {
		return nil, fmt.Errorf("no services found matching prefix %s", prefix)
	}

	return deployment, nil
}

// recoverSwarmTokens attempts to recover Docker Swarm tokens from the server
// and saves them as an encrypted swarm state file
func recoverSwarmTokens(client *ssh.Client, cfg *config.Config, envName string) {
	// Check if this node is a swarm manager
	output, err := client.Execute("docker info --format '{{.Swarm.ControlAvailable}}'")
	if err != nil || strings.TrimSpace(output) != "true" {
		if verbose {
			fmt.Println("Server is not a swarm manager, skipping swarm token recovery")
		}
		return
	}

	fmt.Println("Recovering swarm tokens...")

	// Get worker join token
	workerToken, err := client.Execute("docker swarm join-token worker -q")
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to get worker token: %v\n", err)
		}
		return
	}

	// Get manager join token
	managerToken, err := client.Execute("docker swarm join-token manager -q")
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to get manager token: %v\n", err)
		}
		return
	}

	// Build swarm state
	state := &swarm.SwarmState{
		Initialized:  true,
		ManagerHost:  client.Host(),
		WorkerToken:  strings.TrimSpace(workerToken),
		ManagerToken: strings.TrimSpace(managerToken),
		Nodes:        make(map[string]string),
		LastUpdated:  time.Now().Format(time.RFC3339),
	}

	// Write encrypted swarm state file
	stateFile := filepath.Join(".tako", fmt.Sprintf("swarm_%s_%s.json", cfg.Project.Name, envName))
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		if verbose {
			fmt.Printf("Warning: failed to create state directory: %v\n", err)
		}
		return
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to marshal swarm state: %v\n", err)
		}
		return
	}

	encryptor, err := crypto.NewEncryptorFromKeyFile(crypto.GetProjectKeyPath("."))
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize encryption: %v\n", err)
		}
		return
	}

	encrypted, err := encryptor.Encrypt(data)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to encrypt swarm state: %v\n", err)
		}
		return
	}

	if err := os.WriteFile(stateFile, encrypted, 0600); err != nil {
		if verbose {
			fmt.Printf("Warning: failed to write swarm state: %v\n", err)
		}
		return
	}

	fmt.Println("Swarm tokens recovered and saved")
}

// reconcileAndSave uses ReconcileStateFromRunning to rebuild state from running services
func reconcileAndSave(client *ssh.Client, cfg *config.Config, envName string) error {
	deployment, err := ReconcileStateFromRunning(client, cfg.Project.Name, envName)
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

	// Also recover swarm tokens while we're at it
	recoverSwarmTokens(client, cfg, envName)

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
