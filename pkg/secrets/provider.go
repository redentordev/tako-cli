package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/internal/providercli"
)

const (
	ProviderAWSSSM              = "aws-ssm"
	ProviderAWSSecretsManager   = "aws-secrets-manager"
	ProviderVault               = "vault"
	ProviderOpenBao             = "openbao"
	ProviderOnePassword         = "1password"
	ProviderDoppler             = "doppler"
	ProviderInfisical           = "infisical"
	ProviderGCPSecretManager    = "gcp-secret-manager"
	ProviderAzureKeyVault       = "azure-key-vault"
	ProviderBitwardenSecretsMgr = "bitwarden"
)

var (
	providerCommandContext = exec.CommandContext
	providerCommandTimeout = providercli.DefaultTimeout
	providerLookPath       = exec.LookPath
)

// ProviderOptions carries the adapter-specific selectors shared by
// `tako secrets fetch` and `tako secrets import`. Each adapter documents
// which fields it reads; unused fields are ignored.
type ProviderOptions struct {
	// Profile is the AWS CLI profile.
	Profile string
	// Region is the AWS region.
	Region string
	// Path lists every secret under a provider path/prefix instead of
	// reading explicit names.
	Path string
	// From is a folder/name prefix prepended to explicit names.
	From string
	// Vault is the 1Password vault or Azure Key Vault name.
	Vault string
	// Project is the GCP project, Doppler project, Infisical project ID or
	// Bitwarden Secrets Manager project ID.
	Project string
	// ProviderEnv is the provider-side environment: the Doppler config or
	// the Infisical environment slug.
	ProviderEnv string
}

// ProviderInfo describes a registered secrets provider adapter.
type ProviderInfo struct {
	Name        string
	Aliases     []string
	Command     string
	Description string
	Install     string
}

type providerAdapter struct {
	ProviderInfo
	fetch func(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error)
}

var providerRegistry = []providerAdapter{
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderAWSSSM,
			Aliases:     []string{"ssm", "aws_ssm", "aws-ssm-parameter-store", "aws_ssm_parameter_store", "aws-parameter-store"},
			Command:     "aws",
			Description: "AWS Systems Manager Parameter Store (names, --from prefix, or --path recursive)",
			Install:     "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html",
		},
		fetch: fetchAWSSSMSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderAWSSecretsManager,
			Aliases:     []string{"aws_secrets_manager", "secretsmanager", "aws-sm"},
			Command:     "aws",
			Description: "AWS Secrets Manager (names or --path name prefix; JSON secrets are flattened)",
			Install:     "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html",
		},
		fetch: fetchAWSSecretsManagerSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderVault,
			Aliases:     []string{"hashicorp-vault", "hashicorp_vault", "vault-kv"},
			Command:     "vault",
			Description: "HashiCorp Vault KV v1/v2 (secret paths or --path recursive; uses VAULT_ADDR/VAULT_TOKEN)",
			Install:     "https://developer.hashicorp.com/vault/install",
		},
		fetch: func(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
			return fetchVaultKVSecrets(ctx, "vault", names, options)
		},
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderOpenBao,
			Aliases:     []string{"bao"},
			Command:     "bao",
			Description: "OpenBao KV v1/v2 (secret paths or --path recursive; uses BAO_ADDR/BAO_TOKEN)",
			Install:     "https://openbao.org/docs/install/",
		},
		fetch: func(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
			return fetchVaultKVSecrets(ctx, "bao", names, options)
		},
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderOnePassword,
			Aliases:     []string{"onepassword", "op", "1pass"},
			Command:     "op",
			Description: "1Password (item names with --vault, op:// references, or every item in --vault)",
			Install:     "https://developer.1password.com/docs/cli/get-started/",
		},
		fetch: fetchOnePasswordSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderDoppler,
			Command:     "doppler",
			Description: "Doppler (--project and --provider-env config; all secrets or selected names)",
			Install:     "https://docs.doppler.com/docs/install-cli",
		},
		fetch: fetchDopplerSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderInfisical,
			Command:     "infisical",
			Description: "Infisical (--project ID, --provider-env slug, optional --path folder)",
			Install:     "https://infisical.com/docs/cli/overview",
		},
		fetch: fetchInfisicalSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderGCPSecretManager,
			Aliases:     []string{"gcp", "gcp-sm", "google-secret-manager", "gcp_secret_manager"},
			Command:     "gcloud",
			Description: "Google Cloud Secret Manager (secret names or --path name prefix; latest version)",
			Install:     "https://cloud.google.com/sdk/docs/install",
		},
		fetch: fetchGCPSecretManagerSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderAzureKeyVault,
			Aliases:     []string{"azure", "azure-kv", "akv", "azure_key_vault", "keyvault"},
			Command:     "az",
			Description: "Azure Key Vault (--vault with secret names or --path name prefix)",
			Install:     "https://learn.microsoft.com/cli/azure/install-azure-cli",
		},
		fetch: fetchAzureKeyVaultSecrets,
	},
	{
		ProviderInfo: ProviderInfo{
			Name:        ProviderBitwardenSecretsMgr,
			Aliases:     []string{"bws", "bitwarden-secrets-manager", "bitwarden_secrets_manager"},
			Command:     "bws",
			Description: "Bitwarden Secrets Manager (optional --project ID; uses BWS_ACCESS_TOKEN)",
			Install:     "https://bitwarden.com/help/secrets-manager-cli/",
		},
		fetch: fetchBitwardenSecrets,
	},
}

