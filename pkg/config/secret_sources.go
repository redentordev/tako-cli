package config

import (
	"fmt"
	"regexp"
	"strings"
)

// SecretSource declares secrets that are read live from an external secret
// store (AWS SSM, Vault, 1Password, Doppler, ...) every time Tako needs them,
// instead of being copied into .tako/secrets. A source holds only provider
// selectors, never secret values, so it is safe to commit in tako.yaml.
type SecretSource struct {
	// Provider is a registered secrets provider name or alias (see
	// `tako secrets providers`).
	Provider string `yaml:"provider" json:"provider"`
	// Names lists explicit provider secret names; empty means the provider's
	// listing mode (path, vault, project/config).
	Names       []string `yaml:"names,omitempty" json:"names,omitempty"`
	Path        string   `yaml:"path,omitempty" json:"path,omitempty"`
	From        string   `yaml:"from,omitempty" json:"from,omitempty"`
	Profile     string   `yaml:"profile,omitempty" json:"profile,omitempty"`
	Region      string   `yaml:"region,omitempty" json:"region,omitempty"`
	Vault       string   `yaml:"vault,omitempty" json:"vault,omitempty"`
	Project     string   `yaml:"project,omitempty" json:"project,omitempty"`
	ProviderEnv string   `yaml:"providerEnv,omitempty" json:"providerEnv,omitempty"`
	// PrefixStrip is removed from provider names before deriving keys.
	PrefixStrip string `yaml:"prefixStrip,omitempty" json:"prefixStrip,omitempty"`
	// Map renames provider secret names to explicit Tako secret keys.
	Map map[string]string `yaml:"map,omitempty" json:"map,omitempty"`
}

var secretKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretSourcesFor returns the sources that apply to an environment: the
// top-level sources first, then the environment's own, so later sources
// override earlier ones key by key.
func (c *Config) SecretSourcesFor(envName string) []SecretSource {
	sources := append([]SecretSource(nil), c.SecretSources...)
	if env, ok := c.Environments[envName]; ok {
		sources = append(sources, env.SecretSources...)
	}
	return sources
}

func validateSecretSources(scope string, sources []SecretSource) error {
	for i, source := range sources {
		label := fmt.Sprintf("%s[%d]", scope, i)
		if strings.TrimSpace(source.Provider) == "" {
			return fmt.Errorf("%s: provider is required", label)
		}
		for _, field := range append([]string{source.Provider, source.Path, source.From, source.Profile, source.Region, source.Vault, source.Project, source.ProviderEnv, source.PrefixStrip}, source.Names...) {
			if hasConfigControlChars(field) {
				return fmt.Errorf("%s: values must not contain control characters", label)
			}
		}
		for from, to := range source.Map {
			if strings.TrimSpace(from) == "" {
				return fmt.Errorf("%s: map source names cannot be empty", label)
			}
			if !secretKeyPattern.MatchString(to) {
				return fmt.Errorf("%s: map target %q is not a valid secret key", label, to)
			}
		}
	}
	return nil
}
