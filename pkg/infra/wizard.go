package infra

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// Wizard handles interactive infrastructure configuration
type Wizard struct {
	reader *bufio.Reader
}

// NewWizard creates a new wizard instance
func NewWizard() *Wizard {
	return &Wizard{
		reader: bufio.NewReader(os.Stdin),
	}
}

// WizardResult contains the generated infrastructure configuration
type WizardResult struct {
	Config         *config.InfrastructureConfig
	EstimatedCost  float64
	YAMLContent    string
	ProviderEnvVar string
}

// Run executes the interactive wizard
func (w *Wizard) Run() (*WizardResult, error) {
	fmt.Println("\n🐙 Tako Infrastructure Wizard")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Configure your cloud servers in 3 simple steps.")
	fmt.Println("Security, networking, and firewall are auto-configured.")
	fmt.Println()

	// Step 1: Select provider
	provider, err := w.selectProvider()
	if err != nil {
		return nil, err
	}

	// Step 2: Select region
	region, err := w.selectRegion(provider)
	if err != nil {
		return nil, err
	}

	// Step 3: Configure servers
	servers, err := w.configureServers(provider)
	if err != nil {
		return nil, err
	}

	// Auto-configure networking with smart defaults
	networking := w.autoConfigureNetworking(servers)

	// Calculate pricing
	infraServers := make(map[string]InfraServerSpec)
	for name, spec := range servers {
		infraServers[name] = InfraServerSpec{
			Size:  spec.Size,
			Count: spec.Count,
		}
	}
	estimatedCost := EstimateMonthlyInfraCost(provider, infraServers)

	// Build config
	infraConfig := &config.InfrastructureConfig{
		Provider:   provider,
		Region:     region,
		Servers:    servers,
		Networking: networking,
	}

	// Generate YAML
	yaml := w.generateYAML(infraConfig, estimatedCost)

	// Get environment variable name for provider
	envVar := ProviderEnvVars[provider]
	if provider == "aws" {
		envVar = "AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY"
	}

	return &WizardResult{
		Config:         infraConfig,
		EstimatedCost:  estimatedCost,
		YAMLContent:    yaml,
		ProviderEnvVar: envVar,
	}, nil
}

