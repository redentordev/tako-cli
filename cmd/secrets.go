package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/spf13/cobra"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage deployment secrets",
	Long:  `Manage secrets stored in .tako/secrets files. Secrets are stored locally and never committed to git.`,
}

var secretsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize secrets management",
	Long:  `Initialize Tako secrets management by creating the .tako directory and secrets files.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create .tako directory
		if err := os.MkdirAll(".tako", 0700); err != nil {
			return fmt.Errorf("failed to create .tako directory: %w", err)
		}

		// Create .gitignore
		gitignore := `.tako/secrets*
`
		if err := os.WriteFile(".tako/.gitignore", []byte(gitignore), 0644); err != nil {
			return fmt.Errorf("failed to create .gitignore: %w", err)
		}

		// Create empty secrets files
		files := []string{"secrets", "secrets.staging", "secrets.production"}
		for _, file := range files {
			path := fmt.Sprintf(".tako/%s", file)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				content := fmt.Sprintf("# Tako secrets file: %s\n# Add your secrets here in KEY=value format\n", file)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					return fmt.Errorf("failed to create %s: %w", file, err)
				}
			}
		}

		fmt.Println("✓ Secrets management initialized")
		fmt.Println("  Created: .tako/secrets (common secrets)")
		fmt.Println("  Created: .tako/secrets.staging")
		fmt.Println("  Created: .tako/secrets.production")
		fmt.Println("\nNext steps:")
		fmt.Println("  1. Add secrets: tako secrets set KEY=value")
		fmt.Println("  2. Deploy: tako deploy --env production")

		return nil
	},
}

var secretsSetCmd = &cobra.Command{
	Use:   "set KEY=value",
	Short: "Set a secret value",
	Long:  `Set a secret value in the .tako/secrets files.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Parse KEY=value
		parts := strings.SplitN(args[0], "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid format, use: KEY=value")
		}

		key := parts[0]
		value := parts[1]

		// Get environment flag
		env, _ := cmd.Flags().GetString("env")

		// Create manager
		mgr, err := secrets.NewManager(env)
		if err != nil {
			return err
		}

		// Set the secret
		if err := mgr.Set(key, value, env); err != nil {
			return err
		}

		// Determine which file was updated
		file := "common"
		if env != "" {
			file = env
		}

		fmt.Printf("✓ Secret '%s' saved to %s secrets\n", key, file)

		return nil
	},
}

var secretsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all secret keys",
	Long:  `List all secret keys (values are redacted for security).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		env, _ := cmd.Flags().GetString("env")

		// Create manager
		mgr, err := secrets.NewManager(env)
		if err != nil {
			return err
		}

		// Get all keys
		keys := mgr.List()

		if len(keys) == 0 {
			fmt.Println("No secrets configured")
			return nil
		}

		fmt.Printf("Secrets (%s):\n", getEnvDisplay(env))
		for _, key := range keys {
			fmt.Printf("  %s=[REDACTED]\n", key)
		}

		fmt.Printf("\nTotal: %d secret(s)\n", len(keys))

		return nil
	},
}

var secretsDeleteCmd = &cobra.Command{
	Use:   "delete KEY",
	Short: "Delete a secret",
	Long:  `Delete a secret from the .tako/secrets files.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		env, _ := cmd.Flags().GetString("env")

		// Create manager
		mgr, err := secrets.NewManager(env)
		if err != nil {
			return err
		}

		// Delete the secret
		if err := mgr.Delete(key, env); err != nil {
			return err
		}

		file := "common"
		if env != "" {
			file = env
		}

		fmt.Printf("✓ Secret '%s' deleted from %s secrets\n", key, file)

		return nil
	},
}

var secretsValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate required secrets are present",
	Long:  `Validate that all required secrets referenced in tako.yaml are configured.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		env, _ := cmd.Flags().GetString("env")

		// Load config
		cfg, err := config.LoadConfig(cfgFile)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		// Create manager
		mgr, err := secrets.NewManager(env)
		if err != nil {
			return err
		}

		// Get services for the environment
		services, err := cfg.GetServices(env)
		if err != nil {
			return fmt.Errorf("failed to get services: %w", err)
		}

		// Collect all required secrets (string array format)
		required := []string{}
		for _, service := range services {
			// Service.Secrets is now []string in the new format
			required = append(required, service.Secrets...)
		}

		// Validate
		if err := mgr.ValidateRequired(required); err != nil {
			return err
		}

		fmt.Println("✓ All required secrets are configured")
		fmt.Printf("  Environment: %s\n", getEnvDisplay(env))
		fmt.Printf("  Required: %d\n", len(required))
		fmt.Printf("  Configured: %d\n", len(required))

		return nil
	},
}

func getEnvDisplay(env string) string {
	if env == "" {
		return "all environments"
	}
	return env
}

func init() {
	rootCmd.AddCommand(secretsCmd)

	// Add subcommands
	secretsCmd.AddCommand(secretsInitCmd)
	secretsCmd.AddCommand(secretsSetCmd)
	secretsCmd.AddCommand(secretsListCmd)
	secretsCmd.AddCommand(secretsDeleteCmd)
	secretsCmd.AddCommand(secretsValidateCmd)

	// Add environment flag to relevant commands
	for _, cmd := range []*cobra.Command{
		secretsSetCmd,
		secretsListCmd,
		secretsDeleteCmd,
		secretsValidateCmd,
	} {
		cmd.Flags().StringP("env", "e", "", "Environment (e.g., production, staging)")
	}
}
