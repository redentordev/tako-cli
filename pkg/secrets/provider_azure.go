package secrets

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// fetchAzureKeyVaultSecrets reads Azure Key Vault secrets through `az`.
// --vault names the Key Vault; --path imports every enabled secret whose
// name starts with the given prefix. Authentication is the CLI's own
// (`az login`, managed identity, or a service principal).
func fetchAzureKeyVaultSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	vault := strings.TrimSpace(options.Vault)
	if vault == "" {
		return nil, fmt.Errorf("azure-key-vault provider requires --vault")
	}
	names = prefixedSecretNames(names, options.From)
	if prefix := strings.TrimSpace(options.Path); prefix != "" {
		listed, err := listAzureKeyVaultSecrets(ctx, vault, prefix)
		if err != nil {
			return nil, err
		}
		names = append(names, listed...)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("provide Azure Key Vault secret names or --path")
	}

	results := make(map[string]string, len(names))
	for _, name := range names {
		var response struct {
			Value string `json:"value"`
		}
		args := []string{"keyvault", "secret", "show", "--vault-name", vault, "--name", name, "--output", "json"}
		if err := runProviderJSON(ctx, &response, "Azure Key Vault", "az", args...); err != nil {
			return nil, err
		}
		results[name] = response.Value
	}
	return results, nil
}

func listAzureKeyVaultSecrets(ctx context.Context, vault string, prefix string) ([]string, error) {
	var secrets []struct {
		Name       string `json:"name"`
		Attributes struct {
			Enabled *bool `json:"enabled"`
		} `json:"attributes"`
	}
	args := []string{"keyvault", "secret", "list", "--vault-name", vault, "--output", "json"}
	if err := runProviderJSON(ctx, &secrets, "Azure Key Vault", "az", args...); err != nil {
		return nil, err
	}
	var names []string
	for _, secret := range secrets {
		if secret.Attributes.Enabled != nil && !*secret.Attributes.Enabled {
			continue
		}
		if strings.HasPrefix(secret.Name, prefix) {
			names = append(names, secret.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no enabled Azure Key Vault secrets found with name prefix %s", prefix)
	}
	sort.Strings(names)
	return names, nil
}