// Providers lists the registered secrets provider adapters.
func Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(providerRegistry))
	for _, adapter := range providerRegistry {
		out = append(out, adapter.ProviderInfo)
	}
	return out
}

// ProviderNames lists the canonical names of the registered adapters.
func ProviderNames() []string {
	names := make([]string, 0, len(providerRegistry))
	for _, adapter := range providerRegistry {
		names = append(names, adapter.Name)
	}
	return names
}

// ProviderCommandAvailable reports whether the provider's CLI is on PATH.
func ProviderCommandAvailable(info ProviderInfo) bool {
	_, err := providerLookPath(info.Command)
	return err == nil
}

func FetchProviderSecrets(ctx context.Context, provider string, names []string, options ProviderOptions) (map[string]string, error) {
	adapter, ok := lookupProvider(provider)
	if !ok {
		return nil, fmt.Errorf("unsupported secrets provider %q (supported: %s)", provider, strings.Join(ProviderNames(), ", "))
	}
	if _, err := providerLookPath(adapter.Command); err != nil {
		return nil, providercli.MissingCLIError(adapter.Name, adapter.Command, adapter.Install)
	}
	return adapter.fetch(ctx, names, options)
}

func lookupProvider(provider string) (providerAdapter, bool) {
	name := normalizeProvider(provider)
	for _, adapter := range providerRegistry {
		if adapter.Name == name {
			return adapter, true
		}
	}
	return providerAdapter{}, false
}

func normalizeProvider(provider string) string {
	name := strings.ToLower(strings.TrimSpace(provider))
	for _, adapter := range providerRegistry {
		if adapter.Name == name {
			return adapter.Name
		}
		for _, alias := range adapter.Aliases {
			if alias == name {
				return adapter.Name
			}
		}
	}
	return name
}

// flattenSecretValue stores value under name, or, when value is a JSON
// object, stores each top-level field under name/field.
func flattenSecretValue(results map[string]string, name string, value string) {
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil || object == nil {
		results[name] = value
		return
	}
	flattenSecretObject(results, name, object)
}

func flattenSecretObject(results map[string]string, name string, object map[string]any) {
	for key, item := range object {
		resultKey := key
		if name != "" {
			resultKey = name + "/" + key
		}
		results[resultKey] = secretValueString(item)
	}
}

func secretValueString(item any) string {
	if str, ok := item.(string); ok {
		return str
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return fmt.Sprint(item)
	}
	return string(encoded)
}

func prefixedSecretNames(names []string, from string) []string {
	out := make([]string, 0, len(names))
	from = strings.TrimRight(strings.TrimSpace(from), "/")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if from != "" {
			name = from + "/" + strings.TrimLeft(name, "/")
		}
		out = append(out, name)
	}
	return out
}

// selectSecretKeys narrows a provider's full key/value listing to the
// requested names. No names selects everything; a requested name that the
// provider did not return is an error.
func selectSecretKeys(all map[string]string, names []string, providerLabel string) (map[string]string, error) {
	names = prefixedSecretNames(names, "")
	if len(names) == 0 {
		return all, nil
	}
	results := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		value, ok := all[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		results[name] = value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing %s secret(s): %s", providerLabel, strings.Join(missing, ", "))
	}
	return results, nil
}

func stringBatches(values []string, size int) [][]string {
	if size <= 0 || len(values) == 0 {
		return nil
	}
	var batches [][]string
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		batches = append(batches, values[start:end])
	}
	return batches
}

func runProviderJSON(ctx context.Context, target any, label string, name string, args ...string) error {
	output, err := runProviderCommand(ctx, name, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(output), target); err != nil {
		return fmt.Errorf("failed to parse %s response: %w", label, err)
	}
	return nil
}

// runProviderCommand runs a secrets provider CLI through the shared helper.
// Secret listings keep the historical behavior of falling back to stdout in
// errors when stderr is empty.
func runProviderCommand(ctx context.Context, name string, args ...string) (string, error) {
	return providercli.Run(ctx, providercli.Options{
		Command: providerCommandContext,
		Timeout: providerCommandTimeout,
	}, name, args...)
}
