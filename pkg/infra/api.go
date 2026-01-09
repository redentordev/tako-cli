package infra

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ProviderAPI handles fetching real data from provider APIs
type ProviderAPI struct {
	client *http.Client
}

// NewProviderAPI creates a new API client
func NewProviderAPI() *ProviderAPI {
	return &ProviderAPI{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// RegionInfo contains region details
type RegionInfo struct {
	Slug        string
	Name        string
	Available   bool
	Description string
}

// SizeInfo contains server size details
type SizeInfo struct {
	Slug        string
	Description string
	VCPUs       int
	Memory      int    // MB
	Disk        int    // GB
	PriceHourly float64
	PriceMonthly float64
	Available   bool
}

// FetchRegions gets available regions from provider API
func (api *ProviderAPI) FetchRegions(provider string) ([]RegionInfo, error) {
	switch provider {
	case "digitalocean":
		return api.fetchDORegions()
	case "hetzner":
		return api.fetchHetznerRegions()
	case "linode":
		return api.fetchLinodeRegions()
	case "aws":
		return api.getAWSRegions() // Static list, AWS regions are well-known
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}
}

// FetchSizes gets available server sizes from provider API
func (api *ProviderAPI) FetchSizes(provider, region string) ([]SizeInfo, error) {
	switch provider {
	case "digitalocean":
		return api.fetchDOSizes(region)
	case "hetzner":
		return api.fetchHetznerSizes()
	case "linode":
		return api.fetchLinodeSizes()
	case "aws":
		return api.getAWSSizes() // Static list with known pricing
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}
}

// DigitalOcean API
func (api *ProviderAPI) fetchDORegions() ([]RegionInfo, error) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("DIGITALOCEAN_TOKEN not set")
	}

	req, err := http.NewRequest("GET", "https://api.digitalocean.com/v2/regions", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		Regions []struct {
			Slug      string   `json:"slug"`
			Name      string   `json:"name"`
			Available bool     `json:"available"`
			Features  []string `json:"features"`
		} `json:"regions"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	regions := make([]RegionInfo, 0, len(result.Regions))
	for _, r := range result.Regions {
		if r.Available {
			regions = append(regions, RegionInfo{
				Slug:        r.Slug,
				Name:        r.Name,
				Available:   r.Available,
				Description: r.Name,
			})
		}
	}

	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Slug < regions[j].Slug
	})

	return regions, nil
}

func (api *ProviderAPI) fetchDOSizes(region string) ([]SizeInfo, error) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("DIGITALOCEAN_TOKEN not set")
	}

	req, err := http.NewRequest("GET", "https://api.digitalocean.com/v2/sizes", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		Sizes []struct {
			Slug         string   `json:"slug"`
			Memory       int      `json:"memory"`
			VCPUs        int      `json:"vcpus"`
			Disk         int      `json:"disk"`
			PriceHourly  float64  `json:"price_hourly"`
			PriceMonthly float64  `json:"price_monthly"`
			Regions      []string `json:"regions"`
			Available    bool     `json:"available"`
			Description  string   `json:"description"`
		} `json:"sizes"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	sizes := make([]SizeInfo, 0)
	for _, s := range result.Sizes {
		// Filter by region if specified
		if region != "" {
			inRegion := false
			for _, r := range s.Regions {
				if r == region {
					inRegion = true
					break
				}
			}
			if !inRegion {
				continue
			}
		}

		// Only include basic/standard droplets (s-* prefix)
		if len(s.Slug) > 2 && s.Slug[:2] == "s-" && s.Available {
			sizes = append(sizes, SizeInfo{
				Slug:         s.Slug,
				Description:  s.Description,
				VCPUs:        s.VCPUs,
				Memory:       s.Memory,
				Disk:         s.Disk,
				PriceHourly:  s.PriceHourly,
				PriceMonthly: s.PriceMonthly,
				Available:    s.Available,
			})
		}
	}

	// Sort by price
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i].PriceMonthly < sizes[j].PriceMonthly
	})

	return sizes, nil
}

