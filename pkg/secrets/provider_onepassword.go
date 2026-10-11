package secrets

import (
	"context"
	"fmt"
	"strings"
)

type onePasswordItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Fields []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Value any    `json:"value"`
	} `json:"fields"`
}

// fetchOnePasswordSecrets reads 1Password items through the `op` CLI.
// NAME arguments are item names/IDs (returned as ITEM/field-label) or
// op://vault/item/field secret references (returned as vault/item/field).
// With no names, every item in --vault is read. Authentication is the CLI's
// own: a signed-in session, OP_SERVICE_ACCOUNT_TOKEN, or a Connect server.
func fetchOnePasswordSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	names = prefixedSecretNames(names, options.From)
	vault := strings.TrimSpace(options.Vault)
	if len(names) == 0 {
		if vault == "" {
			return nil, fmt.Errorf("provide 1Password item names, op:// references, or --vault to read every item")
		}
		var items []onePasswordItem
		if err := runProviderJSON(ctx, &items, "1Password", "op", "item", "list", "--vault", vault, "--format", "json"); err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("no 1Password items found in vault %s", vault)
		}
		for _, item := range items {
			names = append(names, item.ID)
		}
	}

	results := make(map[string]string)
	for _, name := range names {
		if reference, ok := strings.CutPrefix(name, "op://"); ok {
			value, err := runProviderCommand(ctx, "op", "read", "--no-newline", name)
			if err != nil {
				return nil, err
			}
			results[reference] = value
			continue
		}
		args := []string{"item", "get", name, "--format", "json"}
		if vault != "" {
			args = append(args, "--vault", vault)
		}
		var item onePasswordItem
		if err := runProviderJSON(ctx, &item, "1Password", "op", args...); err != nil {
			return nil, err
		}
		title := item.Title
		if title == "" {
			title = name
		}
		for _, field := range item.Fields {
			value := secretValueString(field.Value)
			if field.Value == nil || value == "" {
				continue
			}
			label := field.Label
			if label == "" {
				label = field.ID
			}
			results[title+"/"+label] = value
		}
	}
	return results, nil
}
