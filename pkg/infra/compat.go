package infra

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ValidProtocols defines allowed firewall protocols
var ValidProtocols = []string{"tcp", "udp", "icmp"}

// ValidatePort checks if a port number is valid (1-65535)
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is out of valid range (1-65535)", port)
	}
	return nil
}

// ValidatePorts checks if all ports in a slice are valid
func ValidatePorts(ports []int) error {
	for _, port := range ports {
		if err := ValidatePort(port); err != nil {
			return err
		}
	}
	return nil
}

// ValidateProtocol checks if a protocol is valid
func ValidateProtocol(protocol string) error {
	p := strings.ToLower(protocol)
	for _, valid := range ValidProtocols {
		if p == valid {
			return nil
		}
	}
	return fmt.Errorf("invalid protocol '%s', must be one of: %s", protocol, strings.Join(ValidProtocols, ", "))
}

// ValidateCIDR checks if a CIDR block is valid
func ValidateCIDR(cidr string) error {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid CIDR block '%s': %w", cidr, err)
	}
	return nil
}

// ValidateCIDRs checks if all CIDR blocks in a slice are valid
func ValidateCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		if err := ValidateCIDR(cidr); err != nil {
			return err
		}
	}
	return nil
}

// ValidateFirewallRules validates all firewall rules
func ValidateFirewallRules(rules []config.InfraFirewallRule) []string {
	var errors []string
	for i, rule := range rules {
		if err := ValidateProtocol(rule.Protocol); err != nil {
			errors = append(errors, fmt.Sprintf("rule %d: %v", i+1, err))
		}
		if err := ValidatePorts(rule.Ports); err != nil {
			errors = append(errors, fmt.Sprintf("rule %d: %v", i+1, err))
		}
		if err := ValidateCIDRs(rule.Sources); err != nil {
			errors = append(errors, fmt.Sprintf("rule %d: %v", i+1, err))
		}
	}
	return errors
}

// ParseIntFromString safely parses an integer from a string, returning an error if invalid
func ParseIntFromString(s string) (int, error) {
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("failed to parse integer from '%s': %w", s, err)
	}
	return i, nil
}

// ValidateServerCount checks if server count is within allowed range
func ValidateServerCount(count int, provider string) error {
	if count < 1 {
		return fmt.Errorf("server count must be at least 1")
	}
	caps, ok := ProviderCaps[provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}
	if count > caps.MaxServerCount {
		return fmt.Errorf("server count %d exceeds maximum %d for provider %s", count, caps.MaxServerCount, provider)
	}
	return nil
}

// ProviderCapabilities defines what features each provider supports
type ProviderCapabilities struct {
	Name            string
	HasVPC          bool
	HasFirewall     bool
	HasPrivateIP    bool
	HasObjectStore  bool
	HasBlockStorage bool
	HasCDN          bool
	HasLoadBalancer bool
	MinServerCount  int
	MaxServerCount  int
}

// FallbackServerTypes maps providers to their common server types
// Used as a fallback when dynamic API validation is not available
// For dynamic validation, use DynamicValidateServerType() with a token
var FallbackServerTypes = map[string][]string{
	"hetzner": {
		// ARM (Ampere) - Shared
		"cax11", "cax21", "cax31", "cax41",
		// x86 Shared - New naming (CX23, CX33, etc.)
		"cx23", "cx33", "cx43", "cx53",
		// x86 Dedicated (CPX) - AMD
		"cpx11", "cpx21", "cpx31", "cpx41", "cpx51",
		// x86 Dedicated (CCX) - AMD
		"ccx13", "ccx23", "ccx33", "ccx43", "ccx53", "ccx63",
	},
	"digitalocean": {
		// Basic Droplets
		"s-1vcpu-512mb-10gb", "s-1vcpu-1gb", "s-1vcpu-2gb", "s-2vcpu-2gb", "s-2vcpu-4gb",
		"s-4vcpu-8gb", "s-6vcpu-16gb", "s-8vcpu-32gb",
		// General Purpose
		"g-2vcpu-8gb", "g-4vcpu-16gb", "g-8vcpu-32gb",
		// CPU-Optimized
		"c-2", "c-4", "c-8", "c-16",
	},
	"aws": {
		// T3 (Burstable)
		"t3.nano", "t3.micro", "t3.small", "t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge",
		// T3a (AMD Burstable)
		"t3a.nano", "t3a.micro", "t3a.small", "t3a.medium", "t3a.large",
		// M5 (General Purpose)
		"m5.large", "m5.xlarge", "m5.2xlarge", "m5.4xlarge",
		// C5 (Compute)
		"c5.large", "c5.xlarge", "c5.2xlarge",
	},
	"linode": {
		// Shared CPU
		"g6-nanode-1", "g6-standard-1", "g6-standard-2", "g6-standard-4", "g6-standard-6",
		"g6-standard-8", "g6-standard-16",
		// Dedicated CPU
		"g6-dedicated-2", "g6-dedicated-4", "g6-dedicated-8",
	},
}

