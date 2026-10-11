package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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
	vault           string
	project         string
	providerEnv     string
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
	Use:   "set KEY=value | KEY --stdin | KEY --from-file PATH",
	Short: "Set a secret value",
	Long: `Set a secret value in the .tako/secrets files.

A value passed as KEY=value is visible to other local users (ps,
/proc/<pid>/cmdline) while the command runs and may be saved in shell
history. Prefer one of the forms that keep the value off the command line:

  KEY --stdin             read the value from standard input
  KEY --from-file PATH    read the value from PATH

--stdin and --from-file are mutually exclusive and take a bare KEY. One
trailing newline is stripped from values read from stdin or a file.
To import many secrets at once, use 'tako secrets import --format env'.`,
	Example: `  printf '%s' "$DB_PASSWORD" | tako secrets set DB_PASSWORD --stdin --env production
  tako secrets set TLS_KEY --from-file ./certs/server.key
  op read op://Prod/api/token | tako secrets set API_TOKEN --stdin`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		useStdin, _ := cmd.Flags().GetBool("stdin")
		fromFile, _ := cmd.Flags().GetString("from-file")
		key, value, err := resolveSecretSetArg(args[0], useStdin, fromFile, cmd.InOrStdin())
		if err != nil {
			return err
		}

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

		fmt.Fprintf(cmd.OutOrStdout(), "✓ Secret '%s' saved to %s secrets\n", key, file)

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
		return formatConfigLoadError(cfgFile, err)
	}

	// Create manager; configured secretSources are resolved live so
	// validation proves the provider is reachable, not just configured.
	mgr, err := secrets.NewManagerForConfig(cfg, env)
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

Each provider shells out to its official CLI, which must be installed and
authenticated. Run 'tako secrets providers' for details.

` + secretsProviderHelp(),
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
	Use:   "import PROVIDER [NAME...] | import --format env [--file FILE]",
	Short: "Import provider secrets into encrypted Tako secrets",
	Long: `Import external provider secrets into encrypted .tako/secrets files.

By default this command performs a redacted dry run. Pass --write to persist
the imported values. Existing local secrets are skipped unless --overwrite is
provided.

With --format env no provider is used: a dotenv stream (KEY=value lines) is
read from standard input, or from --file, so values never appear on the
command line:

  vault-export | tako secrets import --format env --env production --write
  tako secrets import --format env --file prod.env --write

` + secretsProviderHelp(),
	Args: func(cmd *cobra.Command, args []string) error {
		if secretsImportFormat != "" {
			return cobra.NoArgs(cmd, args)
		}
		return cobra.MinimumNArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if secretsProviderFlags.write && secretsProviderFlags.dryRun {
			return fmt.Errorf("--write and --dry-run cannot be used together")
		}

		var provider string
		var values map[string]string
		var err error
		switch {
		case secretsImportFormat != "":
			if secretsImportFormat != "env" {
				return fmt.Errorf("unsupported --format %q (supported: env)", secretsImportFormat)
			}
			if secretsProviderFlags.prefixStrip != "" {
				return fmt.Errorf("--prefix-strip is not supported with --format env; use --map SOURCE=DEST")
			}
			provider = "dotenv"
			if values, err = readDotenvSecrets(secretsImportFile, cmd.InOrStdin()); err != nil {
				return err
			}
		case secretsImportFile != "":
			return fmt.Errorf("--file requires --format env")
		default:
			provider = args[0]
			values, err = secrets.FetchProviderSecrets(context.Background(), provider, args[1:], providerOptionsFromFlags())
			if err != nil {
				return err
			}
		}

		mappings, err := parseSecretMappings(secretsProviderFlags.maps)
		if err != nil {
			return err
		}
		var planned map[string]string
		if secretsImportFormat != "" {
			// Dotenv keys are already env names; keep them verbatim.
			planned, err = mapDotenvSecrets(values, mappings)
		} else {
			planned, err = secrets.MapProviderSecrets(values, secretsProviderFlags.prefixStrip, mappings)
		}
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

var secretsProvidersCmd = &cobra.Command{
	Use:   "providers",
	Short: "List supported external secrets providers",
	Long:  `List the external secrets providers usable with 'tako secrets fetch' and 'tako secrets import', and whether each provider's CLI is installed.`,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, info := range secrets.Providers() {
			status := "installed"
			if !secrets.ProviderCommandAvailable(info) {
				status = "not found"
			}
			fmt.Printf("%s\n", info.Name)
			fmt.Printf("  %s\n", info.Description)
			fmt.Printf("  CLI: %s (%s)\n", info.Command, status)
			if len(info.Aliases) > 0 {
				fmt.Printf("  Aliases: %s\n", strings.Join(info.Aliases, ", "))
			}
			if status != "installed" {
				fmt.Printf("  Install: %s\n", info.Install)
			}
			fmt.Println()
		}
		return nil
	},
}

