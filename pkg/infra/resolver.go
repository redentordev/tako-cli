package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ProviderEnvVars maps providers to their default environment variable names
var ProviderEnvVars = map[string]string{
	"digitalocean": "DIGITALOCEAN_TOKEN",
	"hetzner":      "HCLOUD_TOKEN",
	"linode":       "LINODE_TOKEN",
}

// AWSEnvVars for AWS credentials
var AWSEnvVars = struct {
	AccessKey string
	SecretKey string
}{
	AccessKey: "AWS_ACCESS_KEY_ID",
	SecretKey: "AWS_SECRET_ACCESS_KEY",
}

// ResolveInfraConfig resolves and validates infrastructure configuration
// - Resolves credentials from environment variables if not specified
// - Resolves generic sizes to provider-specific sizes
// - Applies defaults to server specs
// - Resolves region aliases
func ResolveInfraConfig(infra *config.InfrastructureConfig) error {
	if infra == nil {
		return nil
	}

	// Validate provider
	if !IsValidProvider(infra.Provider) {
		return fmt.Errorf("invalid provider '%s', supported: %v", infra.Provider, ValidProviders())
	}

	// Resolve credentials from env vars if not set
	if err := resolveCredentials(infra); err != nil {
		return err
	}

	// Resolve region alias
	infra.Region = ResolveRegion(infra.Provider, infra.Region)

	// Set SSH defaults
	if infra.SSHUser == "" {
		infra.SSHUser = "root"
	}
	if infra.SSHKey == "" {
		// Try common SSH key paths
		for _, keyPath := range []string{"~/.ssh/id_ed25519", "~/.ssh/id_rsa"} {
			expanded := expandPath(keyPath)
			if _, err := os.Stat(expanded); err == nil {
				infra.SSHKey = keyPath
				break
			}
		}
	}

	// Apply defaults and resolve sizes for each server
	for name, spec := range infra.Servers {
		resolvedSpec := resolveServerSpec(infra, spec)
		infra.Servers[name] = resolvedSpec
	}

	// Auto-configure networking if not specified
	if infra.Networking == nil {
		infra.Networking = autoConfigureNetworking(infra.Servers)
	} else {
		// Fill in missing networking config
		if infra.Networking.Firewall == nil {
			infra.Networking.Firewall = autoConfigureNetworking(infra.Servers).Firewall
		}
	}

	return nil
}

// autoConfigureNetworking sets up VPC and firewall with smart defaults
func autoConfigureNetworking(servers map[string]config.InfraServerSpec) *config.InfraNetworkingConfig {
	// Count total servers
	totalServers := 0
	for _, spec := range servers {
		count := spec.Count
		if count < 1 {
			count = 1
		}
		totalServers += count
	}

	// Base firewall rules - always needed
	rules := []config.InfraFirewallRule{
		// SSH access
		{Protocol: "tcp", Ports: []int{22}, Sources: []string{"0.0.0.0/0"}},
		// HTTP/HTTPS for web traffic
		{Protocol: "tcp", Ports: []int{80, 443}, Sources: []string{"0.0.0.0/0"}},
	}

	// Multi-server: add Docker Swarm ports (internal only)
	if totalServers > 1 {
		rules = append(rules,
			// Swarm management
			config.InfraFirewallRule{Protocol: "tcp", Ports: []int{2377}, Sources: []string{"10.0.0.0/8"}},
			// Node communication
			config.InfraFirewallRule{Protocol: "tcp", Ports: []int{7946}, Sources: []string{"10.0.0.0/8"}},
			config.InfraFirewallRule{Protocol: "udp", Ports: []int{7946}, Sources: []string{"10.0.0.0/8"}},
			// Overlay network
			config.InfraFirewallRule{Protocol: "udp", Ports: []int{4789}, Sources: []string{"10.0.0.0/8"}},
		)
	}

	return &config.InfraNetworkingConfig{
		VPC: &config.InfraVPCConfig{
			Enabled: totalServers > 1, // VPC only needed for multi-server
			IPRange: "10.0.0.0/16",
		},
		Firewall: &config.InfraFirewallConfig{
			Enabled: true,
			Rules:   rules,
		},
	}
}

// resolveCredentials populates credentials from environment variables
func resolveCredentials(infra *config.InfrastructureConfig) error {
	if infra.Provider == "aws" {
		// AWS uses access key + secret key
		if infra.Credentials.AccessKey == "" {
			infra.Credentials.AccessKey = os.Getenv(AWSEnvVars.AccessKey)
		}
		if infra.Credentials.SecretKey == "" {
			infra.Credentials.SecretKey = os.Getenv(AWSEnvVars.SecretKey)
		}
		if infra.Credentials.AccessKey == "" || infra.Credentials.SecretKey == "" {
			return fmt.Errorf("AWS requires credentials: set %s and %s environment variables, or specify in config",
				AWSEnvVars.AccessKey, AWSEnvVars.SecretKey)
		}
	} else {
		// Token-based providers (DO, Hetzner, Linode)
		if infra.Credentials.Token == "" {
			envVar := ProviderEnvVars[infra.Provider]
			infra.Credentials.Token = os.Getenv(envVar)
		}
		if infra.Credentials.Token == "" {
			envVar := ProviderEnvVars[infra.Provider]
			return fmt.Errorf("%s requires a token: set %s environment variable, or specify credentials.token in config",
				infra.Provider, envVar)
		}
	}
	return nil
}

