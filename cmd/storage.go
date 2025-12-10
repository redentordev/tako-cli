package cmd

import (
	"fmt"
	"strings"

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

	// Check client status on all servers
	fmt.Println("\nClient Status:")
	for serverName, serverConfig := range cfg.Servers {
		client, err := sshPool.GetOrCreateWithAuth(
			serverConfig.Host,
			serverConfig.Port,
			serverConfig.User,
			serverConfig.SSHKey,
			serverConfig.Password,
		)
		if err != nil {
			fmt.Printf("  %s: ✗ Connection failed\n", serverName)
			continue
		}

		status, err := nfsProvisioner.GetNFSStatus(client, false)
		if err != nil {
			fmt.Printf("  %s: ✗ Status check failed\n", serverName)
			continue
		}

		if serverName == nfsServerName {
			// For server, show as "local"
			if len(status.Mounts) > 0 {
				fmt.Printf("  %s: ✓ local (NFS server)\n", serverName)
			} else {
				fmt.Printf("  %s: ○ local, no mounts (NFS server)\n", serverName)
			}
		} else {
			// For clients, show mount status
			if len(status.Mounts) > 0 {
				fmt.Printf("  %s: ✓ mounted (%d exports)\n", serverName, len(status.Mounts))
			} else {
				fmt.Printf("  %s: ○ not mounted\n", serverName)
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

	// Remount on all servers
	errors := 0
	for serverName, serverConfig := range cfg.Servers {
		fmt.Printf("→ Remounting on %s...\n", serverName)

		client, err := sshPool.GetOrCreateWithAuth(
			serverConfig.Host,
			serverConfig.Port,
			serverConfig.User,
			serverConfig.SSHKey,
			serverConfig.Password,
		)
		if err != nil {
			fmt.Printf("  ✗ Connection failed: %v\n", err)
			errors++
			continue
		}

		// Determine NFS server host for this client
		nfsHost := nfsServerConfig.Host
		if serverName == nfsServerName {
			nfsHost = "localhost"
		}

		// Remount using SetupNFSClient (handles unmount + mount)
		if err := nfsProvisioner.SetupNFSClient(client, nfsHost, exports); err != nil {
			fmt.Printf("  ✗ Remount failed: %v\n", err)
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
