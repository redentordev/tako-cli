package secrets

import (
	"context"
	"fmt"
	"strings"
)

// fetchVaultKVSecrets reads HashiCorp Vault or OpenBao KV secrets through the
// matching CLI (both share the `kv` command surface). Each field of a secret
// at PATH is returned as PATH/field. Connection settings come from the CLI's
// own environment (VAULT_ADDR, VAULT_TOKEN, VAULT_NAMESPACE or BAO_*).
func fetchVaultKVSecrets(ctx context.Context, command string, names []string, options ProviderOptions) (map[string]string, error) {
	paths := prefixedSecretNames(names, options.From)
	if root := strings.Trim(strings.TrimSpace(options.Path), "/"); root != "" {
		listed, err := listVaultKVPaths(ctx, command, root)
		if err != nil {
			return nil, err
		}
		paths = append(paths, listed...)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("provide %s secret paths or --path", command)
	}

	results := make(map[string]string)
	for _, path := range paths {
		var response struct {
			Data map[string]any `json:"data"`
		}
		if err := runProviderJSON(ctx, &response, command+" kv", command, "kv", "get", "-format=json", path); err != nil {
			return nil, err
		}
		flattenSecretObject(results, strings.Trim(path, "/"), vaultKVData(response.Data))
	}
	return results, nil
}

// vaultKVData unwraps a KV v2 response ({data: {data, metadata}}) and passes
// KV v1 data through unchanged.
func vaultKVData(data map[string]any) map[string]any {
	inner, hasData := data["data"].(map[string]any)
	_, hasMetadata := data["metadata"].(map[string]any)
	if hasData && hasMetadata {
		return inner
	}
	return data
}

func listVaultKVPaths(ctx context.Context, command string, root string) ([]string, error) {
	var keys []string
	if err := runProviderJSON(ctx, &keys, command+" kv", command, "kv", "list", "-format=json", root); err != nil {
		return nil, err
	}
	var paths []string
	for _, key := range keys {
		child := root + "/" + strings.TrimSuffix(key, "/")
		if strings.HasSuffix(key, "/") {
			nested, err := listVaultKVPaths(ctx, command, child)
			if err != nil {
				return nil, err
			}
			paths = append(paths, nested...)
			continue
		}
		paths = append(paths, child)
	}
	return paths, nil
}
