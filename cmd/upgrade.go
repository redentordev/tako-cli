package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/updater"
	"github.com/spf13/cobra"
)

var (
	upgradeForce              bool
	upgradeCheck              bool
	upgradeServersServer      string
	upgradeServersDryRun      bool
	upgradeServersTakodBinary string
	upgradeServersStatusWait  = 30 * time.Second
	upgradeServersStatusPoll  = 1 * time.Second
	upgradeServersStatusProbe = 3 * time.Second
)

var upgradeCmd = &cobra.Command{
	Use:          "upgrade",
	Short:        "Upgrade Tako CLI to the latest version",
	SilenceUsage: true,
	Long: `Upgrade Tako CLI to the latest version from GitHub releases.

This command will:
  - Check for the latest version
  - Download the appropriate binary for your platform
  - Replace the current binary with the new version
  - Create a backup of the current version (just in case)

The upgrade is performed in-place and requires the same permissions
as the original installation.`,
	Example: `  # Check for updates without upgrading
  tako upgrade --check

  # Upgrade to the latest version
  tako upgrade

  # Force upgrade even if already on latest
  tako upgrade --force`,
	RunE: runUpgrade,
}

var upgradeServersCmd = &cobra.Command{
	Use:          "servers",
	Short:        "Upgrade server-side takod agents to this CLI version",
	SilenceUsage: true,
	Long: `Upgrade server-side takod agents for the selected environment.

This command patches stale or missing takod agents without changing application
services. It installs the matching Tako release binary, restarts the takod
systemd service, refreshes /etc/tako/version.json, and verifies /v1/status.

Use --dry-run first to see current agent versions. Development builds must pass
--takod-binary with a Linux tako binary because there is no release asset for
version "dev".`,
	Example: `  # Show current server agent versions
  tako upgrade servers --dry-run

  # Patch every server in the active environment
  tako upgrade servers

  # Patch one server
  tako upgrade servers --server node-a

  # Development/testing with a locally built Linux binary
  tako upgrade servers --takod-binary ./dist/tako-linux-amd64`,
	RunE: runUpgradeServers,
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolVarP(&upgradeCheck, "check", "c", false, "Only check for updates, don't upgrade")
	upgradeCmd.Flags().BoolVarP(&upgradeForce, "force", "f", false, "Force upgrade even if already on latest version")
	upgradeCmd.AddCommand(upgradeServersCmd)
	upgradeServersCmd.Flags().StringVarP(&upgradeServersServer, "server", "s", "", "Server to upgrade (default: all servers in environment)")
	upgradeServersCmd.Flags().BoolVar(&upgradeServersDryRun, "dry-run", false, "Report server agent versions without changing remote state")
	upgradeServersCmd.Flags().StringVar(&upgradeServersTakodBinary, "takod-binary", "", "Path to a Linux tako binary to install as takod (required for development builds)")
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	fmt.Println("🐙 Tako CLI Upgrade Tool")
	fmt.Println()

	// Check for updates
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking for updates...")

	available, latestVersion, err := updater.IsUpdateAvailable(Version)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}

	// If only checking
	if upgradeCheck {
		if available {
			fmt.Printf("\n✨ New version available: %s\n", latestVersion)
			fmt.Printf("   Run 'tako upgrade' to update\n")
		} else {
			fmt.Printf("\n✓ You are already on the latest version (%s)\n", Version)
		}
		return nil
	}

	// Check if update is available
	if !available && !upgradeForce {
		fmt.Printf("\n✓ You are already on the latest version (%s)\n", Version)
		fmt.Println("\nUse --force to reinstall the current version")
		return nil
	}

	if available {
		fmt.Printf("\n📦 New version available: %s → %s\n\n", Version, latestVersion)
	} else {
		fmt.Printf("\n🔄 Reinstalling version: %s\n\n", latestVersion)
	}

	// Perform the upgrade
	if err := updater.DownloadUpdate(latestVersion); err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	fmt.Println()
	fmt.Println("🎉 Upgrade completed successfully!")
	fmt.Println()
	fmt.Println("Verify with: tako --version")

	return nil
}