// Hetzner API
func (api *ProviderAPI) fetchHetznerRegions() ([]RegionInfo, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN not set")
	}

	req, err := http.NewRequest("GET", "https://api.hetzner.cloud/v1/locations", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		Locations []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Country     string `json:"country"`
			City        string `json:"city"`
		} `json:"locations"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	regions := make([]RegionInfo, 0, len(result.Locations))
	for _, l := range result.Locations {
		regions = append(regions, RegionInfo{
			Slug:        l.Name,
			Name:        l.City,
			Available:   true,
			Description: fmt.Sprintf("%s (%s)", l.City, l.Country),
		})
	}

	return regions, nil
}

func (api *ProviderAPI) fetchHetznerSizes() ([]SizeInfo, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN not set")
	}

	req, err := http.NewRequest("GET", "https://api.hetzner.cloud/v1/server_types", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		ServerTypes []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Cores       int    `json:"cores"`
			Memory      int    `json:"memory"` // GB
			Disk        int    `json:"disk"`
			Prices      []struct {
				Location   string `json:"location"`
				PriceHourly struct {
					Gross string `json:"gross"`
				} `json:"price_hourly"`
				PriceMonthly struct {
					Gross string `json:"gross"`
				} `json:"price_monthly"`
			} `json:"prices"`
		} `json:"server_types"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	sizes := make([]SizeInfo, 0)
	for _, s := range result.ServerTypes {
		// Only include shared CPU types (cx*, cpx*, cax*)
		if len(s.Name) < 2 {
			continue
		}
		// Check for valid prefixes
		isSharedCPU := strings.HasPrefix(s.Name, "cx") ||
			strings.HasPrefix(s.Name, "cpx") ||
			strings.HasPrefix(s.Name, "cax")
		if !isSharedCPU {
			continue
		}

		var hourly, monthly float64
		if len(s.Prices) > 0 {
			// Parse prices, ignore errors (will default to 0)
			if _, err := fmt.Sscanf(s.Prices[0].PriceHourly.Gross, "%f", &hourly); err != nil {
				hourly = 0
			}
			if _, err := fmt.Sscanf(s.Prices[0].PriceMonthly.Gross, "%f", &monthly); err != nil {
				monthly = 0
			}
		}

		sizes = append(sizes, SizeInfo{
			Slug:         s.Name,
			Description:  s.Description,
			VCPUs:        s.Cores,
			Memory:       s.Memory * 1024, // Convert GB to MB
			Disk:         s.Disk,
			PriceHourly:  hourly,
			PriceMonthly: monthly,
			Available:    true,
		})
	}

	// Sort by price
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i].PriceMonthly < sizes[j].PriceMonthly
	})

	return sizes, nil
}

// Linode API
func (api *ProviderAPI) fetchLinodeRegions() ([]RegionInfo, error) {
	// Linode regions endpoint is public
	req, err := http.NewRequest("GET", "https://api.linode.com/v4/regions", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Country string `json:"country"`
			Status  string `json:"status"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	regions := make([]RegionInfo, 0)
	for _, r := range result.Data {
		if r.Status == "ok" {
			regions = append(regions, RegionInfo{
				Slug:        r.ID,
				Name:        r.Label,
				Available:   true,
				Description: fmt.Sprintf("%s (%s)", r.Label, r.Country),
			})
		}
	}

	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Slug < regions[j].Slug
	})

	return regions, nil
}

func (api *ProviderAPI) fetchLinodeSizes() ([]SizeInfo, error) {
	// Linode types endpoint is public
	req, err := http.NewRequest("GET", "https://api.linode.com/v4/linode/types", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			VCPUs   int    `json:"vcpus"`
			Memory  int    `json:"memory"` // MB
			Disk    int    `json:"disk"`   // MB
			Price   struct {
				Hourly  float64 `json:"hourly"`
				Monthly float64 `json:"monthly"`
			} `json:"price"`
			Class string `json:"class"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	sizes := make([]SizeInfo, 0)
	for _, s := range result.Data {
		// Only include standard and nanode types
		if s.Class != "standard" && s.Class != "nanode" {
			continue
		}

		sizes = append(sizes, SizeInfo{
			Slug:         s.ID,
			Description:  s.Label,
			VCPUs:        s.VCPUs,
			Memory:       s.Memory,
			Disk:         s.Disk / 1024, // Convert MB to GB
			PriceHourly:  s.Price.Hourly,
			PriceMonthly: s.Price.Monthly,
			Available:    true,
		})
	}

	// Sort by price
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i].PriceMonthly < sizes[j].PriceMonthly
	})

	return sizes, nil
}

