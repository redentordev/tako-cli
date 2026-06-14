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
	setupServer      string
	setupTakodBinary string
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

This only needs to be run once per server.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().StringVarP(&setupServer, "server", "s", "", "Server to set up (default: all servers)")
	setupCmd.Flags().StringVar(&setupTakodBinary, "takod-binary", "", "Path to a Linux tako binary to install as takod during setup (for development/testing)")
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

	envName := getEnvironmentName(cfg)
	targetServerNames, servers, err := setupTargetServers(cfg, envName, setupServer)
	if err != nil {
		return err
	}

	// Set up each server
	for _, name := range targetServerNames {
		server := servers[name]
		fmt.Printf("\n=== Setting up server: %s (%s) ===\n\n", name, server.Host)

		// Get or create SSH client (supports both key and password auth)
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to server %s: %w", name, err)
		}

		prov := provisioner.NewProvisioner(client, verbose)
		meshListenPort := setupMeshListenPort(cfg)

		// Check if server is already set up and needs upgrade
		serverVersion, err := setup.DetectServerVersion(client)
		if err == nil && serverVersion != nil {
			if serverVersion.IsUpgradeAvailable() {
				fmt.Printf("→ Server is at v%s, reapplying current setup v%s...\n", serverVersion.Version, setup.CurrentVersion)
			} else if serverVersion.Version == setup.CurrentVersion {
				fmt.Printf("→ Server is already at the latest version (v%s), refreshing firewall, deploy access, and takod runtime\n", serverVersion.Version)
				if err := refreshCurrentSetup(prov, cfg, name, server.User, meshListenPort); err != nil {
					return fmt.Errorf("failed to refresh current setup on server %s: %w", name, err)
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
			{"Configuring firewall (UFW)", func() error { return prov.ConfigureFirewall(meshListenPort) }},
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
			return setupVersionWriteError(name, err)
		}

		fmt.Printf("\n✓ Server %s set up successfully!\n", name)
	}

	fmt.Printf("\nAll servers set up successfully!\n")
	fmt.Printf("\nNext step: Run 'tako deploy' to deploy your application\n")

	return nil
}

func setupVersionWriteError(serverName string, err error) error {
	return fmt.Errorf("server %s setup completed but failed to write setup version metadata: %w", serverName, err)
}

func setupTargetServers(cfg *config.Config, envName string, requestedServer string) ([]string, map[string]config.ServerConfig, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, nil, err
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, nil, fmt.Errorf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return serverNames, servers, nil
}

func setupMeshListenPort(cfg *config.Config) int {
	if cfg.Mesh != nil && cfg.Mesh.ListenPort > 0 {
		return cfg.Mesh.ListenPort
	}
	return 51820
}

type setupRuntimeInstaller interface {
	InstallTakodBinaryFromFile(string) error
	InstallTakodBinary(string) error
	InstallTakodService(socket string, dataDir string, nodeName string) error
}

type currentSetupRefresher interface {
	setupRuntimeInstaller
	ConfigureFirewall(meshListenPort int) error
	SetupDeployUser(username string) error
}

func refreshCurrentSetup(prov currentSetupRefresher, cfg *config.Config, nodeName string, username string, meshListenPort int) error {
	if err := prov.ConfigureFirewall(meshListenPort); err != nil {
		return fmt.Errorf("refresh firewall: %w", err)
	}
	if err := prov.SetupDeployUser(username); err != nil {
		return fmt.Errorf("refresh deploy user access: %w", err)
	}
	if err := ensureTakodRuntimeForSetup(prov, cfg, nodeName); err != nil {
		return fmt.Errorf("refresh takod runtime: %w", err)
	}
	return nil
}

func ensureTakodRuntimeForSetup(prov setupRuntimeInstaller, cfg *config.Config, nodeName string) error {
	if setupTakodBinary != "" {
		if err := prov.InstallTakodBinaryFromFile(setupTakodBinary); err != nil {
			return err
		}
	} else {
		if err := prov.InstallTakodBinary(Version); err != nil {
			return err
		}
	}
	socket := ""
	dataDir := ""
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil {
		socket = cfg.Runtime.Agent.Socket
		dataDir = cfg.Runtime.Agent.DataDir
	}
	return prov.InstallTakodService(socket, dataDir, nodeName)
}

// consoleLogger implements the setup.Logger interface for console output
type consoleLogger struct{}

func (l *consoleLogger) Log(format string, args ...interface{}) {
	fmt.Printf("  "+format+"\n", args...)
}