// ValidServerTypes is an alias for FallbackServerTypes for backwards compatibility
var ValidServerTypes = FallbackServerTypes

// ValidateServerType checks if a server type is valid for a provider
func ValidateServerType(provider, serverType string) error {
	validTypes, ok := ValidServerTypes[provider]
	if !ok {
		return nil // Unknown provider, skip validation
	}

	// Check exact match
	for _, valid := range validTypes {
		if serverType == valid {
			return nil
		}
	}

	// Return error with suggestions
	return fmt.Errorf("server type '%s' may not be valid for %s. Valid types include: %s (and more)",
		serverType, provider, strings.Join(validTypes[:min(5, len(validTypes))], ", "))
}

// GetValidServerTypes returns valid server types for a provider
func GetValidServerTypes(provider string) []string {
	if types, ok := ValidServerTypes[provider]; ok {
		return types
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ProviderCaps maps provider names to their capabilities
var ProviderCaps = map[string]ProviderCapabilities{
	"digitalocean": {
		Name:            "DigitalOcean",
		HasVPC:          true,
		HasFirewall:     true,
		HasPrivateIP:    true,
		HasObjectStore:  true,  // Spaces
		HasBlockStorage: true,  // Volumes
		HasCDN:          true,  // Spaces CDN
		HasLoadBalancer: true,
		MinServerCount:  1,
		MaxServerCount:  500,
	},
	"hetzner": {
		Name:            "Hetzner",
		HasVPC:          true,  // Networks
		HasFirewall:     true,
		HasPrivateIP:    true,
		HasObjectStore:  true,  // Object Storage
		HasBlockStorage: true,  // Volumes
		HasCDN:          false, // No native CDN
		HasLoadBalancer: true,
		MinServerCount:  1,
		MaxServerCount:  100,
	},
	"aws": {
		Name:            "AWS",
		HasVPC:          true,
		HasFirewall:     true,  // Security Groups
		HasPrivateIP:    true,
		HasObjectStore:  true,  // S3
		HasBlockStorage: true,  // EBS
		HasCDN:          true,  // CloudFront
		HasLoadBalancer: true,  // ALB/ELB
		MinServerCount:  1,
		MaxServerCount:  1000,
	},
	"linode": {
		Name:            "Linode",
		HasVPC:          true,  // VLANs
		HasFirewall:     true,
		HasPrivateIP:    true,
		HasObjectStore:  true,  // Object Storage
		HasBlockStorage: true,  // Volumes
		HasCDN:          false, // No native CDN
		HasLoadBalancer: true,  // NodeBalancers
		MinServerCount:  1,
		MaxServerCount:  250,
	},
}

// ValidateConfig validates infrastructure config for a provider
func ValidateConfig(infra *config.InfrastructureConfig) []string {
	var errors []string

	if infra == nil {
		return []string{"infrastructure config is nil"}
	}

	// Check default provider is valid
	caps, ok := ProviderCaps[infra.Provider]
	if !ok {
		errors = append(errors, fmt.Sprintf("unknown provider '%s', supported: digitalocean, hetzner, aws, linode", infra.Provider))
		return errors
	}

	// Check region is specified
	if infra.Region == "" {
		errors = append(errors, "region is required")
	}

	// Check servers
	if len(infra.Servers) == 0 {
		errors = append(errors, "at least one server must be defined")
	}

	// Track providers and servers per provider
	providerServerCounts := make(map[string]int)
	totalServers := 0
	hasManager := false

	for name, spec := range infra.Servers {
		// Determine effective provider for this server
		provider := spec.Provider
		if provider == "" {
			provider = infra.Provider
		}

		// Validate server's provider
		if spec.Provider != "" {
			if _, ok := ProviderCaps[spec.Provider]; !ok {
				errors = append(errors, fmt.Sprintf("server '%s': unknown provider '%s'", name, spec.Provider))
				continue
			}
		}

		// Validate server's region if overridden
		if spec.Region == "" && spec.Provider != "" {
			errors = append(errors, fmt.Sprintf("server '%s': region required when provider is overridden", name))
		}

		count := spec.Count
		if count < 1 {
			count = 1
		}
		totalServers += count
		providerServerCounts[provider] += count

		if spec.Role == "manager" {
			hasManager = true
		}

		// Validate size for the effective provider
		if spec.Size != "" && !isValidSize(provider, spec.Size) {
			errors = append(errors, fmt.Sprintf("server '%s': unknown size '%s' for provider '%s'", name, spec.Size, provider))
		}
	}

	// Check per-provider limits
	for provider, count := range providerServerCounts {
		providerCaps := ProviderCaps[provider]
		if count > providerCaps.MaxServerCount {
			errors = append(errors, fmt.Sprintf("server count for %s (%d) exceeds limit (%d)", provider, count, providerCaps.MaxServerCount))
		}
	}

	// Warn if no manager for multi-server
	if totalServers > 1 && !hasManager {
		errors = append(errors, "multi-server setup requires at least one server with role: manager")
	}

	// Validate networking config (applies to default provider)
	if infra.Networking != nil {
		if infra.Networking.VPC != nil && infra.Networking.VPC.Enabled {
			if !caps.HasVPC {
				errors = append(errors, fmt.Sprintf("provider '%s' does not support VPC", infra.Provider))
			}
			// Validate VPC IP range
			if infra.Networking.VPC.IPRange != "" {
				if err := ValidateCIDR(infra.Networking.VPC.IPRange); err != nil {
					errors = append(errors, fmt.Sprintf("vpc: %v", err))
				}
			}
		}
		if infra.Networking.Firewall != nil && infra.Networking.Firewall.Enabled {
			if !caps.HasFirewall {
				errors = append(errors, fmt.Sprintf("provider '%s' does not support firewalls", infra.Provider))
			}
			// Validate firewall rules
			ruleErrors := ValidateFirewallRules(infra.Networking.Firewall.Rules)
			errors = append(errors, ruleErrors...)
		}
	}

	// Validate storage config
	if infra.Storage != nil && len(infra.Storage.Buckets) > 0 && !caps.HasObjectStore {
		errors = append(errors, fmt.Sprintf("provider '%s' does not support object storage", infra.Provider))
	}

	// Validate CDN config
	if infra.CDN != nil && infra.CDN.Enabled && !caps.HasCDN {
		errors = append(errors, fmt.Sprintf("provider '%s' does not support CDN (consider using Cloudflare)", infra.Provider))
	}

	return errors
}

// GetRequiredCredentials returns the env vars needed for the infrastructure config
func GetRequiredCredentials(infra *config.InfrastructureConfig) map[string]string {
	providers := GetUsedProviders(infra)
	creds := make(map[string]string)

	for _, provider := range providers {
		switch provider {
		case "digitalocean":
			creds["DIGITALOCEAN_TOKEN"] = "DigitalOcean API token"
		case "hetzner":
			creds["HCLOUD_TOKEN"] = "Hetzner Cloud API token"
		case "linode":
			creds["LINODE_TOKEN"] = "Linode API token"
		case "aws":
			creds["AWS_ACCESS_KEY_ID"] = "AWS access key ID"
			creds["AWS_SECRET_ACCESS_KEY"] = "AWS secret access key"
		}
	}

	return creds
}

// isValidSize checks if a size is valid for a provider
func isValidSize(provider, size string) bool {
	// Generic sizes are always valid
	genericSizes := []string{"small", "medium", "large", "xlarge"}
	for _, gs := range genericSizes {
		if size == gs {
			return true
		}
	}

	// Check provider-specific sizes
	if sizes, ok := ProviderSizes[provider]; ok {
		for _, providerSize := range sizes {
			if providerSize == size {
				return true
			}
		}
	}

	// Allow any size (provider will validate)
	return true
}

// PortableConfig represents a provider-agnostic infrastructure config
type PortableConfig struct {
	Region  string                          `yaml:"region"`  // Friendly name: us-east, europe, asia
	Servers map[string]PortableServerSpec   `yaml:"servers"`
}

// PortableServerSpec is a provider-agnostic server spec
type PortableServerSpec struct {
	Size  string `yaml:"size"`  // small, medium, large, xlarge
	Role  string `yaml:"role"`  // manager, worker
	Count int    `yaml:"count"`
}

// ConvertToProvider converts a portable config to provider-specific
func ConvertToProvider(portable *PortableConfig, provider string) (*config.InfrastructureConfig, error) {
	if _, ok := ProviderCaps[provider]; !ok {
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	infra := &config.InfrastructureConfig{
		Provider: provider,
		Region:   ResolveRegion(provider, portable.Region),
		Servers:  make(map[string]config.InfraServerSpec),
	}

	for name, spec := range portable.Servers {
		infra.Servers[name] = config.InfraServerSpec{
			Size:  ResolveSize(provider, spec.Size),
			Role:  spec.Role,
			Count: spec.Count,
		}
	}

	return infra, nil
}

// GetEquivalentSize finds equivalent size across providers
func GetEquivalentSize(fromProvider, toProvider, size string) string {
	// If it's a generic size, resolve for target provider
	genericSizes := []string{"small", "medium", "large", "xlarge"}
	for _, gs := range genericSizes {
		if size == gs {
			return ResolveSize(toProvider, gs)
		}
	}

	// Try to find equivalent by matching specs
	fromSizes, okFrom := ProviderSizes[fromProvider]
	toSizes, okTo := ProviderSizes[toProvider]

	if !okFrom || !okTo {
		return size
	}

	// Find which generic size this is
	for genericSize, providerSize := range fromSizes {
		if providerSize == size {
			if toSize, ok := toSizes[genericSize]; ok {
				return toSize
			}
		}
	}

	return size
}

// MigrateConfig converts config from one provider to another
func MigrateConfig(infra *config.InfrastructureConfig, toProvider string) (*config.InfrastructureConfig, []string) {
	var warnings []string

	newInfra := &config.InfrastructureConfig{
		Provider:    toProvider,
		Region:      mapRegion(infra.Provider, toProvider, infra.Region),
		SSHKey:      infra.SSHKey,
		SSHUser:     infra.SSHUser,
		Servers:     make(map[string]config.InfraServerSpec),
	}

	// Convert servers
	for name, spec := range infra.Servers {
		newSpec := config.InfraServerSpec{
			Size:  GetEquivalentSize(infra.Provider, toProvider, spec.Size),
			Role:  spec.Role,
			Count: spec.Count,
			Tags:  spec.Tags,
		}
		newInfra.Servers[name] = newSpec
	}

	// Check capabilities
	fromCaps := ProviderCaps[infra.Provider]
	toCaps := ProviderCaps[toProvider]

	if infra.CDN != nil && infra.CDN.Enabled && fromCaps.HasCDN && !toCaps.HasCDN {
		warnings = append(warnings, fmt.Sprintf("%s does not support native CDN - consider using Cloudflare", toProvider))
	}

	if infra.Storage != nil && len(infra.Storage.Buckets) > 0 && !toCaps.HasObjectStore {
		warnings = append(warnings, fmt.Sprintf("%s does not support object storage", toProvider))
	}

	return newInfra, warnings
}

// mapRegion tries to find equivalent region across providers
func mapRegion(fromProvider, toProvider, region string) string {
	// Define region equivalents
	regionMap := map[string]map[string]string{
		// US East
		"nyc1":       {"digitalocean": "nyc1", "hetzner": "ash", "aws": "us-east-1", "linode": "us-east"},
		"nyc3":       {"digitalocean": "nyc3", "hetzner": "ash", "aws": "us-east-1", "linode": "us-east"},
		"us-east-1":  {"digitalocean": "nyc1", "hetzner": "ash", "aws": "us-east-1", "linode": "us-east"},
		"us-east":    {"digitalocean": "nyc1", "hetzner": "ash", "aws": "us-east-1", "linode": "us-east"},
		"ash":        {"digitalocean": "nyc1", "hetzner": "ash", "aws": "us-east-1", "linode": "us-east"},
		// US West
		"sfo3":       {"digitalocean": "sfo3", "hetzner": "hil", "aws": "us-west-2", "linode": "us-west"},
		"us-west-2":  {"digitalocean": "sfo3", "hetzner": "hil", "aws": "us-west-2", "linode": "us-west"},
		"us-west":    {"digitalocean": "sfo3", "hetzner": "hil", "aws": "us-west-2", "linode": "us-west"},
		"hil":        {"digitalocean": "sfo3", "hetzner": "hil", "aws": "us-west-2", "linode": "us-west"},
		// Europe
		"fra1":       {"digitalocean": "fra1", "hetzner": "fsn1", "aws": "eu-central-1", "linode": "eu-central"},
		"fsn1":       {"digitalocean": "fra1", "hetzner": "fsn1", "aws": "eu-central-1", "linode": "eu-central"},
		"nbg1":       {"digitalocean": "fra1", "hetzner": "nbg1", "aws": "eu-central-1", "linode": "eu-central"},
		"eu-central": {"digitalocean": "fra1", "hetzner": "fsn1", "aws": "eu-central-1", "linode": "eu-central"},
		"ams3":       {"digitalocean": "ams3", "hetzner": "fsn1", "aws": "eu-west-1", "linode": "eu-west"},
		"lon1":       {"digitalocean": "lon1", "hetzner": "fsn1", "aws": "eu-west-2", "linode": "eu-west"},
		// Asia
		"sgp1":       {"digitalocean": "sgp1", "hetzner": "hel1", "aws": "ap-southeast-1", "linode": "ap-south"},
		"ap-south":   {"digitalocean": "sgp1", "hetzner": "hel1", "aws": "ap-south-1", "linode": "ap-south"},
	}

	// Find the region group
	for _, providers := range regionMap {
		if providers[fromProvider] == region {
			if mapped, ok := providers[toProvider]; ok {
				return mapped
			}
		}
	}

	// Return original if no mapping found
	return region
}

// PrintProviderComparison shows a comparison between two providers
func PrintProviderComparison(provider1, provider2 string) {
	caps1, ok1 := ProviderCaps[provider1]
	caps2, ok2 := ProviderCaps[provider2]

	if !ok1 || !ok2 {
		fmt.Println("Invalid provider(s)")
		return
	}

	fmt.Printf("\n%s vs %s Comparison\n", caps1.Name, caps2.Name)
	fmt.Println(strings.Repeat("─", 40))

	features := []struct {
		name string
		has1 bool
		has2 bool
	}{
		{"VPC/Private Network", caps1.HasVPC, caps2.HasVPC},
		{"Firewall", caps1.HasFirewall, caps2.HasFirewall},
		{"Object Storage", caps1.HasObjectStore, caps2.HasObjectStore},
		{"Block Storage", caps1.HasBlockStorage, caps2.HasBlockStorage},
		{"CDN", caps1.HasCDN, caps2.HasCDN},
		{"Load Balancer", caps1.HasLoadBalancer, caps2.HasLoadBalancer},
	}

	fmt.Printf("%-20s %-12s %-12s\n", "Feature", caps1.Name, caps2.Name)
	fmt.Println(strings.Repeat("─", 44))

	for _, f := range features {
		v1 := "✗"
		v2 := "✗"
		if f.has1 {
			v1 = "✓"
		}
		if f.has2 {
			v2 = "✓"
		}
		fmt.Printf("%-20s %-12s %-12s\n", f.name, v1, v2)
	}

	// Pricing comparison
	fmt.Println("\nPricing (medium size):")
	p1 := GetServerPricing(provider1, "medium")
	p2 := GetServerPricing(provider2, "medium")
	fmt.Printf("  %s: %s\n", caps1.Name, FormatPricing(p1.Monthly))
	fmt.Printf("  %s: %s\n", caps2.Name, FormatPricing(p2.Monthly))
}