// AWS static data (regions and pricing are well-documented)
func (api *ProviderAPI) getAWSRegions() ([]RegionInfo, error) {
	// Common AWS regions - could be expanded
	regions := []RegionInfo{
		{Slug: "us-east-1", Name: "N. Virginia", Description: "US East (N. Virginia)"},
		{Slug: "us-east-2", Name: "Ohio", Description: "US East (Ohio)"},
		{Slug: "us-west-1", Name: "N. California", Description: "US West (N. California)"},
		{Slug: "us-west-2", Name: "Oregon", Description: "US West (Oregon)"},
		{Slug: "eu-west-1", Name: "Ireland", Description: "EU (Ireland)"},
		{Slug: "eu-west-2", Name: "London", Description: "EU (London)"},
		{Slug: "eu-central-1", Name: "Frankfurt", Description: "EU (Frankfurt)"},
		{Slug: "ap-southeast-1", Name: "Singapore", Description: "Asia Pacific (Singapore)"},
		{Slug: "ap-southeast-2", Name: "Sydney", Description: "Asia Pacific (Sydney)"},
		{Slug: "ap-northeast-1", Name: "Tokyo", Description: "Asia Pacific (Tokyo)"},
		{Slug: "ap-south-1", Name: "Mumbai", Description: "Asia Pacific (Mumbai)"},
		{Slug: "sa-east-1", Name: "Sao Paulo", Description: "South America (Sao Paulo)"},
	}

	for i := range regions {
		regions[i].Available = true
	}

	return regions, nil
}

func (api *ProviderAPI) getAWSSizes() ([]SizeInfo, error) {
	// Common EC2 instance types with approximate us-east-1 pricing
	sizes := []SizeInfo{
		{Slug: "t3.micro", Description: "T3 Micro", VCPUs: 2, Memory: 1024, Disk: 0, PriceHourly: 0.0104, PriceMonthly: 7.59},
		{Slug: "t3.small", Description: "T3 Small", VCPUs: 2, Memory: 2048, Disk: 0, PriceHourly: 0.0208, PriceMonthly: 15.18},
		{Slug: "t3.medium", Description: "T3 Medium", VCPUs: 2, Memory: 4096, Disk: 0, PriceHourly: 0.0416, PriceMonthly: 30.37},
		{Slug: "t3.large", Description: "T3 Large", VCPUs: 2, Memory: 8192, Disk: 0, PriceHourly: 0.0832, PriceMonthly: 60.74},
		{Slug: "t3.xlarge", Description: "T3 XLarge", VCPUs: 4, Memory: 16384, Disk: 0, PriceHourly: 0.1664, PriceMonthly: 121.47},
		{Slug: "t3.2xlarge", Description: "T3 2XLarge", VCPUs: 8, Memory: 32768, Disk: 0, PriceHourly: 0.3328, PriceMonthly: 242.94},
	}

	for i := range sizes {
		sizes[i].Available = true
	}

	return sizes, nil
}

// FormatMemory formats memory in MB to human readable
func FormatMemory(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%dGB", mb/1024)
	}
	return fmt.Sprintf("%dMB", mb)
}