// autoConfigureNetworking sets up VPC and firewall with smart defaults
func (w *Wizard) autoConfigureNetworking(servers map[string]config.InfraServerSpec) *config.InfraNetworkingConfig {
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

func (w *Wizard) selectProvider() (string, error) {
	fmt.Println("Step 1: Select Cloud Provider")
	fmt.Println("─────────────────────────────")
	fmt.Println()

	providers := []struct {
		name        string
		description string
		minPrice    string
	}{
		{"digitalocean", "DigitalOcean - Simple cloud for developers", "$6/mo"},
		{"hetzner", "Hetzner - Best value European provider", "$3.29/mo"},
		{"aws", "AWS - Enterprise-grade cloud", "$7.59/mo"},
		{"linode", "Linode (Akamai) - Developer-friendly", "$5/mo"},
	}

	for i, p := range providers {
		fmt.Printf("  %d. %s (from %s)\n", i+1, p.description, p.minPrice)
	}
	fmt.Println()

	choice, err := w.promptNumber("Select provider (1-4)", 1, 4)
	if err != nil {
		return "", err
	}

	provider := providers[choice-1].name
	fmt.Printf("  ✓ Selected: %s\n\n", provider)
	return provider, nil
}

func (w *Wizard) selectRegion(provider string) (string, error) {
	fmt.Println("Step 2: Select Region")
	fmt.Println("─────────────────────")

	// Try to fetch real regions from API
	api := NewProviderAPI()
	apiRegions, err := api.FetchRegions(provider)

	if err == nil && len(apiRegions) > 0 {
		fmt.Println("  (fetched from API)")
		fmt.Println()

		for i, r := range apiRegions {
			fmt.Printf("  %d. %s - %s\n", i+1, r.Slug, r.Description)
		}
		fmt.Println()

		choice, err := w.promptNumber("Select region", 1, len(apiRegions))
		if err != nil {
			return "", err
		}

		region := apiRegions[choice-1].Slug
		fmt.Printf("  ✓ Selected: %s\n\n", region)
		return region, nil
	}

	// Fall back to embedded data
	fmt.Println("  (using cached data)")
	fmt.Println()

	regions := GetProviderRegions(provider)
	regionDescriptions := map[string]map[string]string{
		"digitalocean": {
			"nyc1": "New York (US East)",
			"nyc3": "New York (US East)",
			"sfo3": "San Francisco (US West)",
			"ams3": "Amsterdam (Europe)",
			"sgp1": "Singapore (Asia)",
			"lon1": "London (Europe)",
			"fra1": "Frankfurt (Europe)",
			"tor1": "Toronto (Canada)",
			"blr1": "Bangalore (India)",
			"syd1": "Sydney (Australia)",
		},
		"hetzner": {
			"nbg1": "Nuremberg (Germany)",
			"fsn1": "Falkenstein (Germany)",
			"hel1": "Helsinki (Finland)",
			"ash":  "Ashburn (US East)",
			"hil":  "Hillsboro (US West)",
		},
		"aws": {
			"us-east-1":  "US East (N. Virginia)",
			"us-west-2":  "US West (Oregon)",
			"eu-west-1":  "EU (Ireland)",
			"ap-south-1": "Asia Pacific (Mumbai)",
		},
		"linode": {
			"us-east":    "Newark (US East)",
			"us-west":    "Fremont (US West)",
			"eu-central": "Frankfurt (Europe)",
			"ap-south":   "Singapore (Asia)",
		},
	}

	for i, region := range regions {
		desc := region
		if descriptions, ok := regionDescriptions[provider]; ok {
			if d, ok := descriptions[region]; ok {
				desc = d
			}
		}
		fmt.Printf("  %d. %s - %s\n", i+1, region, desc)
	}
	fmt.Println()

	choice, err := w.promptNumber("Select region", 1, len(regions))
	if err != nil {
		return "", err
	}

	region := regions[choice-1]
	fmt.Printf("  ✓ Selected: %s\n\n", region)
	return region, nil
}

func (w *Wizard) configureServers(provider string) (map[string]config.InfraServerSpec, error) {
	fmt.Println("Step 3: Configure Servers")
	fmt.Println("─────────────────────────")

	servers := make(map[string]config.InfraServerSpec)

	// Try to fetch real sizes from API
	api := NewProviderAPI()
	apiSizes, err := api.FetchSizes(provider, "")

	type sizeOption struct {
		slug    string
		desc    string
		specs   string
		monthly float64
	}

	var sizes []sizeOption

	if err == nil && len(apiSizes) > 0 {
		fmt.Println("  (fetched from API)")
		fmt.Println()

		// Show first 8 sizes (usually enough options)
		maxSizes := 8
		if len(apiSizes) < maxSizes {
			maxSizes = len(apiSizes)
		}

		for i := 0; i < maxSizes; i++ {
			s := apiSizes[i]
			sizes = append(sizes, sizeOption{
				slug:    s.Slug,
				desc:    s.Description,
				specs:   fmt.Sprintf("%d vCPU, %s", s.VCPUs, FormatMemory(s.Memory)),
				monthly: s.PriceMonthly,
			})
		}
	} else {
		fmt.Println("  (using cached data)")
		fmt.Println()

		// Fall back to generic sizes
		genericSizes := []struct {
			name  string
			desc  string
			specs string
		}{
			{"small", "Small", "1 vCPU, 1GB RAM"},
			{"medium", "Medium", "2 vCPU, 4GB RAM"},
			{"large", "Large", "4 vCPU, 8GB RAM"},
			{"xlarge", "X-Large", "8 vCPU, 16GB RAM"},
		}

		for _, s := range genericSizes {
			pricing := GetServerPricing(provider, s.name)
			sizes = append(sizes, sizeOption{
				slug:    s.name,
				desc:    s.desc,
				specs:   s.specs,
				monthly: pricing.Monthly,
			})
		}
	}

	fmt.Println("Available sizes:")
	for i, s := range sizes {
		fmt.Printf("  %d. %s (%s) - %s\n", i+1, s.desc, s.specs, FormatPricing(s.monthly))
	}
	fmt.Println()

	// Configure manager node
	fmt.Println("Primary server (manager node):")
	managerSize, err := w.promptNumber(fmt.Sprintf("  Size (1-%d)", len(sizes)), 1, len(sizes))
	if err != nil {
		return nil, err
	}

	selectedSize := sizes[managerSize-1]
	servers["manager"] = config.InfraServerSpec{
		Size:  selectedSize.slug,
		Role:  "manager",
		Count: 1,
	}
	fmt.Printf("  ✓ Manager: %s (%s)\n\n", selectedSize.desc, FormatPricing(selectedSize.monthly))

	// Optional worker nodes
	fmt.Println("Worker nodes (optional, for horizontal scaling):")
	addWorkers, err := w.promptYesNo("  Add worker nodes?", false)
	if err != nil {
		return nil, err
	}

	if addWorkers {
		workerSize, err := w.promptNumber(fmt.Sprintf("  Worker size (1-%d)", len(sizes)), 1, len(sizes))
		if err != nil {
			return nil, err
		}

		workerCount, err := w.promptNumber("  Number of workers (1-10)", 1, 10)
		if err != nil {
			return nil, err
		}

		workerSizeSpec := sizes[workerSize-1]
		servers["workers"] = config.InfraServerSpec{
			Size:  workerSizeSpec.slug,
			Role:  "worker",
			Count: workerCount,
		}
		totalWorkerCost := workerSizeSpec.monthly * float64(workerCount)
		fmt.Printf("  ✓ Workers: %d x %s (%s)\n\n", workerCount, workerSizeSpec.desc, FormatPricing(totalWorkerCost))
	}

	return servers, nil
}

func (w *Wizard) generateYAML(infra *config.InfrastructureConfig, cost float64) string {
	var sb strings.Builder

	sb.WriteString("# Infrastructure provisioned by Tako CLI\n")
	sb.WriteString(fmt.Sprintf("# Estimated monthly cost: %s\n", FormatPricing(cost)))
	sb.WriteString("# Security (firewall, VPC) is auto-configured with sensible defaults\n")
	sb.WriteString("\n")
	sb.WriteString("infrastructure:\n")
	sb.WriteString(fmt.Sprintf("  provider: %s\n", infra.Provider))
	sb.WriteString(fmt.Sprintf("  region: %s\n", infra.Region))
	sb.WriteString("  # Credentials loaded from environment variables automatically\n")
	sb.WriteString("\n")

	// Servers
	sb.WriteString("  servers:\n")
	for name, spec := range infra.Servers {
		sb.WriteString(fmt.Sprintf("    %s:\n", name))
		sb.WriteString(fmt.Sprintf("      size: %s\n", spec.Size))
		sb.WriteString(fmt.Sprintf("      role: %s\n", spec.Role))
		if spec.Count > 1 {
			sb.WriteString(fmt.Sprintf("      count: %d\n", spec.Count))
		}
	}
	sb.WriteString("\n")

	// Only include networking section if user needs to customize
	sb.WriteString("  # Networking is auto-configured. Uncomment to customize:\n")
	sb.WriteString("  # networking:\n")
	sb.WriteString("  #   firewall:\n")
	sb.WriteString("  #     enabled: true\n")
	sb.WriteString("  #     rules:\n")
	sb.WriteString("  #       - protocol: tcp\n")
	sb.WriteString("  #         ports: [22, 80, 443]\n")
	sb.WriteString("  #         sources: [\"0.0.0.0/0\"]\n")

	return sb.String()
}

func (w *Wizard) promptNumber(prompt string, min, max int) (int, error) {
	for {
		fmt.Printf("%s: ", prompt)
		input, err := w.reader.ReadString('\n')
		if err != nil {
			return 0, err
		}

		input = strings.TrimSpace(input)
		num, err := strconv.Atoi(input)
		if err != nil || num < min || num > max {
			fmt.Printf("  Please enter a number between %d and %d\n", min, max)
			continue
		}

		return num, nil
	}
}

func (w *Wizard) promptYesNo(prompt string, defaultYes bool) (bool, error) {
	defaultStr := "Y/n"
	if !defaultYes {
		defaultStr = "y/N"
	}

	fmt.Printf("%s [%s]: ", prompt, defaultStr)
	input, err := w.reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return defaultYes, nil
	}

	return input == "y" || input == "yes", nil
}

