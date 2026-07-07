package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage deployment secrets",
	Long:  `Manage secrets stored in .tako/secrets files. Secrets are stored locally and never committed to git.`,
}

var secretsProviderFlags struct {
	profile         string
	region          string
	path            string
	from            string
	env             string
	prefixStrip     string
	maps            []string
	dryRun          bool
	write           bool
	overwrite       bool
	debugShowValues bool
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
		if err := fileutil.WriteFileAtomic(".tako/.gitignore", []byte(secrets.GitignoreContent), 0644); err != nil {
			return fmt.Errorf("failed to create .gitignore: %w", err)
		}

		// Create empty secrets files
		files := []string{"secrets", "secrets.staging", "secrets.production"}
		for _, file := range files {
			path := fmt.Sprintf(".tako/%s", file)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				content := fmt.Sprintf("# Tako secrets file: %s\n# Add your secrets here in KEY=value format\n", file)
				if err := fileutil.WriteFileAtomic(path, []byte(content), 0600); err != nil {
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
	RunE:  runSecretsList,
}

func runSecretsList(cmd *cobra.Command, args []string) error {
	env, _ := cmd.Flags().GetString("env")

	// Create manager
	mgr, err := secrets.NewManager(env)
	if err != nil {
		return err
	}

	// Get all keys
	keys := mgr.List()

	// Machine modes reserve stdout for parseable output. The result document
	// carries KEYS only; secret values never reach any machine output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "No secrets configured")
	} else {
		fmt.Fprintf(out, "Secrets (%s):\n", getEnvDisplay(env))
		for _, key := range keys {
			fmt.Fprintf(out, "  %s=[REDACTED]\n", key)
		}
		fmt.Fprintf(out, "\nTotal: %d secret(s)\n", len(keys))
	}

	return emitResultDocument(engine.SecretsListResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSecretsListResult,
		Environment: env,
		Keys:        keys,
		Count:       len(keys),
	})
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
	RunE:  runSecretsValidate,
}

func runSecretsValidate(cmd *cobra.Command, args []string) error {
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
	sort.Strings(required)

	missing := []string{}
	for _, key := range required {
		if !mgr.Has(key) {
			missing = append(missing, key)
		}
	}

	result := engine.SecretsValidateResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSecretsValidateResult,
		Project:     cfg.Project.Name,
		Environment: env,
		Valid:       len(missing) == 0,
		Required:    required,
		Missing:     missing,
	}

	if len(missing) > 0 {
		if emitErr := emitResultDocument(result); emitErr != nil {
			return emitErr
		}
		return &engine.InvalidRequestError{Err: fmt.Errorf("missing required secrets: %s", strings.Join(missing, ", "))}
	}

	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}
	fmt.Fprintln(out, "✓ All required secrets are configured")
	fmt.Fprintf(out, "  Environment: %s\n", getEnvDisplay(env))
	fmt.Fprintf(out, "  Required: %d\n", len(required))
	fmt.Fprintf(out, "  Configured: %d\n", len(required))

	return emitResultDocument(result)
}

var secretsFetchCmd = &cobra.Command{
	Use:   "fetch PROVIDER [NAME...]",
	Short: "Fetch secrets from an external provider",
	Long: `Fetch secrets from an external provider and print only redacted values by default.

Supported providers:
  aws-ssm
  aws-secrets-manager`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider := args[0]
		names := args[1:]

		values, err := secrets.FetchProviderSecrets(context.Background(), provider, names, providerOptionsFromFlags())
		if err != nil {
			return err
		}
		keys := sortedSecretMapKeys(values)
		payload := make(map[string]string, len(keys))
		for _, key := range keys {
			if secretsProviderFlags.debugShowValues {
				payload[key] = values[key]
			} else {
				payload[key] = "[REDACTED]"
			}
		}
		out := map[string]any{
			"provider":    provider,
			"environment": getEnvDisplay(secretsProviderFlags.env),
			"count":       len(keys),
			"secrets":     payload,
		}
		encoded, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		if !secretsProviderFlags.debugShowValues {
			fmt.Println("Values redacted. Use --debug-show-values only when you deliberately need plaintext output.")
		}
		return nil
	},
}

