package secrets

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
)

// fetchGCPSecretManagerSecrets reads the latest version of Google Cloud
// Secret Manager secrets through `gcloud`. --project overrides the gcloud
// default project; --path imports every secret whose name starts with the
// given prefix.
func fetchGCPSecretManagerSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	names = prefixedSecretNames(names, options.From)
	if prefix := strings.TrimSpace(options.Path); prefix != "" {
		listed, err := listGCPSecrets(ctx, prefix, options)
		if err != nil {
			return nil, err
		}
		names = append(names, listed...)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("provide GCP Secret Manager secret names or --path")
	}

	results := make(map[string]string, len(names))
	for _, name := range names {
		args := []string{"secrets", "versions", "access", "latest", "--secret=" + name, "--format=json"}
		args = appendGCPFlags(args, options)
		var response struct {
			Payload struct {
				Data string `json:"data"`
			} `json:"payload"`
		}
		if err := runProviderJSON(ctx, &response, "GCP Secret Manager", "gcloud", args...); err != nil {
			return nil, err
		}
		value, err := decodeGCPPayload(response.Payload.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode GCP secret %s: %w", name, err)
		}
		results[name] = value
	}
	return results, nil
}

func listGCPSecrets(ctx context.Context, prefix string, options ProviderOptions) ([]string, error) {
	args := appendGCPFlags([]string{"secrets", "list", "--format=json"}, options)
	var secrets []struct {
		Name string `json:"name"`
	}
	if err := runProviderJSON(ctx, &secrets, "GCP Secret Manager", "gcloud", args...); err != nil {
		return nil, err
	}
	var names []string
	for _, secret := range secrets {
		// gcloud reports full resource names: projects/P/secrets/NAME.
		name := path.Base(secret.Name)
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no GCP secrets found with name prefix %s", prefix)
	}
	sort.Strings(names)
	return names, nil
}

func decodeGCPPayload(data string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(data)
	}
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func appendGCPFlags(args []string, options ProviderOptions) []string {
	if project := strings.TrimSpace(options.Project); project != "" {
		args = append(args, "--project="+project)
	}
	return args
}
