package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// envRefPattern matches a whole-value ${ENV_VAR} reference, the only form
// allowed for registry passwords in config files.
var envRefPattern = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// rawRegistriesDocument is the pre-expansion shadow of the registries block,
// parsed leniently so the env-ref check can run before ${VAR} substitution.
type rawRegistriesDocument struct {
	Registries map[string]struct {
		Username string `yaml:"username" json:"username"`
		Password string `yaml:"password" json:"password"`
	} `yaml:"registries" json:"registries"`
}

// validateRawRegistryCredentials rejects literal registry passwords in the
// raw config content. It runs before env expansion — afterwards a resolved
// secret is indistinguishable from a literal one.
func validateRawRegistryCredentials(data []byte, isJSON bool) error {
	var doc rawRegistriesDocument
	if isJSON {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil // the strict parse after expansion reports the real error
		}
	} else {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil
		}
	}
	for host, registry := range doc.Registries {
		password := strings.TrimSpace(registry.Password)
		if password == "" {
			continue
		}
		if !envRefPattern.MatchString(password) {
			return fmt.Errorf("registry %s: password must be an environment variable reference like ${REGISTRY_TOKEN}; literal credentials in the config file are not allowed", host)
		}
	}
	return nil
}

// validateRegistries validates the expanded registries block.
func validateRegistries(registries map[string]RegistryConfig) error {
	for host, registry := range registries {
		trimmedHost := strings.TrimSpace(host)
		if trimmedHost == "" {
			return fmt.Errorf("registries: registry host cannot be empty")
		}
		if strings.ContainsAny(trimmedHost, " \t\"\\") || hasConfigControlChars(trimmedHost) {
			return fmt.Errorf("registries: invalid registry host %q", host)
		}
		if strings.TrimSpace(registry.Username) == "" {
			return fmt.Errorf("registry %s: username is required", host)
		}
		if registry.Password == "" {
			return fmt.Errorf("registry %s: password is required (use ${ENV_VAR})", host)
		}
		if hasConfigControlChars(registry.Username) || hasConfigControlChars(registry.Password) {
			return fmt.Errorf("registry %s: credentials must not contain control characters", host)
		}
	}
	return nil
}

func hasConfigControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
