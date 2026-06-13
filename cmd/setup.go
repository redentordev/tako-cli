package cmd

import (
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	setupServer string
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Set up server with takod, proxy, and security hardening",
	Long: `Setup configures your VPS server with all necessary components:
  - Container runtime prerequisites
  - WireGuard mesh networking
  - takod node-local runtime
  - tako-proxy for automatic SSL and load balancing
  - UFW firewall configuration
  - Security hardening (disable root login, fail2ban)
  - Deploy user creation and SSH key setup
  - Monitoring agent for system metrics
  - NFS shared storage (if configured)

This only needs to be run once per server.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().StringVarP(&setupServer, "server", "s", "", "Server to set up (default: all servers)")
}

func runSetup(cmd *cobra.Command, args []string) error {
	// Load deployment configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Determine which servers to set up
	servers := cfg.Servers
	if setupServer != "" {
		server, exists := cfg.Servers[setupServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", setupServer)
		}
		servers = map[string]config.ServerConfig{setupServer: server}
	}

	// Set up each server
	for name, server := range servers {
		fmt.Printf("\n=== Setting up server: %s (%s) ===\n\n", name, server.Host)

		// Get or create SSH client (supports both key and password auth)
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to server %s: %w", name, err)
		}

		prov := provisioner.NewProvisioner(client, verbose)

		// Check if server is already set up and needs upgrade
		serverVersion, err := setup.DetectServerVersion(client)
		if err == nil && serverVersion != nil {
			if serverVersion.IsUpgradeAvailable() {
				fmt.Printf("→ Server is at v%s, reapplying current setup v%s...\n", serverVersion.Version, setup.CurrentVersion)
			} else if serverVersion.Version == setup.CurrentVersion {
				fmt.Printf("→ Server is already at the latest version (v%s), refreshing takod runtime\n", serverVersion.Version)
				if err := ensureTakodRuntimeForSetup(prov, cfg, name); err != nil {
					return fmt.Errorf("failed to refresh takod runtime on server %s: %w", name, err)
				}
				continue
			}
		} else {
			fmt.Printf("→ Setting up server from scratch...\n")
		}

		// Run provisioning steps
		steps := []struct {
			name string
			fn   func() error
		}{
			{"Checking system requirements", prov.CheckRequirements},
			{"Updating system packages", prov.UpdateSystem},
			{"Installing Docker", prov.InstallDocker},
			{"Installing WireGuard", prov.InstallWireGuard},
			{"Configuring firewall (UFW)", prov.ConfigureFirewall},
			{"Hardening security", prov.HardenSecurity},
			{"Verifying auto-recovery", prov.VerifyAutoRecovery},
			{"Setting up deploy user", func() error { return prov.SetupDeployUser(server.User) }},
			{"Installing monitoring agent", prov.InstallMonitoringAgent},
			{"Installing takod runtime", func() error { return ensureTakodRuntimeForSetup(prov, cfg, name) }},
		}

		for _, step := range steps {
			fmt.Printf("→ %s...\n", step.name)
			if err := step.fn(); err != nil {
				return fmt.Errorf("failed at step '%s' on server %s: %w", step.name, name, err)
			}
			fmt.Printf("  ✓ %s completed\n", step.name)
		}

		// Write version file after successful setup
		newVersion := &setup.ServerVersion{
			Version:        setup.CurrentVersion,
			InstalledAt:    time.Now(),
			TakoCLIVersion: Version,
			Components:     make(map[string]string),
			Features:       []string{"docker", "wireguard-mesh", "tako-proxy", "firewall", "monitoring"},
		}

		if err := setup.WriteVersionFile(client, newVersion); err != nil {
			fmt.Printf("  ⚠ Warning: Failed to write version file: %v\n", err)
		}

		fmt.Printf("\n✓ Server %s set up successfully!\n", name)
	}

	// Setup NFS if configured and appropriate
	if cfg.IsNFSEnabled() {
		serverCount := len(cfg.Servers)
		shouldSetup, reason := provisioner.ShouldSetupNFS(cfg, serverCount)

		if shouldSetup {
			fmt.Printf("\n=== Setting up NFS shared storage ===\n\n")
			if err := setupNFS(cfg, sshPool, envFlag); err != nil {
				return fmt.Errorf("failed to setup NFS: %w", err)
			}
		} else {
			fmt.Printf("\n=== NFS Shared Storage ===\n")
			fmt.Printf("→ Skipping NFS setup: %s\n", reason)
			if serverCount == 1 {
				fmt.Printf("  Tip: NFS volumes (nfs:name:/path) will use local bind mounts on single-server deployments.\n")
			}
		}
	}

	fmt.Printf("\nAll servers set up successfully!\n")
	fmt.Printf("\nNext step: Run 'tako deploy' to deploy your application\n")

	return nil
}

func ensureTakodRuntimeForSetup(prov *provisioner.Provisioner, cfg *config.Config, nodeName string) error {
	if err := prov.InstallTakodBinary(Version); err != nil {
		return err
	}
	socket := ""
	dataDir := ""
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil {
		socket = cfg.Runtime.Agent.Socket
		dataDir = cfg.Runtime.Agent.DataDir
	}
	return prov.InstallTakodService(socket, dataDir, nodeName)
}

// setupNFS configures NFS server and clients based on configuration
func setupNFS(cfg *config.Config, sshPool *ssh.Pool, envName string) error {
	nfsConfig := cfg.GetNFSConfig()
	if nfsConfig == nil || !nfsConfig.Enabled {
		return nil
	}

	// Use default environment if not specified
	if envName == "" {
		envName = cfg.GetDefaultEnvironment()
	}

	// Validate export paths before proceeding
	for _, export := range nfsConfig.Exports {
		if err := provisioner.ValidateNFSExportPath(export.Path); err != nil {
			return fmt.Errorf("invalid NFS export '%s': %w", export.Name, err)
		}
	}

	// Get NFS server name
	nfsServerName, err := cfg.GetNFSServerName(envName)
	if err != nil {
		return fmt.Errorf("failed to determine NFS server: %w", err)
	}

	nfsServerConfig, exists := cfg.Servers[nfsServerName]
	if !exists {
		return fmt.Errorf("NFS server '%s' not found in servers configuration", nfsServerName)
	}

	fmt.Printf("→ NFS server will be: %s (%s)\n", nfsServerName, nfsServerConfig.Host)

	// Get all server IPs for firewall configuration
	serverIPs := make([]string, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		// Validate IP addresses
		if err := provisioner.ValidateIPAddress(server.Host); err != nil {
			return fmt.Errorf("invalid server IP for firewall rules: %w", err)
		}
		serverIPs = append(serverIPs, server.Host)
	}

	// Create NFS provisioner
	nfsProvisioner := provisioner.NewNFSProvisioner(cfg.Project.Name, envName, verbose)

	// Connect to NFS server and set it up
	nfsClient, err := sshPool.GetOrCreateWithAuth(
		nfsServerConfig.Host,
		nfsServerConfig.Port,
		nfsServerConfig.User,
		nfsServerConfig.SSHKey,
		nfsServerConfig.Password,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to NFS server %s: %w", nfsServerName, err)
	}

	// Check for existing NFS setup and handle migration if needed
	existingStatus, _ := nfsProvisioner.DetectExistingNFS(nfsClient)
	if existingStatus != nil && existingStatus.ServiceActive {
		if verbose {
			fmt.Printf("  Detected existing NFS server, updating configuration...\n")
		}
	}

	fmt.Printf("→ Setting up NFS server on %s...\n", nfsServerName)
	serverInfo, err := nfsProvisioner.SetupNFSServer(nfsClient, nfsConfig, serverIPs)
	if err != nil {
		return fmt.Errorf("failed to setup NFS server: %w", err)
	}
	serverInfo.ServerName = nfsServerName
	serverInfo.Host = nfsServerConfig.Host

	fmt.Printf("  ✓ NFS server configured with %d exports\n", len(serverInfo.Exports))

	// Setup NFS clients on all other servers
	clientErrors := 0
	for serverName, serverConfig := range cfg.Servers {
		if serverName == nfsServerName {
			continue // Skip the NFS server itself
		}

		fmt.Printf("→ Setting up NFS client on %s...\n", serverName)

		client, err := sshPool.GetOrCreateWithAuth(
			serverConfig.Host,
			serverConfig.Port,
			serverConfig.User,
			serverConfig.SSHKey,
			serverConfig.Password,
		)
		if err != nil {
			fmt.Printf("  ⚠ Warning: failed to connect to server %s: %v\n", serverName, err)
			clientErrors++
			continue
		}

		if err := nfsProvisioner.SetupNFSClient(client, nfsServerConfig.Host, serverInfo.Exports); err != nil {
			fmt.Printf("  ⚠ Warning: failed to setup NFS client on %s: %v\n", serverName, err)
			clientErrors++
			continue
		}

		fmt.Printf("  ✓ NFS client configured on %s\n", serverName)
	}

	// Also setup local mount on the NFS server itself (for local access)
	fmt.Printf("→ Setting up local NFS access on %s...\n", nfsServerName)
	if err := nfsProvisioner.SetupNFSClient(nfsClient, "localhost", serverInfo.Exports); err != nil {
		// Non-fatal: local mount may fail but remote access still works
		fmt.Printf("  ⚠ Warning: local NFS mount on server failed: %v\n", err)
	} else {
		fmt.Printf("  ✓ Local NFS access configured\n")
	}

	fmt.Printf("\n✓ NFS shared storage setup completed!\n")
	fmt.Printf("  Server: %s (%s)\n", nfsServerName, nfsServerConfig.Host)
	fmt.Printf("  Exports:\n")
	for _, export := range serverInfo.Exports {
		fmt.Printf("    - %s: %s\n", export.Name, export.Path)
		fmt.Printf("      Mount point: %s\n", export.MountPoint)
	}

	if clientErrors > 0 {
		fmt.Printf("\n⚠ Warning: %d client(s) failed to setup. Run 'tako storage remount' to retry.\n", clientErrors)
	}

	return nil
}

// consoleLogger implements the setup.Logger interface for console output
type consoleLogger struct{}

func (l *consoleLogger) Log(format string, args ...interface{}) {
	fmt.Printf("  "+format+"\n", args...)
}