// resolveServerSpec applies defaults and resolves sizes
func resolveServerSpec(infra *config.InfrastructureConfig, spec config.InfraServerSpec) config.InfraServerSpec {
	// Determine effective provider and region for this server
	provider := spec.Provider
	if provider == "" {
		provider = infra.Provider
	}
	region := spec.Region
	if region == "" {
		region = infra.Region
	}

	// Validate provider if overridden
	if spec.Provider != "" && !IsValidProvider(spec.Provider) {
		// Keep original, will fail validation later
		provider = infra.Provider
	}

	// Apply defaults
	if infra.Defaults != nil {
		if spec.Size == "" {
			spec.Size = infra.Defaults.Size
		}
		if spec.Image == "" {
			spec.Image = infra.Defaults.Image
		}
		if len(spec.SSHKeys) == 0 {
			spec.SSHKeys = infra.Defaults.SSHKeys
		}
		if len(spec.Tags) == 0 {
			spec.Tags = infra.Defaults.Tags
		}
	}

	// Apply final defaults
	if spec.Size == "" {
		spec.Size = "medium"
	}
	if spec.Image == "" {
		spec.Image = GetDefaultImage(provider)
	}
	if spec.Count < 1 {
		spec.Count = 1
	}
	if spec.Role == "" {
		spec.Role = "worker"
	}

	// Resolve generic size to provider-specific
	spec.Size = ResolveSize(provider, spec.Size)

	// Resolve region
	spec.Region = ResolveRegion(provider, region)

	// Store resolved provider
	spec.Provider = provider

	return spec
}

// GetServerProvider returns the effective provider for a server
func GetServerProvider(infra *config.InfrastructureConfig, spec config.InfraServerSpec) string {
	if spec.Provider != "" {
		return spec.Provider
	}
	return infra.Provider
}

// GetServerRegion returns the effective region for a server
func GetServerRegion(infra *config.InfrastructureConfig, spec config.InfraServerSpec) string {
	if spec.Region != "" {
		return spec.Region
	}
	return infra.Region
}

// GetUsedProviders returns all providers used in the infrastructure config
func GetUsedProviders(infra *config.InfrastructureConfig) []string {
	providers := make(map[string]bool)
	providers[infra.Provider] = true

	for _, spec := range infra.Servers {
		if spec.Provider != "" {
			providers[spec.Provider] = true
		}
	}

	result := make([]string, 0, len(providers))
	for p := range providers {
		result = append(result, p)
	}
	return result
}

// IsMultiProvider returns true if config uses multiple providers
func IsMultiProvider(infra *config.InfrastructureConfig) bool {
	return len(GetUsedProviders(infra)) > 1
}

// GenerateServersConfig creates the servers config section from infrastructure outputs
func GenerateServersConfig(infra *config.InfrastructureConfig, outputs map[string]interface{}) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig)

	if infra == nil || outputs == nil {
		return servers
	}

	sshKey := infra.SSHKey
	sshUser := infra.SSHUser
	if sshUser == "" {
		sshUser = "root"
	}

	for name, spec := range infra.Servers {
		count := spec.Count
		if count < 1 {
			count = 1
		}

		for i := 0; i < count; i++ {
			serverName := name
			if count > 1 {
				serverName = fmt.Sprintf("%s-%d", name, i)
			}

			// Get IP from outputs
			ipKey := fmt.Sprintf("%s_ip", serverName)
			ip, ok := outputs[ipKey].(string)
			if !ok || ip == "" {
				continue // Skip if no IP available
			}

			// Determine role
			role := spec.Role
			if role == "" {
				role = "worker"
			}

			servers[serverName] = config.ServerConfig{
				Host:   ip,
				User:   sshUser,
				SSHKey: sshKey,
				Role:   role,
			}
		}
	}

	return servers
}

// MergeServersConfig merges generated servers with existing config
// Existing config takes precedence
func MergeServersConfig(existing, generated map[string]config.ServerConfig) map[string]config.ServerConfig {
	result := make(map[string]config.ServerConfig)

	// Add generated servers
	for name, server := range generated {
		result[name] = server
	}

	// Override with existing (existing takes precedence)
	for name, server := range existing {
		result[name] = server
	}

	return result
}

// GetEnvironmentServers returns server names for an environment from infrastructure
func GetEnvironmentServers(infra *config.InfrastructureConfig) []string {
	var servers []string

	if infra == nil {
		return servers
	}

	for name, spec := range infra.Servers {
		count := spec.Count
		if count < 1 {
			count = 1
		}

		for i := 0; i < count; i++ {
			serverName := name
			if count > 1 {
				serverName = fmt.Sprintf("%s-%d", name, i)
			}
			servers = append(servers, serverName)
		}
	}

	return servers
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
