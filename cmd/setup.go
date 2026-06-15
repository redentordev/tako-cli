package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	setupServer        string
	setupTakodBinary   string
	setupDedicatedEdge bool
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

This only needs to be run once per server.

Use --dedicated-edge only on a node that should let a project-owned edge
service bind public ports 80 and 443 directly. The command refuses to disable
shared tako-proxy while active proxy route files still exist.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().StringVarP(&setupServer, "server", "s", "", "Server to set up (default: all servers)")
	setupCmd.Flags().StringVar(&setupTakodBinary, "takod-binary", "", "Path to a Linux tako binary to install as takod during setup (for development/testing)")
	setupCmd.Flags().BoolVar(&setupDedicatedEdge, "dedicated-edge", false, "Disable shared tako-proxy after setup when no active proxy routes exist, for direct 80/443 edge services")
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
				if setupDedicatedEdge {
					if err := disableTakodProxyForSetup(client, cfg, name); err != nil {
						return fmt.Errorf("failed to prepare dedicated edge on server %s: %w", name, err)
					}
				} else if err := enableTakodProxyForSetup(client, cfg, envName, name); err != nil {
					return fmt.Errorf("failed to prepare shared proxy on server %s: %w", name, err)
				}
				if err := writeSetupVersionMetadata(client, setupFeatures(setupDedicatedEdge)); err != nil {
					return setupVersionWriteError(name, err)
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

		if setupDedicatedEdge {
			fmt.Printf("→ Preparing dedicated edge mode...\n")
			if err := disableTakodProxyForSetup(client, cfg, name); err != nil {
				return fmt.Errorf("failed to prepare dedicated edge on server %s: %w", name, err)
			}
			fmt.Printf("  ✓ Dedicated edge mode prepared\n")
		} else {
			fmt.Printf("→ Preparing shared proxy mode...\n")
			if err := enableTakodProxyForSetup(client, cfg, envName, name); err != nil {
				return fmt.Errorf("failed to prepare shared proxy on server %s: %w", name, err)
			}
			fmt.Printf("  ✓ Shared proxy mode prepared\n")
		}

		// Write version file after successful setup
		if err := writeSetupVersionMetadata(client, setupFeatures(setupDedicatedEdge)); err != nil {
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

func setupFeatures(dedicatedEdge bool) []string {
	features := []string{"docker", "wireguard-mesh", "firewall", "monitoring"}
	if dedicatedEdge {
		features = append(features, "dedicated-edge")
	} else {
		features = append(features, "tako-proxy")
	}
	return features
}

func writeSetupVersionMetadata(client *ssh.Client, features []string) error {
	newVersion := &setup.ServerVersion{
		Version:        setup.CurrentVersion,
		InstalledAt:    time.Now(),
		TakoCLIVersion: Version,
		Components:     make(map[string]string),
		Features:       features,
	}
	return setup.WriteVersionFile(client, newVersion)
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
	EnsureDockerBuildx() error
}

func refreshCurrentSetup(prov currentSetupRefresher, cfg *config.Config, nodeName string, username string, meshListenPort int) error {
	if err := prov.ConfigureFirewall(meshListenPort); err != nil {
		return fmt.Errorf("refresh firewall: %w", err)
	}
	if err := prov.EnsureDockerBuildx(); err != nil {
		return fmt.Errorf("refresh docker buildx: %w", err)
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

func disableTakodProxyForSetup(client takodclient.RequestExecutor, cfg *config.Config, serverName string) error {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "DELETE", "/v1/proxy", nil)
		if err == nil {
			var response takod.DisableProxyResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				return fmt.Errorf("failed to parse proxy disable response: %w", err)
			}
			if response.Removed {
				fmt.Printf("  ✓ Disabled shared tako-proxy on %s\n", serverName)
			} else {
				fmt.Printf("  ✓ Shared tako-proxy already absent on %s\n", serverName)
			}
			return nil
		}
		lastErr = err
		if !isTakodTemporarilyUnavailable(err) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("takod did not become ready for proxy disable: %w", lastErr)
}

func enableTakodProxyForSetup(client takodclient.RequestExecutor, cfg *config.Config, envName string, serverName string) error {
	network := runtimeid.NetworkName(cfg.Project.Name, envName)
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "POST", "/v1/proxy", takod.ReconcileProxyRequest{
			Network: network,
		})
		if err == nil {
			var response takod.ReconcileProxyResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				return fmt.Errorf("failed to parse proxy reconcile response: %w", err)
			}
			if response.Container != "tako-proxy" {
				return fmt.Errorf("unexpected proxy container %q", response.Container)
			}
			fmt.Printf("  ✓ Shared tako-proxy running on %s\n", serverName)
			return nil
		}
		lastErr = err
		if !isTakodTemporarilyUnavailable(err) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("takod did not become ready for proxy reconcile: %w", lastErr)
}

func isTakodTemporarilyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "takod socket or curl is unavailable")
}

// consoleLogger implements the setup.Logger interface for console output
type consoleLogger struct{}

func (l *consoleLogger) Log(format string, args ...interface{}) {
	fmt.Printf("  "+format+"\n", args...)
}
