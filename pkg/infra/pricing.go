package infra

import "fmt"

// Pricing contains monthly cost estimates in USD
type Pricing struct {
	Hourly  float64
	Monthly float64
}

// ServerPricing maps provider -> size -> pricing
var ServerPricing = map[string]map[string]Pricing{
	"digitalocean": {
		"small":  {Hourly: 0.009, Monthly: 6},
		"medium": {Hourly: 0.036, Monthly: 24},
		"large":  {Hourly: 0.071, Monthly: 48},
		"xlarge": {Hourly: 0.143, Monthly: 96},
		// Provider-specific sizes
		"s-1vcpu-1gb":  {Hourly: 0.009, Monthly: 6},
		"s-2vcpu-4gb":  {Hourly: 0.036, Monthly: 24},
		"s-4vcpu-8gb":  {Hourly: 0.071, Monthly: 48},
		"s-8vcpu-16gb": {Hourly: 0.143, Monthly: 96},
	},
	"hetzner": {
		"small":  {Hourly: 0.005, Monthly: 3.29},
		"medium": {Hourly: 0.008, Monthly: 5.39},
		"large":  {Hourly: 0.014, Monthly: 9.59},
		"xlarge": {Hourly: 0.027, Monthly: 17.99},
		// Provider-specific sizes
		"cx11": {Hourly: 0.005, Monthly: 3.29},
		"cx21": {Hourly: 0.008, Monthly: 5.39},
		"cx31": {Hourly: 0.014, Monthly: 9.59},
		"cx41": {Hourly: 0.027, Monthly: 17.99},
	},
	"aws": {
		"small":  {Hourly: 0.0104, Monthly: 7.59},
		"medium": {Hourly: 0.0416, Monthly: 30.37},
		"large":  {Hourly: 0.0832, Monthly: 60.74},
		"xlarge": {Hourly: 0.1664, Monthly: 121.47},
		// Provider-specific sizes
		"t3.micro":  {Hourly: 0.0104, Monthly: 7.59},
		"t3.medium": {Hourly: 0.0416, Monthly: 30.37},
		"t3.large":  {Hourly: 0.0832, Monthly: 60.74},
		"t3.xlarge": {Hourly: 0.1664, Monthly: 121.47},
	},
	"linode": {
		"small":  {Hourly: 0.0075, Monthly: 5},
		"medium": {Hourly: 0.018, Monthly: 12},
		"large":  {Hourly: 0.036, Monthly: 24},
		"xlarge": {Hourly: 0.072, Monthly: 48},
		// Provider-specific sizes
		"g6-nanode-1":   {Hourly: 0.0075, Monthly: 5},
		"g6-standard-2": {Hourly: 0.018, Monthly: 12},
		"g6-standard-4": {Hourly: 0.036, Monthly: 24},
		"g6-standard-8": {Hourly: 0.072, Monthly: 48},
	},
}

// StoragePricing maps provider -> storage type -> price per GB/month
var StoragePricing = map[string]map[string]float64{
	"digitalocean": {
		"spaces":  0.02, // $0.02/GB/month, $5 min
		"volumes": 0.10, // $0.10/GB/month
	},
	"hetzner": {
		"object":  0.0052, // €0.0052/GB/month (no min)
		"volumes": 0.052,  // €0.052/GB/month
	},
	"aws": {
		"s3":  0.023, // $0.023/GB/month (S3 Standard)
		"ebs": 0.08,  // $0.08/GB/month (gp3)
	},
	"linode": {
		"object":  0.02, // $0.02/GB/month
		"volumes": 0.10, // $0.10/GB/month
	},
}

// CDNPricing maps provider -> price per GB transferred
var CDNPricing = map[string]float64{
	"digitalocean": 0.01,  // $0.01/GB (included in Spaces)
	"hetzner":      0.00,  // Free egress (in EU)
	"aws":          0.085, // $0.085/GB (CloudFront, first 10TB)
	"linode":       0.005, // $0.005/GB
}

// GetServerPricing returns pricing for a server size
func GetServerPricing(provider, size string) Pricing {
	if providerPricing, ok := ServerPricing[provider]; ok {
		if pricing, ok := providerPricing[size]; ok {
			return pricing
		}
	}
	return Pricing{}
}

// EstimateMonthlyInfraCost calculates estimated monthly cost for servers
func EstimateMonthlyInfraCost(provider string, servers map[string]InfraServerSpec) float64 {
	total := 0.0
	for _, spec := range servers {
		size := spec.Size
		if size == "" {
			size = "medium"
		}
		count := spec.Count
		if count < 1 {
			count = 1
		}
		pricing := GetServerPricing(provider, size)
		total += pricing.Monthly * float64(count)
	}
	return total
}

// InfraServerSpec mirrors config.InfraServerSpec for pricing
type InfraServerSpec struct {
	Size  string
	Count int
}

// FormatPricing formats pricing for display
func FormatPricing(monthly float64) string {
	if monthly < 10 {
		return fmt.Sprintf("$%.2f/mo", monthly)
	}
	return fmt.Sprintf("$%.0f/mo", monthly)
}

// GetProviderComparison returns pricing comparison across providers
func GetProviderComparison(size string, count int) map[string]float64 {
	comparison := make(map[string]float64)
	for provider := range ServerPricing {
		pricing := GetServerPricing(provider, size)
		comparison[provider] = pricing.Monthly * float64(count)
	}
	return comparison
}
