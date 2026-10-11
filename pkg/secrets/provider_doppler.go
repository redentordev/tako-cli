package secrets

import (
	"context"
	"strings"
)

// dopplerMetadataKeys are injected by `doppler secrets download` into every
// config; they describe the source and are not application secrets.
var dopplerMetadataKeys = map[string]bool{
	"DOPPLER_PROJECT":     true,
	"DOPPLER_CONFIG":      true,
	"DOPPLER_ENVIRONMENT": true,
}

// fetchDopplerSecrets downloads a Doppler config through the `doppler` CLI.
// --project and --provider-env (the Doppler config, e.g. prd) fall back to
// the directory scope from `doppler setup`; DOPPLER_TOKEN service tokens work
// as usual. NAME arguments select individual secrets.
func fetchDopplerSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	args := []string{"secrets", "download", "--no-file", "--format", "json"}
	if project := strings.TrimSpace(options.Project); project != "" {
		args = append(args, "--project", project)
	}
	if config := strings.TrimSpace(options.ProviderEnv); config != "" {
		args = append(args, "--config", config)
	}
	var all map[string]string
	if err := runProviderJSON(ctx, &all, "Doppler", "doppler", args...); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		for key := range dopplerMetadataKeys {
			delete(all, key)
		}
	}
	return selectSecretKeys(all, names, "Doppler")
}