var secretsImportCmd = &cobra.Command{
	Use:   "import PROVIDER [NAME...]",
	Short: "Import provider secrets into encrypted Tako secrets",
	Long: `Import external provider secrets into encrypted .tako/secrets files.

By default this command performs a redacted dry run. Pass --write to persist
the imported values. Existing local secrets are skipped unless --overwrite is
provided.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if secretsProviderFlags.write && secretsProviderFlags.dryRun {
			return fmt.Errorf("--write and --dry-run cannot be used together")
		}

		provider := args[0]
		names := args[1:]
		values, err := secrets.FetchProviderSecrets(context.Background(), provider, names, providerOptionsFromFlags())
		if err != nil {
			return err
		}

		mappings, err := parseSecretMappings(secretsProviderFlags.maps)
		if err != nil {
			return err
		}
		planned, err := mapImportedSecrets(values, secretsProviderFlags.prefixStrip, mappings)
		if err != nil {
			return err
		}
		keys := sortedSecretMapKeys(planned)

		fmt.Printf("Provider: %s\n", provider)
		fmt.Printf("Environment: %s\n", getEnvDisplay(secretsProviderFlags.env))
		fmt.Printf("Discovered: %d secret(s)\n", len(values))
		fmt.Printf("Mapped: %d secret(s)\n", len(keys))
		for _, key := range keys {
			fmt.Printf("  %s=[REDACTED]\n", key)
		}

		if !secretsProviderFlags.write {
			fmt.Println("\nDry run only. Re-run with --write to update encrypted Tako secrets.")
			return nil
		}

		mgr, err := secrets.NewManager(secretsProviderFlags.env)
		if err != nil {
			return err
		}
		written := 0
		skipped := 0
		for _, key := range keys {
			if mgr.Has(key) && !secretsProviderFlags.overwrite {
				skipped++
				fmt.Printf("  Skipped existing: %s\n", key)
				continue
			}
			if err := mgr.Set(key, planned[key], secretsProviderFlags.env); err != nil {
				return fmt.Errorf("failed to write secret %s: %w", key, err)
			}
			written++
		}
		fmt.Printf("\nImported %d secret(s)", written)
		if skipped > 0 {
			fmt.Printf(" (%d skipped; use --overwrite to replace)", skipped)
		}
		fmt.Println()
		return nil
	},
}

func getEnvDisplay(env string) string {
	if env == "" {
		return "all environments"
	}
	return env
}

func providerOptionsFromFlags() secrets.ProviderOptions {
	return secrets.ProviderOptions{
		Profile: secretsProviderFlags.profile,
		Region:  secretsProviderFlags.region,
		Path:    secretsProviderFlags.path,
		From:    secretsProviderFlags.from,
	}
}

func parseSecretMappings(values []string) (map[string]string, error) {
	mappings := make(map[string]string, len(values))
	for _, value := range values {
		source, target, ok := strings.Cut(value, "=")
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if !ok || source == "" || target == "" {
			return nil, fmt.Errorf("invalid --map value %q, use SOURCE=DEST", value)
		}
		if !isValidImportedSecretKey(target) {
			return nil, fmt.Errorf("invalid mapped secret key %q", target)
		}
		mappings[source] = target
	}
	return mappings, nil
}

func mapImportedSecrets(values map[string]string, prefixStrip string, mappings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for source, value := range values {
		key, ok := mappings[source]
		if !ok {
			key = importedSecretKey(source, prefixStrip)
		}
		if !isValidImportedSecretKey(key) {
			return nil, fmt.Errorf("provider secret %q maps to invalid env key %q", source, key)
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("multiple provider secrets map to %s; use --map to disambiguate", key)
		}
		out[key] = value
	}
	return out, nil
}

func importedSecretKey(source string, prefixStrip string) string {
	key := strings.TrimSpace(source)
	if prefixStrip != "" {
		prefix := strings.TrimSpace(prefixStrip)
		if strings.HasPrefix(key, prefix) {
			key = strings.TrimPrefix(key, prefix)
		}
		key = strings.TrimLeft(key, "/")
	}
	key = strings.Trim(key, "/")
	var out strings.Builder
	lastWasSeparator := false
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToUpper(r))
			lastWasSeparator = false
			continue
		}
		if !lastWasSeparator {
			out.WriteRune('_')
			lastWasSeparator = true
		}
	}
	result := strings.Trim(out.String(), "_")
	if result == "" {
		return "SECRET"
	}
	if result[0] >= '0' && result[0] <= '9' {
		result = "SECRET_" + result
	}
	return result
}

func isValidImportedSecretKey(key string) bool {
	if key == "" {
		return false
	}
	first := rune(key[0])
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for _, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

func sortedSecretMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func init() {
	rootCmd.AddCommand(secretsCmd)

	// Add subcommands
	secretsCmd.AddCommand(secretsInitCmd)
	secretsCmd.AddCommand(secretsSetCmd)
	secretsCmd.AddCommand(secretsListCmd)
	secretsCmd.AddCommand(secretsDeleteCmd)
	secretsCmd.AddCommand(secretsValidateCmd)
	secretsCmd.AddCommand(secretsFetchCmd)
	secretsCmd.AddCommand(secretsImportCmd)

	// Add environment flag to relevant commands
	for _, cmd := range []*cobra.Command{
		secretsSetCmd,
		secretsListCmd,
		secretsDeleteCmd,
		secretsValidateCmd,
	} {
		cmd.Flags().StringP("env", "e", "", "Environment (e.g., production, staging)")
	}

	for _, cmd := range []*cobra.Command{
		secretsFetchCmd,
		secretsImportCmd,
	} {
		cmd.Flags().StringVar(&secretsProviderFlags.profile, "profile", "", "AWS CLI profile")
		cmd.Flags().StringVar(&secretsProviderFlags.region, "region", "", "AWS region")
		cmd.Flags().StringVar(&secretsProviderFlags.path, "path", "", "Provider path/prefix to import recursively")
		cmd.Flags().StringVar(&secretsProviderFlags.from, "from", "", "Provider folder/name prefix for explicit names")
		cmd.Flags().StringVarP(&secretsProviderFlags.env, "env", "e", "", "Environment (e.g., production, staging)")
		cmd.Flags().StringVar(&secretsProviderFlags.prefixStrip, "prefix-strip", "", "Strip this provider prefix before deriving local secret keys")
		cmd.Flags().StringArrayVar(&secretsProviderFlags.maps, "map", nil, "Map provider source to local secret key (SOURCE=DEST)")
		cmd.Flags().BoolVar(&secretsProviderFlags.dryRun, "dry-run", false, "Preview provider import without writing")
		cmd.Flags().BoolVar(&secretsProviderFlags.debugShowValues, "debug-show-values", false, "Print plaintext secret values")
	}
	secretsImportCmd.Flags().BoolVar(&secretsProviderFlags.write, "write", false, "Write imported secrets to encrypted .tako/secrets files")
	secretsImportCmd.Flags().BoolVar(&secretsProviderFlags.overwrite, "overwrite", false, "Overwrite existing local secrets")
}
