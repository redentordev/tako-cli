package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/infra"
	"github.com/spf13/cobra"
)

var (
	provisionPreview bool
	provisionYes     bool
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Provision cloud infrastructure defined in tako.yaml",
	Long: `Provision creates cloud servers and networking as defined in the
infrastructure section of tako.yaml.

This command uses Pulumi to manage infrastructure lifecycle with state
stored locally in .tako/pulumi/

Supported providers: digitalocean, hetzner (aws, linode coming soon)

Examples:
  tako provision                    # Provision all infrastructure
  tako provision --preview          # Preview changes without applying
  tako provision -y                 # Skip confirmation prompt`,
	RunE: runProvision,
}

func init() {
	rootCmd.AddCommand(provisionCmd)

	provisionCmd.Flags().BoolVar(&provisionPreview, "preview", false, "Preview changes without applying")
	provisionCmd.Flags().BoolVarP(&provisionYes, "yes", "y", false, "Skip confirmation prompt")
}

func runProvision(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if infrastructure is defined
	if cfg.Infrastructure == nil {
		fmt.Println("No infrastructure section found in tako.yaml")
		fmt.Println("\nTo use infrastructure provisioning, add an infrastructure section:")
		fmt.Print(`
infrastructure:
  provider: digitalocean  # or hetzner
  region: nyc1
  credentials:
    token: ${DO_TOKEN}
  servers:
    web:
      size: s-2vcpu-4gb
      image: ubuntu-22-04-x64
      role: manager
`)
		return nil
	}

	// Ensure Pulumi is installed
	if err := infra.EnsurePulumi(verbose); err != nil {
		return fmt.Errorf("failed to ensure Pulumi is installed: %w", err)
	}

	// Get tako directory
	takoDir := filepath.Join(".", ".tako")

	// Create orchestrator
	orchestrator, err := infra.NewOrchestrator(cfg, takoDir, envFlag, verbose)
	if err != nil {
		return fmt.Errorf("failed to initialize infrastructure: %w", err)
	}

	ctx := context.Background()

	// Preview mode
	if provisionPreview {
		fmt.Printf("Previewing infrastructure changes for %s...\n\n", cfg.Project.Name)
		fmt.Printf("Provider: %s\n", cfg.Infrastructure.Provider)
		fmt.Printf("Region:   %s\n\n", cfg.Infrastructure.Region)

		result, err := orchestrator.Preview(ctx)
		if err != nil {
			return fmt.Errorf("preview failed: %w", err)
		}

		if result.Summary != "" {
			fmt.Println(result.Summary)
		}
		fmt.Printf("\nPreview completed in %s\n", result.Duration.Round(time.Millisecond))
		return nil
	}

	// Show what will be created
	fmt.Printf("Provisioning infrastructure for %s\n\n", cfg.Project.Name)
	fmt.Printf("Provider: %s\n", cfg.Infrastructure.Provider)
	fmt.Printf("Region:   %s\n\n", cfg.Infrastructure.Region)

	fmt.Println("Servers to provision:")
	for name, spec := range cfg.Infrastructure.Servers {
		count := spec.Count
		if count < 1 {
			count = 1
		}
		role := spec.Role
		if role == "" {
			role = "worker"
		}
		fmt.Printf("  - %s: %s x%d (%s)\n", name, spec.Size, count, role)
	}

	if cfg.Infrastructure.Networking != nil {
		if cfg.Infrastructure.Networking.VPC != nil && cfg.Infrastructure.Networking.VPC.Enabled {
			fmt.Println("\nNetworking:")
			fmt.Printf("  - VPC: %s\n", cfg.Infrastructure.Networking.VPC.IPRange)
		}
		if cfg.Infrastructure.Networking.Firewall != nil && cfg.Infrastructure.Networking.Firewall.Enabled {
			fmt.Printf("  - Firewall: %d rules\n", len(cfg.Infrastructure.Networking.Firewall.Rules))
		}
	}

	// Confirm unless -y flag
	if !provisionYes {
		fmt.Print("\nProceed with provisioning? [y/N] ")
		var response string
		fmt.Scanln(&response)
		if response != "y" && response != "Y" {
			fmt.Println("Cancelled")
			return nil
		}
	}

	fmt.Println("\nProvisioning infrastructure...")

	// Run provision
	result, err := orchestrator.Provision(ctx)
	if err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("provisioning failed: %s", result.Error)
	}

	// Show results
	fmt.Println("\n=== Provisioned Servers ===")
	for name, server := range result.Servers {
		fmt.Printf("\n%s:\n", name)
		fmt.Printf("  ID:         %s\n", server.ID)
		fmt.Printf("  Public IP:  %s\n", server.PublicIP)
		if server.PrivateIP != "" {
			fmt.Printf("  Private IP: %s\n", server.PrivateIP)
		}
		fmt.Printf("  Role:       %s\n", server.Role)
	}

	if result.Network != nil && result.Network.VPCID != "" {
		fmt.Println("\n=== Network ===")
		fmt.Printf("  VPC ID: %s\n", result.Network.VPCID)
		if result.Network.FirewallID != "" {
			fmt.Printf("  Firewall ID: %s\n", result.Network.FirewallID)
		}
	}

	fmt.Printf("\nProvisioning completed in %s\n", result.Duration.Round(time.Millisecond))
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Run 'tako setup' to install Docker and configure servers")
	fmt.Println("  2. Run 'tako deploy' to deploy your application")
	fmt.Println("\nOr run 'tako deploy --full' for the complete lifecycle")

	return nil
}

// CheckInfrastructureProvisioned checks if infrastructure is provisioned
// Used by other commands that depend on infrastructure
func CheckInfrastructureProvisioned(cfg *config.Config) error {
	if cfg.Infrastructure == nil {
		return nil // No infrastructure defined, using static servers
	}

	takoDir := filepath.Join(".", ".tako")
	stateMgr := infra.NewStateManager(takoDir, cfg.Project.Name, envFlag)

	if !stateMgr.IsProvisioned() {
		return fmt.Errorf("infrastructure is defined but not provisioned. Run 'tako provision' first")
	}

	return nil
}

// GetInfrastructureOutputs retrieves provisioned infrastructure outputs
func GetInfrastructureOutputs(cfg *config.Config) (map[string]interface{}, error) {
	if cfg.Infrastructure == nil {
		return nil, nil
	}

	takoDir := filepath.Join(".", ".tako")
	stateMgr := infra.NewStateManager(takoDir, cfg.Project.Name, envFlag)

	return stateMgr.LoadOutputs()
}

// ResolveInfrastructureVariable resolves ${infrastructure.name.ip} variables
func ResolveInfrastructureVariable(cfg *config.Config, varPath string) (string, error) {
	outputs, err := GetInfrastructureOutputs(cfg)
	if err != nil {
		return "", err
	}
	if outputs == nil {
		return "", fmt.Errorf("no infrastructure outputs available")
	}

	// varPath is like "web.ip" or "workers-0.ip"
	// Convert to output key format: "web_ip" or "workers-0_ip"
	key := varPath
	// Replace last . with _ for the property
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '.' {
			key = key[:i] + "_" + key[i+1:]
			break
		}
	}

	if value, ok := outputs[key]; ok {
		return fmt.Sprintf("%v", value), nil
	}

	return "", fmt.Errorf("infrastructure output not found: %s", varPath)
}
