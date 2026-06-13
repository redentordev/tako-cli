package cmd

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Manage shared storage (NFS)",
	Long: `Manage shared storage configuration and status.

Subcommands:
  status   - Show NFS storage status across all servers
  remount  - Remount NFS exports on all clients

Examples:
  tako storage status               # Show storage status
  tako storage status -e production # Show status for specific environment
  tako storage remount              # Remount NFS on all clients`,
}

var storageStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show NFS storage status",
	Long:  `Display the status of NFS shared storage across all servers.`,
	RunE:  runStorageStatus,
}

var storageRemountCmd = &cobra.Command{
	Use:   "remount",
	Short: "Remount NFS exports",
	Long:  `Remount NFS exports on all client servers. Useful if mounts become stale or disconnected.`,
	RunE:  runStorageRemount,
}

func init() {
	rootCmd.AddCommand(storageCmd)
	storageCmd.AddCommand(storageStatusCmd)
	storageCmd.AddCommand(storageRemountCmd)
}

func runStorageStatus(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if NFS is enabled
	if !cfg.IsNFSEnabled() {
		fmt.Println("NFS storage is not enabled in this project.")
		fmt.Println("\nTo enable NFS, add the following to your tako.yaml:")
		fmt.Println(`
storage:
  nfs:
    enabled: true
    server: auto  # or specify a server name
    exports:
      - name: shared_data
        path: /srv/nfs/data`)
		return nil
	}

	nfsConfig := cfg.GetNFSConfig()

	// Use specified environment or default
	envName := envFlag
	if envName == "" {
		envName = cfg.GetDefaultEnvironment()
	}

	// Get NFS server info
	nfsServerName, err := cfg.GetNFSServerName(envName)
	if err != nil {
		return fmt.Errorf("failed to determine NFS server: %w", err)
	}

	nfsServerConfig := cfg.Servers[nfsServerName]
	targetServers, err := storageTargetServers(cfg, envName, nfsServerName)
	if err != nil {
		return err
	}

	fmt.Println("NFS Storage Status")
	fmt.Println(strings.Repeat("─", 50))
	fmt.Printf("Server:      %s (%s)\n", nfsServerName, nfsServerConfig.Host)
	fmt.Printf("Environment: %s\n", envName)
	fmt.Println()

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Create NFS provisioner
	nfsProvisioner := provisioner.NewNFSProvisioner(cfg.Project.Name, envName, verbose)

	// Check NFS server status
	fmt.Println("Exports:")
	for _, export := range nfsConfig.Exports {
		mountPoint := nfsProvisioner.GetNFSMountPoint(export.Name)
		fmt.Printf("  %s\n", export.Name)
		fmt.Printf("    Path:        %s\n", export.Path)
		fmt.Printf("    Mount point: %s\n", mountPoint)
		if export.Size != "" {
			fmt.Printf("    Size hint:   %s\n", export.Size)
		}
	}
	fmt.Println()

	// Connect to NFS server and get status
	fmt.Println("Server Status:")
	nfsClient, err := sshPool.GetOrCreateWithAuth(
		nfsServerConfig.Host,
		nfsServerConfig.Port,
		nfsServerConfig.User,
		nfsServerConfig.SSHKey,
		nfsServerConfig.Password,
	)
	if err != nil {
		fmt.Printf("  %s: ✗ Connection failed: %v\n", nfsServerName, err)
	} else {
		status, err := nfsProvisioner.GetNFSStatus(nfsClient, true)
		if err != nil {
			fmt.Printf("  %s: ✗ Status check failed: %v\n", nfsServerName, err)
		} else {
			if status.ServiceActive {
				fmt.Printf("  %s: ● Active (NFS server)\n", nfsServerName)
			} else {
				fmt.Printf("  %s: ○ Inactive (NFS server not running)\n", nfsServerName)
			}
			if status.ClientCount > 0 {
				fmt.Printf("    Connected clients: %d\n", status.ClientCount)
			}
		}
	}

	// Check client status on selected environment nodes.
	fmt.Println("\nClient Status:")
	for _, result := range collectStorageStatusNodes(cfg.Servers, targetServers, nfsServerName, func(_ string, server config.ServerConfig, isServer bool) (*provisioner.NFSStatus, error) {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("connection failed: %w", err)
		}
		return nfsProvisioner.GetNFSStatus(client, isServer)
	}) {
		if result.err != nil {
			fmt.Printf("  %s: ✗ %v\n", result.serverName, result.err)
			continue
		}
		if result.serverName == nfsServerName {
			// For server, show as "local"
			if len(result.status.Mounts) > 0 {
				fmt.Printf("  %s: ✓ local (NFS server)\n", result.serverName)
			} else {
				fmt.Printf("  %s: ○ local, no mounts (NFS server)\n", result.serverName)
			}
		} else {
			// For clients, show mount status
			if len(result.status.Mounts) > 0 {
				fmt.Printf("  %s: ✓ mounted (%d exports)\n", result.serverName, len(result.status.Mounts))
			} else {
				fmt.Printf("  %s: ○ not mounted\n", result.serverName)
			}
		}
	}

	return nil
}

