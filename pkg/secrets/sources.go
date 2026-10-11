package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
)

// sourceCache memoizes resolved sources for the life of the process. One
// deploy builds env files for many services and also registers secrets with
// redactors; each of those must not re-query the provider.
var sourceCache = struct {
	sync.Mutex
	values map[string]map[string]string
}{values: make(map[string]map[string]string)}

// ResolveSecretSources reads every source through its provider adapter and
// maps the results to Tako secret keys. Later sources override earlier
// ones, mirroring how environment secrets override common secrets.
func ResolveSecretSources(ctx context.Context, sources []config.SecretSource) (map[string]string, error) {
	resolved := make(map[string]string)
	for i, source := range sources {
		values, err := resolveSecretSource(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("secret source %d (%s): %w", i+1, source.Provider, err)
		}
		for key, value := range values {
			resolved[key] = value
		}
	}
	return resolved, nil
}

func resolveSecretSource(ctx context.Context, source config.SecretSource) (map[string]string, error) {
	fingerprint, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	cacheKey := string(fingerprint)

	sourceCache.Lock()
	cached, ok := sourceCache.values[cacheKey]
	sourceCache.Unlock()
	if ok {
		return cached, nil
	}

	values, err := FetchProviderSecrets(ctx, source.Provider, source.Names, ProviderOptions{
		Profile:     source.Profile,
		Region:      source.Region,
		Path:        source.Path,
		From:        source.From,
		Vault:       source.Vault,
		Project:     source.Project,
		ProviderEnv: source.ProviderEnv,
	})
	if err != nil {
		return nil, err
	}
	mapped, err := MapProviderSecrets(values, source.PrefixStrip, source.Map)
	if err != nil {
		return nil, err
	}

	sourceCache.Lock()
	sourceCache.values[cacheKey] = mapped
	sourceCache.Unlock()
	return mapped, nil
}

func resetSecretSourceCache() {
	sourceCache.Lock()
	sourceCache.values = make(map[string]map[string]string)
	sourceCache.Unlock()
}

// NewManagerForConfig creates a manager for environment that layers the
// config's secretSources over the local .tako/secrets files. Provider
// values win: the external store is the source of truth, so a stale local
// copy can never shadow a rotated secret.
func NewManagerForConfig(cfg *config.Config, environment string) (*Manager, error) {
	m, err := NewManager(environment)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return m, nil
	}
	sources := cfg.SecretSourcesFor(environment)
	if len(sources) == 0 {
		return m, nil
	}
	resolved, err := ResolveSecretSources(context.Background(), sources)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve secretSources: %w", err)
	}
	m.mu.Lock()
	m.sourced = resolved
	m.mu.Unlock()
	if err := m.loadSecrets(); err != nil {
		return nil, fmt.Errorf("failed to load secrets: %w", err)
	}
	return m, nil
}
