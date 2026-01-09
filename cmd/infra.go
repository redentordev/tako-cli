package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/infra"
	"github.com/spf13/cobra"
)

func osLookupEnv(key string) string {
	return os.Getenv(key)
}

var (
	infraYes bool
)

var infraCmd = &cobra.Command{
	Use:   "infra",
	Short: "Manage cloud infrastructure",
	Long: `Commands for managing cloud infrastructure provisioning.

Use 'tako infra' subcommands to preview, apply, or destroy infrastructure
defined in the infrastructure section of tako.yaml.`,
}

var infraPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview infrastructure changes",
	Long:  `Show what infrastructure changes would be made without applying them.`,
	RunE:  runInfraPreview,
}

var infraUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Apply infrastructure changes",
	Long:  `Create or update infrastructure defined in tako.yaml.`,
	RunE:  runInfraUp,
}

var infraDestroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Destroy infrastructure",
	Long:  `Destroy all infrastructure provisioned by Tako.`,
	RunE:  runInfraDestroy,
}

var infraOutputsCmd = &cobra.Command{
	Use:   "outputs",
	Short: "Show infrastructure outputs",
	Long:  `Display provisioned infrastructure outputs like server IPs.`,
	RunE:  runInfraOutputs,
}

var infraStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show infrastructure status",
	Long:  `Display the current state of provisioned infrastructure.`,
	RunE:  runInfraStatus,
}

var infraValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate infrastructure config",
	Long:  `Check infrastructure configuration for errors and compatibility issues.`,
	RunE:  runInfraValidate,
}

var infraSwitchCmd = &cobra.Command{
	Use:   "switch [provider]",
	Short: "Switch to a different provider",
	Long: `Convert infrastructure config to a different cloud provider.

This command helps migrate your infrastructure configuration between providers
while maintaining equivalent server sizes and finding appropriate regions.

Example:
  tako infra switch hetzner    # Convert current config to Hetzner`,
	Args: cobra.ExactArgs(1),
	RunE: runInfraSwitch,
}

var infraCompareCmd = &cobra.Command{
	Use:   "compare [provider1] [provider2]",
	Short: "Compare two providers",
	Long:  `Show feature and pricing comparison between two cloud providers.`,
	Args:  cobra.ExactArgs(2),
	RunE:  runInfraCompare,
}

var infraStateCmd = &cobra.Command{
	Use:   "state",
	Short: "Show state backend information",
	Long: `Display information about where infrastructure state is stored.

State can be stored locally, in S3-compatible storage (DO Spaces, Linode Object Storage, AWS S3),
or synced to the manager node for multi-machine deployments.

Configure state backend in tako.yaml:

  infrastructure:
    state:
      backend: s3                    # local, s3, or manager
      bucket: tako-state-myapp       # S3 bucket name
      encrypt: true                  # Encrypt state at rest

For S3-compatible storage, set these environment variables:
  - TAKO_STATE_PASSPHRASE: Encryption passphrase for state
  - For AWS: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  - For DO Spaces: SPACES_ACCESS_KEY_ID, SPACES_SECRET_ACCESS_KEY`,
	RunE: runInfraState,
}