// ShowPricingComparison displays pricing across all providers
func ShowPricingComparison() {
	fmt.Println("\n📊 Provider Pricing Comparison (Monthly)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	sizes := []string{"small", "medium", "large", "xlarge"}
	providers := []string{"hetzner", "linode", "digitalocean", "aws"}

	// Header
	fmt.Printf("%-10s", "Size")
	for _, p := range providers {
		fmt.Printf("%-14s", p)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 66))

	// Rows
	for _, size := range sizes {
		fmt.Printf("%-10s", size)
		for _, provider := range providers {
			pricing := GetServerPricing(provider, size)
			fmt.Printf("%-14s", FormatPricing(pricing.Monthly))
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("💡 Hetzner offers the best value, AWS offers enterprise features")
	fmt.Println()
}

// ShowStoragePricing displays storage pricing
func ShowStoragePricing() {
	fmt.Println("\n📦 Storage Pricing (per GB/month)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	providers := []string{"hetzner", "digitalocean", "linode", "aws"}

	fmt.Printf("%-14s%-12s%-12s\n", "Provider", "Object", "Block")
	fmt.Println(strings.Repeat("─", 38))

	storageTypes := map[string][]string{
		"hetzner":      {"object", "volumes"},
		"digitalocean": {"spaces", "volumes"},
		"linode":       {"object", "volumes"},
		"aws":          {"s3", "ebs"},
	}

	for _, p := range providers {
		types := storageTypes[p]
		objectPrice := StoragePricing[p][types[0]]
		blockPrice := StoragePricing[p][types[1]]
		fmt.Printf("%-14s$%-11.3f$%-11.2f\n", p, objectPrice, blockPrice)
	}

	fmt.Println()
}
