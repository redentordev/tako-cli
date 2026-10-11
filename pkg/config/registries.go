package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry credential providers accepted in registries.<host>.provider.
const (
	RegistryProviderECR              = "ecr"
	RegistryProviderGCR              = "gcr"
	RegistryProviderArtifactRegistry = "artifact-registry"
	RegistryProviderGHCR             = "ghcr"
)

// RegistryProviderNames lists the accepted provider values in display order.
func RegistryProviderNames() []string {
	return []string{RegistryProviderECR, RegistryProviderGCR, RegistryProviderArtifactRegistry, RegistryProviderGHCR}
}

// NormalizeRegistryProvider maps a provider value to its canonical adapter
// name (artifact-registry is an alias of gcr). Unknown values return "".
func NormalizeRegistryProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case RegistryProviderECR:
		return RegistryProviderECR
	case RegistryProviderGCR, RegistryProviderArtifactRegistry:
		return RegistryProviderGCR
	case RegistryProviderGHCR:
		return RegistryProviderGHCR
	default:
		return ""
	}
}

var (
	ecrHostPattern      = regexp.MustCompile(`^\d{12}\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
	gcrHostPattern      = regexp.MustCompile(`^([a-z0-9-]+\.)?gcr\.io$`)
	artifactHostPattern = regexp.MustCompile(`^[a-z0-9-]+-docker\.pkg\.dev$`)
)

// ECRRegionFromHost returns the AWS region encoded in an ECR registry host
// (<account>.dkr.ecr[-fips].<region>.amazonaws.com[.cn]).
func ECRRegionFromHost(host string) (string, bool) {
	match := ecrHostPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(host)))
	if match == nil {
		return "", false
	}
	return match[2], true
}

// RegistryProviderMatchesHost reports whether a canonical provider can mint
// credentials for host.
func RegistryProviderMatchesHost(provider, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch NormalizeRegistryProvider(provider) {
	case RegistryProviderECR:
		return ecrHostPattern.MatchString(host)
	case RegistryProviderGCR:
		return gcrHostPattern.MatchString(host) || artifactHostPattern.MatchString(host)
	case RegistryProviderGHCR:
		return host == "ghcr.io"
	default:
		return false
	}
}

// ImageRegistryHost extracts the registry host from an image reference. Bare
// and namespaced Docker Hub references map to docker.io; a first path
// segment containing '.' or ':' or equal to localhost is the host.
func ImageRegistryHost(imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	idx := strings.IndexByte(imageRef, '/')
	if idx < 0 {
		return "docker.io"
	}
	first := imageRef[:idx]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

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
// secret is indistinguishable from a literal one. Provider entries carry no
// password and pass; provider-plus-password is rejected after expansion.
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
		if strings.TrimSpace(registry.Provider) != "" {
			if err := validateProviderRegistry(trimmedHost, registry); err != nil {
				return err
			}
			continue
		}
		if registry.Region != "" || registry.Profile != "" || registry.Account != "" {
			return fmt.Errorf("registry %s: region, profile and account are only valid with provider", host)
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

func validateProviderRegistry(host string, registry RegistryConfig) error {
	provider := NormalizeRegistryProvider(registry.Provider)
	if provider == "" {
		return fmt.Errorf("registry %s: unsupported provider %q (supported: %s)", host, registry.Provider, strings.Join(RegistryProviderNames(), ", "))
	}
	if registry.Password != "" {
		return fmt.Errorf("registry %s: provider and password are mutually exclusive", host)
	}
	if registry.Username != "" && provider != RegistryProviderGHCR {
		return fmt.Errorf("registry %s: username is only valid with provider ghcr; %s credentials use a fixed username", host, registry.Provider)
	}
	if (registry.Region != "" || registry.Profile != "") && provider != RegistryProviderECR {
		return fmt.Errorf("registry %s: region and profile are only valid with provider ecr", host)
	}
	if registry.Account != "" && provider != RegistryProviderGCR {
		return fmt.Errorf("registry %s: account is only valid with provider gcr or artifact-registry", host)
	}
	for _, value := range []string{registry.Username, registry.Region, registry.Profile, registry.Account} {
		if hasConfigControlChars(value) || strings.HasPrefix(strings.TrimSpace(value), "-") {
			return fmt.Errorf("registry %s: provider options must not contain control characters or start with '-'", host)
		}
	}
	if !RegistryProviderMatchesHost(provider, host) {
		return fmt.Errorf("registry %s: provider %s requires %s", host, registry.Provider, registryProviderHostHint(provider))
	}
	if provider == RegistryProviderECR && registry.Region != "" {
		hostRegion, _ := ECRRegionFromHost(host)
		if strings.TrimSpace(registry.Region) != hostRegion {
			return fmt.Errorf("registry %s: region %q conflicts with region %q in the ECR host", host, registry.Region, hostRegion)
		}
	}
	return nil
}

func registryProviderHostHint(provider string) string {
	switch provider {
	case RegistryProviderECR:
		return "an ECR registry host like <account>.dkr.ecr.<region>.amazonaws.com"
	case RegistryProviderGCR:
		return "a gcr.io, *.gcr.io or *-docker.pkg.dev registry host"
	default:
		return "the ghcr.io registry host"
	}
}

func hasConfigControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
