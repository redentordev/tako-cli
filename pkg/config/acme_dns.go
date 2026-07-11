package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// rawACMEDNSDocument is a pre-expansion shadow of the environment ACME
// blocks. Once ${VAR} expansion has happened a secret value is
// indistinguishable from a literal, so this check must run on raw input.
type rawACMEDNSDocument struct {
	Environments map[string]struct {
		Proxy *struct {
			ACME *struct {
				Credentials map[string]string `yaml:"credentials" json:"credentials"`
			} `yaml:"acme" json:"acme"`
		} `yaml:"proxy" json:"proxy"`
	} `yaml:"environments" json:"environments"`
}

func validateRawACMEDNSCredentials(data []byte, isJSON bool) error {
	var doc rawACMEDNSDocument
	if isJSON {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil // the strict parse after expansion reports syntax errors
		}
	} else if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}

	for envName, env := range doc.Environments {
		if env.Proxy == nil || env.Proxy.ACME == nil {
			continue
		}
		keys := make([]string, 0, len(env.Proxy.ACME.Credentials))
		for key := range env.Proxy.ACME.Credentials {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := strings.TrimSpace(env.Proxy.ACME.Credentials[key])
			if value == "" || !envRefPattern.MatchString(value) {
				return fmt.Errorf("environment %s proxy.acme.credentials.%s must be an environment variable reference like ${DNS_API_TOKEN}; literal credentials in the config file are not allowed", envName, key)
			}
		}
	}
	return nil
}

func validateEnvironmentACME(envName string, proxy *EnvironmentProxyConfig) error {
	if proxy == nil || proxy.ACME == nil {
		return nil
	}
	acme := proxy.ACME
	acme.DNSProvider = strings.ToLower(strings.TrimSpace(acme.DNSProvider))
	allowed := map[string]map[string]bool{
		ACMEDNSProviderCloudflare: {
			"apiToken":  true,
			"zoneToken": false,
		},
		ACMEDNSProviderHetzner: {
			"apiToken": true,
		},
		ACMEDNSProviderDigitalOcean: {
			"apiToken": true,
		},
	}
	required, ok := allowed[acme.DNSProvider]
	if !ok {
		return fmt.Errorf("environment %s proxy.acme.dnsProvider must be one of cloudflare, hetzner, digitalocean", envName)
	}
	for key, value := range acme.Credentials {
		if _, ok := required[key]; !ok {
			return fmt.Errorf("environment %s proxy.acme.credentials contains unsupported %s credential %q", envName, acme.DNSProvider, key)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("environment %s proxy.acme.credentials.%s is required", envName, key)
		}
		if hasConfigControlChars(value) {
			return fmt.Errorf("environment %s proxy.acme.credentials.%s contains control characters", envName, key)
		}
	}
	for key, isRequired := range required {
		if isRequired && strings.TrimSpace(acme.Credentials[key]) == "" {
			return fmt.Errorf("environment %s proxy.acme.credentials.%s is required for %s", envName, key, acme.DNSProvider)
		}
	}
	return nil
}

// EnvironmentACME returns the normalized ACME config for an environment.
func (c *Config) EnvironmentACME(envName string) *EnvironmentACMEConfig {
	if c == nil {
		return nil
	}
	env, ok := c.Environments[envName]
	if !ok || env.Proxy == nil {
		return nil
	}
	return env.Proxy.ACME
}
