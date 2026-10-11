package secrets

import (
	"context"
	"fmt"
	"strings"
)

// fetchBitwardenSecrets lists Bitwarden Secrets Manager secrets through the
// `bws` CLI, optionally scoped to the --project ID. The machine-account
// token comes from BWS_ACCESS_TOKEN. NAME arguments select secrets by key.
func fetchBitwardenSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	args := []string{"secret", "list"}
	if project := strings.TrimSpace(options.Project); project != "" {
		args = append(args, project)
	}
	args = append(args, "--output", "json")
	var entries []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := runProviderJSON(ctx, &entries, "Bitwarden Secrets Manager", "bws", args...); err != nil {
		return nil, err
	}
	all := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.Key == "" {
			continue
		}
		if existing, ok := all[entry.Key]; ok && existing != entry.Value {
			return nil, fmt.Errorf("bitwarden returned conflicting values for %s; scope the import with --project", entry.Key)
		}
		all[entry.Key] = entry.Value
	}
	return selectSecretKeys(all, names, "Bitwarden")
}
