package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/updater"
	"github.com/spf13/cobra"
)

var (
	upgradeForce bool
	upgradeCheck bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade Tako CLI to the latest version",
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

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolVarP(&upgradeCheck, "check", "c", false, "Only check for updates, don't upgrade")
	upgradeCmd.Flags().BoolVarP(&upgradeForce, "force", "f", false, "Force upgrade even if already on latest version")
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
