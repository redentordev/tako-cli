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
	Short: "Provision server with Docker, Traefik, and security hardening",
	Long: `Setup provisions your VPS server with all necessary components:
  - Docker and Docker Compose
  - Traefik reverse proxy for automatic SSL and load balancing
  - UFW firewall configuration
  - Security hardening (disable root login, fail2ban)
  - Deploy user creation and SSH key setup
  - Monitoring agent for system metrics

This only needs to be run once per server.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().StringVarP(&setupServer, "server", "s", "", "Server to provision (default: all servers)")
}

func runSetup(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Determine which servers to provision
	servers := cfg.Servers
	if setupServer != "" {
		server, exists := cfg.Servers[setupServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", setupServer)
		}
		servers = map[string]config.ServerConfig{setupServer: server}
	}

	// Provision each server
	for name, server := range servers {
		fmt.Printf("\n=== Provisioning server: %s (%s) ===\n\n", name, server.Host)

		// Get or create SSH client
		client, err := sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
		if err != nil {
			return fmt.Errorf("failed to connect to server %s: %w", name, err)
		}

		// Check if server is already set up and needs upgrade
		serverVersion, err := setup.DetectServerVersion(client)
		if err == nil && serverVersion != nil {
			// Server already set up
			if serverVersion.IsUpgradeAvailable() {
				fmt.Printf("→ Server is at v%s, upgrading to v%s...\n", serverVersion.Version, setup.CurrentVersion)

				// Create upgrader with console logger
				upgrader := setup.NewUpgrader(client, &consoleLogger{})

				// Plan upgrade
				path, err := setup.PlanUpgrade(serverVersion.Version, setup.CurrentVersion)
				if err != nil {
					return fmt.Errorf("failed to plan upgrade: %w", err)
				}

				// Execute upgrade
				if err := upgrader.Execute(path); err != nil {
					return fmt.Errorf("upgrade failed: %w", err)
				}

				fmt.Printf("  ✓ Server upgraded to v%s\n", setup.CurrentVersion)
				continue // Skip fresh setup
			} else {
				fmt.Printf("→ Server is already at the latest version (v%s), skipping setup\n", serverVersion.Version)
				continue
			}
		}

		// Server not set up - run full provisioning
		fmt.Printf("→ Setting up server from scratch...\n")

		// Create provisioner
		prov := provisioner.NewProvisioner(client, verbose)

		// Run provisioning steps
		steps := []struct {
			name string
			fn   func() error
		}{
			{"Checking system requirements", prov.CheckRequirements},
			{"Updating system packages", prov.UpdateSystem},
			{"Installing Docker", prov.InstallDocker},
			{"Configuring firewall (UFW)", prov.ConfigureFirewall},
			{"Hardening security", prov.HardenSecurity},
			{"Verifying auto-recovery", prov.VerifyAutoRecovery},
			{"Setting up deploy user", func() error { return prov.SetupDeployUser(server.User) }},
			{"Installing monitoring agent", prov.InstallMonitoringAgent},
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
			TakoCLIVersion: "0.3.0", // TODO: Get from build
			Components:     make(map[string]string),
			Features:       []string{"docker", "traefik-proxy", "firewall", "monitoring"},
		}

		if err := setup.WriteVersionFile(client, newVersion); err != nil {
			fmt.Printf("  ⚠ Warning: Failed to write version file: %v\n", err)
		}

		fmt.Printf("\n✓ Server %s provisioned successfully!\n", name)
	}

	fmt.Printf("\nAll servers provisioned successfully!\n")
	fmt.Printf("\nNext step: Run 'tako deploy' to deploy your application\n")

	return nil
}

// consoleLogger implements the setup.Logger interface for console output
type consoleLogger struct{}

func (l *consoleLogger) Log(format string, args ...interface{}) {
	fmt.Printf("  "+format+"\n", args...)
}
