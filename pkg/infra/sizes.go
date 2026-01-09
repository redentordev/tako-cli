package infra

// ServerSize represents a generic server size that maps to provider-specific sizes
type ServerSize string

const (
	SizeSmall  ServerSize = "small"  // 1 vCPU, 1GB RAM
	SizeMedium ServerSize = "medium" // 2 vCPU, 4GB RAM
	SizeLarge  ServerSize = "large"  // 4 vCPU, 8GB RAM
	SizeXLarge ServerSize = "xlarge" // 8 vCPU, 16GB RAM
)

// ProviderSizes maps generic sizes to provider-specific instance types
var ProviderSizes = map[string]map[ServerSize]string{
	"digitalocean": {
		SizeSmall:  "s-1vcpu-1gb",
		SizeMedium: "s-2vcpu-4gb",
		SizeLarge:  "s-4vcpu-8gb",
		SizeXLarge: "s-8vcpu-16gb",
	},
	"hetzner": {
		SizeSmall:  "cx11",  // 1 vCPU, 2GB (closest)
		SizeMedium: "cx21",  // 2 vCPU, 4GB
		SizeLarge:  "cx31",  // 4 vCPU, 8GB
		SizeXLarge: "cx41",  // 8 vCPU, 16GB
	},
	"aws": {
		SizeSmall:  "t3.micro",
		SizeMedium: "t3.medium",
		SizeLarge:  "t3.large",
		SizeXLarge: "t3.xlarge",
	},
	"linode": {
		SizeSmall:  "g6-nanode-1",
		SizeMedium: "g6-standard-2",
		SizeLarge:  "g6-standard-4",
		SizeXLarge: "g6-standard-8",
	},
}

// ProviderImages maps providers to default Ubuntu 22.04 images
var ProviderImages = map[string]string{
	"digitalocean": "ubuntu-22-04-x64",
	"hetzner":      "ubuntu-22.04",
	"aws":          "ami-0c7217cdde317cfec", // Ubuntu 22.04 us-east-1
	"linode":       "linode/ubuntu22.04",
}

// ProviderRegions maps providers to their available regions with friendly names
var ProviderRegions = map[string]map[string]string{
	"digitalocean": {
		"nyc":       "nyc1",
		"nyc1":      "nyc1",
		"nyc3":      "nyc3",
		"sfo":       "sfo3",
		"sfo3":      "sfo3",
		"ams":       "ams3",
		"ams3":      "ams3",
		"sgp":       "sgp1",
		"sgp1":      "sgp1",
		"lon":       "lon1",
		"lon1":      "lon1",
		"fra":       "fra1",
		"fra1":      "fra1",
		"tor":       "tor1",
		"tor1":      "tor1",
		"blr":       "blr1",
		"blr1":      "blr1",
		"syd":       "syd1",
		"syd1":      "syd1",
	},
	"hetzner": {
		"nuremberg": "nbg1",
		"nbg1":      "nbg1",
		"falkenstein": "fsn1",
		"fsn1":      "fsn1",
		"helsinki":  "hel1",
		"hel1":      "hel1",
		"ashburn":   "ash",
		"ash":       "ash",
		"hillsboro": "hil",
		"hil":       "hil",
	},
	"aws": {
		"us-east":    "us-east-1",
		"us-east-1":  "us-east-1",
		"us-west":    "us-west-2",
		"us-west-2":  "us-west-2",
		"eu-west":    "eu-west-1",
		"eu-west-1":  "eu-west-1",
		"ap-south":   "ap-south-1",
		"ap-south-1": "ap-south-1",
	},
	"linode": {
		"newark":     "us-east",
		"us-east":    "us-east",
		"fremont":    "us-west",
		"us-west":    "us-west",
		"frankfurt":  "eu-central",
		"eu-central": "eu-central",
		"singapore":  "ap-south",
		"ap-south":   "ap-south",
	},
}

// ResolveSize converts a generic or provider-specific size to the actual provider size
func ResolveSize(provider, size string) string {
	// Check if it's a generic size
	if sizes, ok := ProviderSizes[provider]; ok {
		if resolved, ok := sizes[ServerSize(size)]; ok {
			return resolved
		}
	}
	// Return as-is (assume it's already provider-specific)
	return size
}

// ResolveRegion converts a friendly region name to provider-specific region
func ResolveRegion(provider, region string) string {
	if regions, ok := ProviderRegions[provider]; ok {
		if resolved, ok := regions[region]; ok {
			return resolved
		}
	}
	// Return as-is
	return region
}

// GetDefaultImage returns the default image for a provider
func GetDefaultImage(provider string) string {
	if image, ok := ProviderImages[provider]; ok {
		return image
	}
	return "ubuntu-22-04-x64"
}

// ValidProviders returns list of supported providers
func ValidProviders() []string {
	return []string{"digitalocean", "hetzner", "aws", "linode"}
}

// IsValidProvider checks if a provider is supported
func IsValidProvider(provider string) bool {
	for _, p := range ValidProviders() {
		if p == provider {
			return true
		}
	}
	return false
}

// GetProviderRegions returns available regions for a provider
func GetProviderRegions(provider string) []string {
	regions := []string{}
	if r, ok := ProviderRegions[provider]; ok {
		seen := make(map[string]bool)
		for _, v := range r {
			if !seen[v] {
				regions = append(regions, v)
				seen[v] = true
			}
		}
	}
	return regions
}