var infraTypesCmd = &cobra.Command{
	Use:   "types [provider]",
	Short: "List available server types",
	Long: `List available server types for a cloud provider.

This command queries the provider API directly to get the current list
of available server types, including their specs and pricing.

Examples:
  tako infra types hetzner      # List Hetzner server types
  tako infra types digitalocean # List DigitalOcean droplet sizes
  tako infra types --clear      # Clear cached server types

The results are cached for 24 hours to reduce API calls.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInfraTypes,
}

var infraTypesClear bool

func init() {
	rootCmd.AddCommand(infraCmd)

	infraCmd.AddCommand(infraPreviewCmd)
	infraCmd.AddCommand(infraUpCmd)
	infraCmd.AddCommand(infraDestroyCmd)
	infraCmd.AddCommand(infraOutputsCmd)
	infraCmd.AddCommand(infraStatusCmd)
	infraCmd.AddCommand(infraValidateCmd)
	infraCmd.AddCommand(infraSwitchCmd)
	infraCmd.AddCommand(infraCompareCmd)
	infraCmd.AddCommand(infraStateCmd)
	infraCmd.AddCommand(infraTypesCmd)

	infraUpCmd.Flags().BoolVarP(&infraYes, "yes", "y", false, "Skip confirmation prompt")
	infraDestroyCmd.Flags().BoolVarP(&infraYes, "yes", "y", false, "Skip confirmation prompt")
	infraTypesCmd.Flags().BoolVar(&infraTypesClear, "clear", false, "Clear cached server types")
}

func runInfraPreview(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		return fmt.Errorf("no infrastructure section defined in tako.yaml")
	}

	takoDir := filepath.Join(".", ".tako")
	orchestrator, err := infra.NewOrchestrator(cfg, takoDir, getEnvironmentName(cfg), verbose)
	if err != nil {
		return err
	}

	fmt.Printf("Previewing infrastructure changes...\n\n")

	ctx := context.Background()
	result, err := orchestrator.Preview(ctx)
	if err != nil {
		return err
	}

	if result.Summary != "" {
		fmt.Println(result.Summary)
	}

	fmt.Printf("\nPreview completed in %s\n", result.Duration.Round(time.Millisecond))
	return nil
}

func runInfraUp(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		return fmt.Errorf("no infrastructure section defined in tako.yaml")
	}

	takoDir := filepath.Join(".", ".tako")
	orchestrator, err := infra.NewOrchestrator(cfg, takoDir, getEnvironmentName(cfg), verbose)
	if err != nil {
		return err
	}

	// Confirm unless -y flag
	if !infraYes {
		fmt.Printf("This will create/update infrastructure for %s\n", cfg.Project.Name)
		fmt.Print("Continue? [y/N] ")
		var response string
		fmt.Scanln(&response)
		if response != "y" && response != "Y" {
			fmt.Println("Cancelled")
			return nil
		}
	}

	fmt.Printf("Applying infrastructure changes...\n\n")

	ctx := context.Background()
	result, err := orchestrator.Provision(ctx)
	if err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("provisioning failed: %s", result.Error)
	}

	fmt.Println("\n=== Infrastructure Applied ===")
	for name, server := range result.Servers {
		fmt.Printf("%s: %s (%s)\n", name, server.PublicIP, server.Role)
	}

	fmt.Printf("\nCompleted in %s\n", result.Duration.Round(time.Millisecond))
	return nil
}

func runInfraDestroy(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		return fmt.Errorf("no infrastructure section defined in tako.yaml")
	}

	takoDir := filepath.Join(".", ".tako")
	orchestrator, err := infra.NewOrchestrator(cfg, takoDir, getEnvironmentName(cfg), verbose)
	if err != nil {
		return err
	}

	// Check if there's anything to destroy
	if !orchestrator.IsProvisioned() {
		fmt.Println("No infrastructure is currently provisioned")
		return nil
	}

	// Confirm unless -y flag
	if !infraYes {
		fmt.Printf("WARNING: This will DESTROY all infrastructure for %s\n", cfg.Project.Name)
		fmt.Printf("Provider: %s, Region: %s\n", cfg.Infrastructure.Provider, cfg.Infrastructure.Region)
		fmt.Print("\nType 'destroy' to confirm: ")
		var response string
		fmt.Scanln(&response)
		if response != "destroy" {
			fmt.Println("Cancelled")
			return nil
		}
	}

	fmt.Printf("Destroying infrastructure...\n\n")

	ctx := context.Background()
	result, err := orchestrator.Destroy(ctx)
	if err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("destroy failed: %s", result.Error)
	}

	fmt.Println("\nInfrastructure destroyed successfully")
	fmt.Printf("Completed in %s\n", result.Duration.Round(time.Millisecond))
	return nil
}

func runInfraOutputs(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		return fmt.Errorf("no infrastructure section defined in tako.yaml")
	}

	takoDir := filepath.Join(".", ".tako")
	stateMgr := infra.NewStateManagerWithConfig(takoDir, cfg.Project.Name, envFlag, cfg.Infrastructure)

	outputs, err := stateMgr.LoadOutputs()
	if err != nil {
		return fmt.Errorf("failed to load outputs: %w", err)
	}

	if outputs == nil {
		fmt.Println("No infrastructure outputs found")
		fmt.Println("Run 'tako provision' to create infrastructure")
		return nil
	}

	// Pretty print outputs
	data, err := json.MarshalIndent(outputs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format outputs: %w", err)
	}

	fmt.Println("Infrastructure Outputs:")
	fmt.Println(string(data))
	return nil
}

func runInfraStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		fmt.Println("No infrastructure section defined in tako.yaml")
		return nil
	}

	takoDir := filepath.Join(".", ".tako")
	stateMgr := infra.NewStateManagerWithConfig(takoDir, cfg.Project.Name, envFlag, cfg.Infrastructure)

	state, err := stateMgr.LoadInfraState()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	if state == nil {
		fmt.Println("Infrastructure Status: Not Provisioned")
		fmt.Println("\nRun 'tako provision' to create infrastructure")
		return nil
	}

	fmt.Println("Infrastructure Status: Provisioned")
	fmt.Printf("\nProvider:    %s\n", state.Provider)
	fmt.Printf("Region:      %s\n", state.Region)
	fmt.Printf("Environment: %s\n", state.Environment)
	fmt.Printf("Provisioned: %s\n", state.LastProvisioned.Format(time.RFC3339))

	if len(state.Servers) > 0 {
		fmt.Println("\nServers:")
		for name, server := range state.Servers {
			fmt.Printf("  %s:\n", name)
			fmt.Printf("    IP:     %s\n", server.PublicIP)
			fmt.Printf("    Role:   %s\n", server.Role)
			fmt.Printf("    Status: %s\n", server.Status)
		}
	}

	if state.Network != nil && state.Network.VPCID != "" {
		fmt.Println("\nNetwork:")
		fmt.Printf("  VPC ID:      %s\n", state.Network.VPCID)
		if state.Network.FirewallID != "" {
			fmt.Printf("  Firewall ID: %s\n", state.Network.FirewallID)
		}
	}

	return nil
}

func runInfraValidate(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		fmt.Println("No infrastructure section defined in tako.yaml")
		return nil
	}

	// Resolve config (applies defaults)
	if err := infra.ResolveInfraConfig(cfg.Infrastructure); err != nil {
		return fmt.Errorf("config resolution failed: %w", err)
	}

	// Validate
	errors := infra.ValidateConfig(cfg.Infrastructure)

	if len(errors) == 0 {
		fmt.Println("✓ Infrastructure configuration is valid")
		fmt.Printf("\nProvider: %s\n", cfg.Infrastructure.Provider)
		fmt.Printf("Region:   %s\n", cfg.Infrastructure.Region)

		// Show server summary
		totalServers := 0
		for name, spec := range cfg.Infrastructure.Servers {
			count := spec.Count
			if count < 1 {
				count = 1
			}
			totalServers += count
			fmt.Printf("\nServer '%s':\n", name)
			fmt.Printf("  Size:  %s\n", spec.Size)
			fmt.Printf("  Role:  %s\n", spec.Role)
			if count > 1 {
				fmt.Printf("  Count: %d\n", count)
			}
		}

		// Estimate cost
		infraServers := make(map[string]infra.InfraServerSpec)
		for name, spec := range cfg.Infrastructure.Servers {
			infraServers[name] = infra.InfraServerSpec{
				Size:  spec.Size,
				Count: spec.Count,
			}
		}
		cost := infra.EstimateMonthlyInfraCost(cfg.Infrastructure.Provider, infraServers)
		fmt.Printf("\nEstimated monthly cost: %s\n", infra.FormatPricing(cost))

		return nil
	}

	fmt.Println("✗ Infrastructure configuration has errors:")
	for _, e := range errors {
		fmt.Printf("  - %s\n", e)
	}
	return fmt.Errorf("validation failed with %d error(s)", len(errors))
}

func runInfraSwitch(cmd *cobra.Command, args []string) error {
	toProvider := args[0]

	// Validate target provider
	if !infra.IsValidProvider(toProvider) {
		return fmt.Errorf("invalid provider '%s', supported: %v", toProvider, infra.ValidProviders())
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		return fmt.Errorf("no infrastructure section defined in tako.yaml")
	}

	if cfg.Infrastructure.Provider == toProvider {
		fmt.Printf("Already using %s\n", toProvider)
		return nil
	}

	// Migrate config
	newInfra, warnings := infra.MigrateConfig(cfg.Infrastructure, toProvider)

	// Show migration preview
	fmt.Printf("Migration: %s → %s\n", cfg.Infrastructure.Provider, toProvider)
	fmt.Println(string(repeatRune('─', 40)))

	fmt.Printf("\nRegion: %s → %s\n", cfg.Infrastructure.Region, newInfra.Region)

	fmt.Println("\nServers:")
	for name, oldSpec := range cfg.Infrastructure.Servers {
		newSpec := newInfra.Servers[name]
		if oldSpec.Size != newSpec.Size {
			fmt.Printf("  %s: %s → %s\n", name, oldSpec.Size, newSpec.Size)
		} else {
			fmt.Printf("  %s: %s (unchanged)\n", name, newSpec.Size)
		}
	}

	// Show pricing difference
	oldCost := infra.EstimateMonthlyInfraCost(cfg.Infrastructure.Provider, toInfraServerSpecs(cfg.Infrastructure.Servers))
	newCost := infra.EstimateMonthlyInfraCost(toProvider, toInfraServerSpecs(newInfra.Servers))

	fmt.Printf("\nEstimated monthly cost: %s → %s", infra.FormatPricing(oldCost), infra.FormatPricing(newCost))
	if newCost < oldCost {
		fmt.Printf(" (save %s)\n", infra.FormatPricing(oldCost-newCost))
	} else if newCost > oldCost {
		fmt.Printf(" (+%s)\n", infra.FormatPricing(newCost-oldCost))
	} else {
		fmt.Println()
	}

	if len(warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range warnings {
			fmt.Printf("  ⚠ %s\n", w)
		}
	}

	// Confirm
	fmt.Print("\nApply this migration? [y/N] ")
	var response string
	fmt.Scanln(&response)
	if response != "y" && response != "Y" {
		fmt.Println("Cancelled")
		return nil
	}

	// Update config
	cfg.Infrastructure = newInfra

	// Save config
	if err := config.SaveConfig(cfgFile, cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n✓ Switched to %s\n", toProvider)
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Set %s environment variable\n", getProviderEnvVar(toProvider))
	fmt.Println("  2. Run 'tako provision' to create new infrastructure")
	fmt.Println("  3. Run 'tako infra destroy' on old provider if needed")

	return nil
}

func runInfraCompare(cmd *cobra.Command, args []string) error {
	provider1 := args[0]
	provider2 := args[1]

	if !infra.IsValidProvider(provider1) {
		return fmt.Errorf("invalid provider '%s'", provider1)
	}
	if !infra.IsValidProvider(provider2) {
		return fmt.Errorf("invalid provider '%s'", provider2)
	}

	infra.PrintProviderComparison(provider1, provider2)
	return nil
}

// Helper functions

func repeatRune(r rune, count int) []rune {
	result := make([]rune, count)
	for i := range result {
		result[i] = r
	}
	return result
}

func toInfraServerSpecs(servers map[string]config.InfraServerSpec) map[string]infra.InfraServerSpec {
	result := make(map[string]infra.InfraServerSpec)
	for name, spec := range servers {
		result[name] = infra.InfraServerSpec{
			Size:  spec.Size,
			Count: spec.Count,
		}
	}
	return result
}

func getProviderEnvVar(provider string) string {
	envVars := map[string]string{
		"digitalocean": "DIGITALOCEAN_TOKEN",
		"hetzner":      "HCLOUD_TOKEN",
		"linode":       "LINODE_TOKEN",
		"aws":          "AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY",
	}
	if v, ok := envVars[provider]; ok {
		return v
	}
	return "provider credentials"
}

func runInfraState(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Infrastructure == nil {
		fmt.Println("No infrastructure section defined in tako.yaml")
		fmt.Println("\nState backend: Local (default)")
		return nil
	}

	takoDir := filepath.Join(".", ".tako")
	stateMgr := infra.NewStateManagerWithConfig(takoDir, cfg.Project.Name, envFlag, cfg.Infrastructure)

	fmt.Println("State Backend Configuration")
	fmt.Println(string(repeatRune('─', 40)))

	backend := stateMgr.GetBackend()
	description := stateMgr.GetStateBackendDescription()

	fmt.Printf("\nBackend:  %s\n", backend)
	fmt.Printf("Location: %s\n", description)

	// Show S3-specific info
	if backend == infra.BackendS3 && cfg.Infrastructure.State != nil {
		state := cfg.Infrastructure.State
		if state.Bucket != "" {
			fmt.Printf("Bucket:   %s\n", state.Bucket)
		}
		if state.Region != "" {
			fmt.Printf("Region:   %s\n", state.Region)
		}
		if state.Endpoint != "" {
			fmt.Printf("Endpoint: %s\n", state.Endpoint)
		}
		if state.Encrypt {
			fmt.Println("Encrypt:  enabled")
		}
	}

	// Check provisioned state
	if stateMgr.IsProvisioned() {
		fmt.Println("\nState Status: Infrastructure provisioned")
		state, err := stateMgr.LoadInfraState()
		if err == nil && state != nil {
			fmt.Printf("Last Updated: %s\n", state.LastProvisioned.Format(time.RFC3339))
		}
	} else {
		fmt.Println("\nState Status: No infrastructure provisioned")
	}

	// Show helpful info for remote backends
	if backend == infra.BackendS3 {
		fmt.Println("\nMulti-machine deployment:")
		fmt.Println("  State is stored remotely and can be accessed from any machine.")
		fmt.Println("  Set TAKO_STATE_PASSPHRASE env var for encryption.")
	} else if backend == infra.BackendLocal {
		fmt.Println("\nNote: Local state cannot be shared across machines.")
		fmt.Println("  Configure remote state in tako.yaml:")
		fmt.Println()
		fmt.Println("  infrastructure:")
		fmt.Println("    state:")
		fmt.Println("      backend: s3")
		fmt.Printf("      bucket: %s\n", stateMgr.GetS3BucketSuggestion())
		fmt.Println("      encrypt: true")
	}

	return nil
}

func runInfraTypes(cmd *cobra.Command, args []string) error {
	fetcher := infra.GetTypesFetcher()

	// Handle --clear flag
	if infraTypesClear {
		provider := ""
		if len(args) > 0 {
			provider = args[0]
		}
		if err := fetcher.ClearCache(provider); err != nil {
			return fmt.Errorf("failed to clear cache: %w", err)
		}
		if provider == "" {
			fmt.Println("Cleared all server type caches")
		} else {
			fmt.Printf("Cleared server type cache for %s\n", provider)
		}
		return nil
	}

	// Determine provider
	var provider, token string
	if len(args) > 0 {
		provider = args[0]
	} else {
		// Try to get from config
		cfg, err := config.LoadConfig(cfgFile)
		if err == nil && cfg.Infrastructure != nil {
			provider = cfg.Infrastructure.Provider
			token = cfg.Infrastructure.Credentials.Token
		}
	}

	if provider == "" {
		fmt.Println("Usage: tako infra types <provider>")
		fmt.Println("\nSupported providers:")
		for _, p := range infra.ValidProviders() {
			fmt.Printf("  - %s\n", p)
		}
		return nil
	}

	// Validate provider
	if !infra.IsValidProvider(provider) {
		return fmt.Errorf("invalid provider '%s', supported: %v", provider, infra.ValidProviders())
	}

	// Get token if not from config
	if token == "" {
		token = getProviderToken(provider)
	}

	if token == "" {
		fmt.Printf("Warning: No API token found for %s. Set %s environment variable for dynamic validation.\n\n",
			provider, getProviderEnvVar(provider))
		fmt.Println("Showing cached/fallback server types:")
		fmt.Println()

		// Show fallback types
		types := infra.GetValidServerTypes(provider)
		if types == nil {
			return fmt.Errorf("no server types available for %s", provider)
		}

		fmt.Printf("Common server types for %s:\n\n", provider)
		for _, t := range types {
			fmt.Printf("  %s\n", t)
		}
		return nil
	}

	fmt.Printf("Fetching server types from %s API...\n\n", provider)

	output, err := fetcher.ListAvailableTypes(provider, token)
	if err != nil {
		return fmt.Errorf("failed to fetch server types: %w", err)
	}

	fmt.Print(output)
	return nil
}

func getProviderToken(provider string) string {
	envVars := map[string][]string{
		"hetzner":      {"HCLOUD_TOKEN"},
		"digitalocean": {"DIGITALOCEAN_TOKEN", "DO_TOKEN"},
		"linode":       {"LINODE_TOKEN"},
		"aws":          {"AWS_ACCESS_KEY_ID"},
	}

	if vars, ok := envVars[provider]; ok {
		for _, v := range vars {
			if val := getEnvOS(v); val != "" {
				return val
			}
		}
	}
	return ""
}

func getEnvOS(key string) string {
	// This needs to import os
	return lookupOSEnv(key)
}

var lookupOSEnv = osLookupEnv