func runStorageRemount(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if NFS is enabled
	if !cfg.IsNFSEnabled() {
		return fmt.Errorf("NFS storage is not enabled in this project")
	}

	nfsConfig := cfg.GetNFSConfig()

	// Use specified environment or default
	envName := envFlag
	if envName == "" {
		envName = cfg.GetDefaultEnvironment()
	}

	// Get NFS server info
	nfsServerName, err := cfg.GetNFSServerName(envName)
	if err != nil {
		return fmt.Errorf("failed to determine NFS server: %w", err)
	}

	nfsServerConfig := cfg.Servers[nfsServerName]
	targetServers, err := storageTargetServers(cfg, envName, nfsServerName)
	if err != nil {
		return err
	}

	fmt.Printf("Remounting NFS exports...\n\n")

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Create NFS provisioner
	nfsProvisioner := provisioner.NewNFSProvisioner(cfg.Project.Name, envName, verbose)

	// Build export info
	exports := make([]provisioner.NFSExportInfo, 0, len(nfsConfig.Exports))
	for _, export := range nfsConfig.Exports {
		exports = append(exports, provisioner.NFSExportInfo{
			Name:       export.Name,
			Path:       export.Path,
			MountPoint: nfsProvisioner.GetNFSMountPoint(export.Name),
		})
	}

	results := collectStorageRemountNodes(cfg.Servers, targetServers, nfsServerName, nfsServerConfig.Host, func(_ string, server config.ServerConfig, nfsHost string) error {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("connection failed: %w", err)
		}
		return nfsProvisioner.SetupNFSClient(client, nfsHost, exports)
	})

	errors := 0
	for _, result := range results {
		fmt.Printf("→ Remounting on %s...\n", result.serverName)
		if result.err != nil {
			fmt.Printf("  ✗ Remount failed: %v\n", result.err)
			errors++
			continue
		}
		fmt.Printf("  ✓ Remounted successfully\n")
	}

	fmt.Println()
	if errors > 0 {
		return fmt.Errorf("remount completed with %d errors", errors)
	}

	fmt.Println("✓ All NFS exports remounted successfully")
	return nil
}

type storageStatusReadFunc func(serverName string, server config.ServerConfig, isServer bool) (*provisioner.NFSStatus, error)

type storageStatusResult struct {
	index      int
	serverName string
	status     *provisioner.NFSStatus
	err        error
}

func collectStorageStatusNodes(servers map[string]config.ServerConfig, serverNames []string, nfsServerName string, read storageStatusReadFunc) []storageStatusResult {
	resultCh := make(chan storageStatusResult, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			resultCh <- storageStatusResult{index: index, serverName: serverName, err: fmt.Errorf("server not found in configuration")}
			continue
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			status, err := read(serverName, server, serverName == nfsServerName)
			resultCh <- storageStatusResult{index: index, serverName: serverName, status: status, err: err}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)

	results := make([]storageStatusResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

type storageRemountFunc func(serverName string, server config.ServerConfig, nfsHost string) error

type storageRemountResult struct {
	index      int
	serverName string
	err        error
}

func collectStorageRemountNodes(servers map[string]config.ServerConfig, serverNames []string, nfsServerName string, nfsServerHost string, remount storageRemountFunc) []storageRemountResult {
	resultCh := make(chan storageRemountResult, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			resultCh <- storageRemountResult{index: index, serverName: serverName, err: fmt.Errorf("server not found in configuration")}
			continue
		}
		nfsHost := nfsServerHost
		if serverName == nfsServerName {
			nfsHost = "localhost"
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig, nfsHost string) {
			defer wg.Done()
			resultCh <- storageRemountResult{index: index, serverName: serverName, err: remount(serverName, server, nfsHost)}
		}(index, serverName, server, nfsHost)
	}
	wg.Wait()
	close(resultCh)

	results := make([]storageRemountResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func storageTargetServers(cfg *config.Config, envName string, nfsServerName string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	seen := make(map[string]bool, len(envServers)+1)
	targets := make([]string, 0, len(envServers)+1)
	for _, serverName := range envServers {
		if !seen[serverName] {
			targets = append(targets, serverName)
			seen[serverName] = true
		}
	}
	if nfsServerName != "" && !seen[nfsServerName] {
		targets = append(targets, nfsServerName)
	}
	sort.Strings(targets)
	return targets, nil
}
