package secrets

import (
	"context"
	"fmt"
	"strings"
)

// fetchInfisicalSecrets exports an Infisical environment through the
// `infisical` CLI. --project is the project ID, --provider-env the
// environment slug (dev, staging, prod) and --path the secret folder.
// Authentication is the CLI's own: `infisical login` or INFISICAL_TOKEN.
// NAME arguments select individual secrets.
func fetchInfisicalSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	args := []string{"export", "--format=json"}
	if env := strings.TrimSpace(options.ProviderEnv); env != "" {
		args = append(args, "--env="+env)
	}
	if project := strings.TrimSpace(options.Project); project != "" {
		args = append(args, "--projectId="+project)
	}
	if path := strings.TrimSpace(options.Path); path != "" {
		args = append(args, "--path="+path)
	}
	var entries []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := runProviderJSON(ctx, &entries, "Infisical", "infisical", args...); err != nil {
		return nil, err
	}
	all := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.Key == "" {
			continue
		}
		if existing, ok := all[entry.Key]; ok && existing != entry.Value {
			return nil, fmt.Errorf("infisical returned conflicting values for %s; narrow with --path", entry.Key)
		}
		all[entry.Key] = entry.Value
	}
	return selectSecretKeys(all, names, "Infisical")
}