func secretsProviderHelp() string {
	var b strings.Builder
	b.WriteString("Supported providers:\n")
	for _, info := range secrets.Providers() {
		fmt.Fprintf(&b, "  %-20s %s\n", info.Name, info.Description)
	}
	b.WriteString(`
Examples:
  tako secrets import aws-ssm --path /myapp/prod --prefix-strip /myapp/prod
  tako secrets import aws-secrets-manager myapp/prod --prefix-strip myapp/prod
  tako secrets import vault --path secret/myapp/prod --prefix-strip secret/myapp/prod
  tako secrets import 1password Postgres Stripe --vault Production
  tako secrets import doppler --project api --provider-env prd
  tako secrets import infisical --project <id> --provider-env prod
  tako secrets import gcp-secret-manager --project acme --path myapp-
  tako secrets import azure-key-vault --vault acme-kv --path myapp-
  tako secrets import bitwarden --project <id>`)
	return b.String()
}

func getEnvDisplay(env string) string {
	if env == "" {
		return "all environments"
	}
	return env
}

func providerOptionsFromFlags() secrets.ProviderOptions {
	return secrets.ProviderOptions{
		Profile:     secretsProviderFlags.profile,
		Region:      secretsProviderFlags.region,
		Path:        secretsProviderFlags.path,
		From:        secretsProviderFlags.from,
		Vault:       secretsProviderFlags.vault,
		Project:     secretsProviderFlags.project,
		ProviderEnv: secretsProviderFlags.providerEnv,
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
		if !secrets.IsValidSecretKey(target) {
			return nil, fmt.Errorf("invalid mapped secret key %q", target)
		}
		mappings[source] = target
	}
	return mappings, nil
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
	secretsCmd.AddCommand(secretsProvidersCmd)

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
		cmd.Flags().StringVar(&secretsProviderFlags.profile, "profile", "", "AWS CLI profile (aws-ssm, aws-secrets-manager)")
		cmd.Flags().StringVar(&secretsProviderFlags.region, "region", "", "AWS region (aws-ssm, aws-secrets-manager)")
		cmd.Flags().StringVar(&secretsProviderFlags.path, "path", "", "Provider path/prefix to import recursively (Infisical: secret folder)")
		cmd.Flags().StringVar(&secretsProviderFlags.from, "from", "", "Provider folder/name prefix for explicit names")
		cmd.Flags().StringVar(&secretsProviderFlags.vault, "vault", "", "1Password vault or Azure Key Vault name")
		cmd.Flags().StringVar(&secretsProviderFlags.project, "project", "", "GCP project, Doppler project, Infisical project ID, or Bitwarden project ID")
		cmd.Flags().StringVar(&secretsProviderFlags.providerEnv, "provider-env", "", "Provider-side environment: Doppler config or Infisical environment slug")
		cmd.Flags().StringVarP(&secretsProviderFlags.env, "env", "e", "", "Environment (e.g., production, staging)")
		cmd.Flags().StringVar(&secretsProviderFlags.prefixStrip, "prefix-strip", "", "Strip this provider prefix before deriving local secret keys")
		cmd.Flags().StringArrayVar(&secretsProviderFlags.maps, "map", nil, "Map provider source to local secret key (SOURCE=DEST)")
		cmd.Flags().BoolVar(&secretsProviderFlags.dryRun, "dry-run", false, "Preview provider import without writing")
		cmd.Flags().BoolVar(&secretsProviderFlags.debugShowValues, "debug-show-values", false, "Print plaintext secret values")
	}
	secretsImportCmd.Flags().BoolVar(&secretsProviderFlags.write, "write", false, "Write imported secrets to encrypted .tako/secrets files")
	secretsImportCmd.Flags().BoolVar(&secretsProviderFlags.overwrite, "overwrite", false, "Overwrite existing local secrets")
	secretsImportCmd.Flags().StringVar(&secretsImportFormat, "format", "", "Import a local stream instead of a provider (supported: env)")
	secretsImportCmd.Flags().StringVar(&secretsImportFile, "file", "", "Read the --format env stream from FILE instead of stdin ('-' = stdin)")
	secretsSetCmd.Flags().Bool("stdin", false, "Read the secret value from stdin (one trailing newline is stripped)")
	secretsSetCmd.Flags().String("from-file", "", "Read the secret value from PATH (one trailing newline is stripped)")
	secretsSetCmd.MarkFlagsMutuallyExclusive("stdin", "from-file")
}

var (
	secretsImportFormat string
	secretsImportFile   string
)