func runUpgradeServers(cmd *cobra.Command, args []string) error {
	if err := validateUpgradeServersOptions(Version, upgradeServersTakodBinary, upgradeServersDryRun); err != nil {
		return err
	}

	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	targetServerNames, servers, err := setupTargetServers(cfg, envName, upgradeServersServer)
	if err != nil {
		return err
	}
	if len(targetServerNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	fmt.Println("🐙 Tako server agent upgrade")
	fmt.Printf("Environment: %s\n", envName)
	fmt.Printf("Target CLI version: %s\n", Version)
	if upgradeServersDryRun {
		fmt.Println("Mode: dry-run")
	}

	for _, name := range targetServerNames {
		server := servers[name]
		fmt.Printf("\n=== Server: %s (%s) ===\n", name, server.Host)
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to server %s: %w", name, err)
		}

		status, statusErr := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe)
		current := "unavailable"
		if statusErr == nil {
			current = status.Version
		}
		fmt.Printf("Current takod: %s\n", current)
		if statusErr != nil {
			fmt.Printf("Status check: %v\n", statusErr)
		}

		serverVersion, versionErr := setup.DetectServerVersion(client)
		if upgradeServersDryRun {
			if versionErr != nil {
				fmt.Printf("Setup manifest: unavailable (%v)\n", versionErr)
				fmt.Printf("Action: setup required (%s -> %s)\n", current, Version)
				continue
			}
			action := "current"
			if statusErr != nil {
				action = "status unavailable"
			} else if current != Version || serverVersion.TakoCLIVersion != Version {
				action = "upgrade needed"
			}
			fmt.Printf("Setup manifest: v%s (CLI %s)\n", serverVersion.Version, serverVersion.TakoCLIVersion)
			fmt.Printf("Action: %s (%s -> %s)\n", action, current, Version)
			continue
		}
		if versionErr != nil {
			return fmt.Errorf("server %s is not set up; run 'tako setup --server %s' first: %w", name, name, versionErr)
		}

		prov := provisioner.NewProvisioner(client, verbose)
		if err := ensureTakodRuntimeWithBinary(prov, cfg, name, upgradeServersTakodBinary); err != nil {
			return fmt.Errorf("failed to upgrade takod on server %s: %w", name, err)
		}
		if err := setup.WriteVersionFile(client, setupVersionManifest(serverVersion)); err != nil {
			return setupVersionWriteError(name, err)
		}
		verified, err := waitForTakodAgentVersion(client, cfg, Version, upgradeServersStatusWait, upgradeServersStatusPoll, upgradeServersStatusProbe)
		if err != nil {
			return fmt.Errorf("server %s takod upgrade did not verify: %w", name, err)
		}
		fmt.Printf("✓ Upgraded takod: %s -> %s\n", current, verified.Version)
	}

	if upgradeServersDryRun {
		fmt.Println("\nDry-run complete. Apply with: tako upgrade servers")
		return nil
	}

	fmt.Println("\nAll selected server agents upgraded and verified.")
	fmt.Println("Next:")
	fmt.Printf("  tako state status -e %s\n", envName)
	fmt.Printf("  tako deploy -e %s --yes\n", envName)
	return nil
}

func validateUpgradeServersOptions(cliVersion string, takodBinary string, dryRun bool) error {
	if dryRun {
		return nil
	}
	version := strings.TrimSpace(cliVersion)
	if cliVersionRequiresTakodBinary(version) && strings.TrimSpace(takodBinary) == "" {
		return fmt.Errorf("server upgrades from a development or non-release CLI build require --takod-binary with a Linux tako binary")
	}
	return nil
}

func cliVersionRequiresTakodBinary(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" || version == "dev" || version == "unknown" {
		return true
	}
	return isGitDescribeSnapshot(version)
}

func isGitDescribeSnapshot(version string) bool {
	if strings.Contains(version, "-dirty") {
		return true
	}
	parts := strings.Split(version, "-")
	if len(parts) < 3 {
		return false
	}
	for i := 1; i < len(parts)-1; i++ {
		if !allASCIIBytes(parts[i], isASCIIDigit) {
			continue
		}
		next := parts[i+1]
		if len(next) > 1 && next[0] == 'g' && allASCIIBytes(next[1:], isASCIIHex) {
			return true
		}
	}
	return false
}

func allASCIIBytes(value string, valid func(byte) bool) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !valid(value[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isASCIIHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func probeTakodAgentStatus(client takodclient.RequestExecutor, cfg *config.Config, timeout time.Duration) (*takod.Status, error) {
	output, err := takodclient.RequestJSONWithTimeout(client, takodSocketFromConfig(cfg), "GET", "/v1/status", nil, timeout)
	if err != nil {
		return nil, err
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return nil, fmt.Errorf("failed to parse takod status: %w", err)
	}
	return &status, nil
}

func waitForTakodAgentVersion(client takodclient.RequestExecutor, cfg *config.Config, wantVersion string, wait time.Duration, poll time.Duration, probe time.Duration) (*takod.Status, error) {
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	var lastVersion string
	for {
		status, err := probeTakodAgentStatus(client, cfg, probe)
		if err == nil {
			lastVersion = status.Version
			if status.Version == wantVersion {
				return status, nil
			}
			lastErr = fmt.Errorf("reported version %s", status.Version)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poll)
	}
	if lastVersion != "" {
		return nil, fmt.Errorf("expected version %s, last %s", wantVersion, lastVersion)
	}
	return nil, fmt.Errorf("expected version %s, status unavailable: %w", wantVersion, lastErr)
}
