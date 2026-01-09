package infra

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TypesFetcher handles dynamic fetching of server types from provider APIs
type TypesFetcher struct {
	cacheDir    string
	cacheTTL    time.Duration
	httpClient  *http.Client
	mu          sync.RWMutex
	memoryCache map[string]*ServerTypeCache
}

// ServerTypeCache represents cached server types for a provider
type ServerTypeCache struct {
	Provider   string       `json:"provider"`
	Types      []ServerType `json:"types"`
	FetchedAt  time.Time    `json:"fetched_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
}

// ServerType represents a server type from a provider
type ServerType struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	CPUs        int     `json:"cpus"`
	Memory      int     `json:"memory"`      // in MB
	Disk        int     `json:"disk"`        // in GB
	PriceHourly float64 `json:"price_hourly"`
	Available   bool    `json:"available"`
}

// NewTypesFetcher creates a new types fetcher with caching
func NewTypesFetcher(cacheDir string) *TypesFetcher {
	return &TypesFetcher{
		cacheDir:    cacheDir,
		cacheTTL:    24 * time.Hour, // Cache for 24 hours
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		memoryCache: make(map[string]*ServerTypeCache),
	}
}

// GetServerTypes returns available server types for a provider
func (f *TypesFetcher) GetServerTypes(provider, token string) ([]ServerType, error) {
	// Check memory cache first
	f.mu.RLock()
	if cache, ok := f.memoryCache[provider]; ok && time.Now().Before(cache.ExpiresAt) {
		f.mu.RUnlock()
		return cache.Types, nil
	}
	f.mu.RUnlock()

	// Check file cache
	cache, err := f.loadFromFileCache(provider)
	if err == nil && time.Now().Before(cache.ExpiresAt) {
		f.mu.Lock()
		f.memoryCache[provider] = cache
		f.mu.Unlock()
		return cache.Types, nil
	}

	// Fetch from API
	types, err := f.fetchFromAPI(provider, token)
	if err != nil {
		// If fetch fails but we have stale cache, use it
		if cache != nil {
			return cache.Types, nil
		}
		return nil, err
	}

	// Save to cache
	newCache := &ServerTypeCache{
		Provider:  provider,
		Types:     types,
		FetchedAt: time.Now(),
		ExpiresAt: time.Now().Add(f.cacheTTL),
	}

	f.mu.Lock()
	f.memoryCache[provider] = newCache
	f.mu.Unlock()

	_ = f.saveToFileCache(provider, newCache)

	return types, nil
}

// ValidateServerType checks if a server type is valid for a provider
func (f *TypesFetcher) ValidateServerType(provider, serverType, token string) (bool, string, error) {
	types, err := f.GetServerTypes(provider, token)
	if err != nil {
		return false, "", err
	}

	for _, t := range types {
		if t.ID == serverType {
			if !t.Available {
				return false, fmt.Sprintf("server type '%s' exists but is not currently available", serverType), nil
			}
			return true, "", nil
		}
	}

	// Find suggestions
	suggestions := f.findSimilarTypes(serverType, types, 5)
	suggestionMsg := ""
	if len(suggestions) > 0 {
		suggestionMsg = fmt.Sprintf("Did you mean: %v", suggestions)
	}

	return false, fmt.Sprintf("server type '%s' not found for provider '%s'. %s", serverType, provider, suggestionMsg), nil
}

// findSimilarTypes finds similar server types for suggestions
func (f *TypesFetcher) findSimilarTypes(input string, types []ServerType, max int) []string {
	var suggestions []string
	for _, t := range types {
		if !t.Available {
			continue
		}
		// Simple prefix/suffix matching
		if len(suggestions) < max {
			if len(input) >= 2 && len(t.ID) >= 2 {
				if input[:2] == t.ID[:2] || input[len(input)-2:] == t.ID[len(t.ID)-2:] {
					suggestions = append(suggestions, t.ID)
				}
			}
		}
	}
	return suggestions
}

// fetchFromAPI fetches server types from the provider API
func (f *TypesFetcher) fetchFromAPI(provider, token string) ([]ServerType, error) {
	switch provider {
	case "hetzner":
		return f.fetchHetznerTypes(token)
	case "digitalocean":
		return f.fetchDigitalOceanTypes(token)
	case "linode":
		return f.fetchLinodeTypes(token)
	case "aws":
		return f.fetchAWSTypes(token)
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}
}

// Hetzner API response structures
type hetznerServerTypesResponse struct {
	ServerTypes []hetznerServerType `json:"server_types"`
}

type hetznerServerType struct {
	ID          int                    `json:"id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Cores       int                    `json:"cores"`
	Memory      float64                `json:"memory"` // in GB
	Disk        int                    `json:"disk"`   // in GB
	Deprecated  bool                   `json:"deprecated"`
	Prices      []hetznerPrice         `json:"prices"`
}

type hetznerPrice struct {
	Location     string                 `json:"location"`
	PriceHourly  hetznerPriceValue      `json:"price_hourly"`
	PriceMonthly hetznerPriceValue      `json:"price_monthly"`
}

type hetznerPriceValue struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

func (f *TypesFetcher) fetchHetznerTypes(token string) ([]ServerType, error) {
	req, err := http.NewRequest("GET", "https://api.hetzner.cloud/v1/server_types", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Hetzner server types: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Hetzner API error (status %d): %s", resp.StatusCode, string(body))
	}

	var response hetznerServerTypesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode Hetzner response: %w", err)
	}

	types := make([]ServerType, 0, len(response.ServerTypes))
	for _, st := range response.ServerTypes {
		priceHourly := 0.0
		if len(st.Prices) > 0 {
			fmt.Sscanf(st.Prices[0].PriceHourly.Gross, "%f", &priceHourly)
		}

		types = append(types, ServerType{
			ID:          st.Name,
			Name:        st.Name,
			Description: st.Description,
			CPUs:        st.Cores,
			Memory:      int(st.Memory * 1024), // Convert GB to MB
			Disk:        st.Disk,
			PriceHourly: priceHourly,
			Available:   !st.Deprecated,
		})
	}

	return types, nil
}

// DigitalOcean API response structures
type digitalOceanSizesResponse struct {
	Sizes []digitalOceanSize `json:"sizes"`
}

type digitalOceanSize struct {
	Slug         string   `json:"slug"`
	Memory       int      `json:"memory"`      // in MB
	VCPUs        int      `json:"vcpus"`
	Disk         int      `json:"disk"`        // in GB
	PriceHourly  float64  `json:"price_hourly"`
	PriceMonthly float64  `json:"price_monthly"`
	Available    bool     `json:"available"`
	Regions      []string `json:"regions"`
	Description  string   `json:"description"`
}

func (f *TypesFetcher) fetchDigitalOceanTypes(token string) ([]ServerType, error) {
	req, err := http.NewRequest("GET", "https://api.digitalocean.com/v2/sizes", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch DigitalOcean sizes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("DigitalOcean API error (status %d): %s", resp.StatusCode, string(body))
	}

	var response digitalOceanSizesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode DigitalOcean response: %w", err)
	}

	types := make([]ServerType, 0, len(response.Sizes))
	for _, s := range response.Sizes {
		desc := s.Description
		if desc == "" {
			desc = fmt.Sprintf("%d vCPU, %dMB RAM, %dGB SSD", s.VCPUs, s.Memory, s.Disk)
		}

		types = append(types, ServerType{
			ID:          s.Slug,
			Name:        s.Slug,
			Description: desc,
			CPUs:        s.VCPUs,
			Memory:      s.Memory,
			Disk:        s.Disk,
			PriceHourly: s.PriceHourly,
			Available:   s.Available,
		})
	}

	return types, nil
}

// Linode API response structures
type linodeTypesResponse struct {
	Data []linodeType `json:"data"`
}

type linodeType struct {
	ID          string       `json:"id"`
	Label       string       `json:"label"`
	Memory      int          `json:"memory"`      // in MB
	VCPUs       int          `json:"vcpus"`
	Disk        int          `json:"disk"`        // in MB
	Price       linodePrice  `json:"price"`
}

type linodePrice struct {
	Hourly  float64 `json:"hourly"`
	Monthly float64 `json:"monthly"`
}

func (f *TypesFetcher) fetchLinodeTypes(token string) ([]ServerType, error) {
	req, err := http.NewRequest("GET", "https://api.linode.com/v4/linode/types", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Linode types: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Linode API error (status %d): %s", resp.StatusCode, string(body))
	}

	var response linodeTypesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode Linode response: %w", err)
	}

	types := make([]ServerType, 0, len(response.Data))
	for _, t := range response.Data {
		types = append(types, ServerType{
			ID:          t.ID,
			Name:        t.Label,
			Description: fmt.Sprintf("%d vCPU, %dMB RAM, %dGB SSD", t.VCPUs, t.Memory, t.Disk/1024),
			CPUs:        t.VCPUs,
			Memory:      t.Memory,
			Disk:        t.Disk / 1024, // Convert MB to GB
			PriceHourly: t.Price.Hourly,
			Available:   true,
		})
	}

	return types, nil
}

// AWS doesn't have a simple public API for instance types without SDK
// We'll use a static list but with a note that it should be updated
func (f *TypesFetcher) fetchAWSTypes(token string) ([]ServerType, error) {
	// AWS requires the SDK for proper instance type listing
	// For now, return the common types that are available in most regions
	// This could be enhanced to use AWS EC2 DescribeInstanceTypes API
	types := []ServerType{
		// T3 Burstable
		{ID: "t3.nano", Name: "t3.nano", Description: "2 vCPU, 0.5GB RAM", CPUs: 2, Memory: 512, Available: true},
		{ID: "t3.micro", Name: "t3.micro", Description: "2 vCPU, 1GB RAM", CPUs: 2, Memory: 1024, Available: true},
		{ID: "t3.small", Name: "t3.small", Description: "2 vCPU, 2GB RAM", CPUs: 2, Memory: 2048, Available: true},
		{ID: "t3.medium", Name: "t3.medium", Description: "2 vCPU, 4GB RAM", CPUs: 2, Memory: 4096, Available: true},
		{ID: "t3.large", Name: "t3.large", Description: "2 vCPU, 8GB RAM", CPUs: 2, Memory: 8192, Available: true},
		{ID: "t3.xlarge", Name: "t3.xlarge", Description: "4 vCPU, 16GB RAM", CPUs: 4, Memory: 16384, Available: true},
		{ID: "t3.2xlarge", Name: "t3.2xlarge", Description: "8 vCPU, 32GB RAM", CPUs: 8, Memory: 32768, Available: true},
		// T3a AMD
		{ID: "t3a.nano", Name: "t3a.nano", Description: "2 vCPU, 0.5GB RAM (AMD)", CPUs: 2, Memory: 512, Available: true},
		{ID: "t3a.micro", Name: "t3a.micro", Description: "2 vCPU, 1GB RAM (AMD)", CPUs: 2, Memory: 1024, Available: true},
		{ID: "t3a.small", Name: "t3a.small", Description: "2 vCPU, 2GB RAM (AMD)", CPUs: 2, Memory: 2048, Available: true},
		{ID: "t3a.medium", Name: "t3a.medium", Description: "2 vCPU, 4GB RAM (AMD)", CPUs: 2, Memory: 4096, Available: true},
		{ID: "t3a.large", Name: "t3a.large", Description: "2 vCPU, 8GB RAM (AMD)", CPUs: 2, Memory: 8192, Available: true},
		{ID: "t3a.xlarge", Name: "t3a.xlarge", Description: "4 vCPU, 16GB RAM (AMD)", CPUs: 4, Memory: 16384, Available: true},
		{ID: "t3a.2xlarge", Name: "t3a.2xlarge", Description: "8 vCPU, 32GB RAM (AMD)", CPUs: 8, Memory: 32768, Available: true},
		// M5 General Purpose
		{ID: "m5.large", Name: "m5.large", Description: "2 vCPU, 8GB RAM", CPUs: 2, Memory: 8192, Available: true},
		{ID: "m5.xlarge", Name: "m5.xlarge", Description: "4 vCPU, 16GB RAM", CPUs: 4, Memory: 16384, Available: true},
		{ID: "m5.2xlarge", Name: "m5.2xlarge", Description: "8 vCPU, 32GB RAM", CPUs: 8, Memory: 32768, Available: true},
		{ID: "m5.4xlarge", Name: "m5.4xlarge", Description: "16 vCPU, 64GB RAM", CPUs: 16, Memory: 65536, Available: true},
		// M6i Intel
		{ID: "m6i.large", Name: "m6i.large", Description: "2 vCPU, 8GB RAM (Intel)", CPUs: 2, Memory: 8192, Available: true},
		{ID: "m6i.xlarge", Name: "m6i.xlarge", Description: "4 vCPU, 16GB RAM (Intel)", CPUs: 4, Memory: 16384, Available: true},
		{ID: "m6i.2xlarge", Name: "m6i.2xlarge", Description: "8 vCPU, 32GB RAM (Intel)", CPUs: 8, Memory: 32768, Available: true},
		// C5 Compute
		{ID: "c5.large", Name: "c5.large", Description: "2 vCPU, 4GB RAM", CPUs: 2, Memory: 4096, Available: true},
		{ID: "c5.xlarge", Name: "c5.xlarge", Description: "4 vCPU, 8GB RAM", CPUs: 4, Memory: 8192, Available: true},
		{ID: "c5.2xlarge", Name: "c5.2xlarge", Description: "8 vCPU, 16GB RAM", CPUs: 8, Memory: 16384, Available: true},
		// R5 Memory
		{ID: "r5.large", Name: "r5.large", Description: "2 vCPU, 16GB RAM", CPUs: 2, Memory: 16384, Available: true},
		{ID: "r5.xlarge", Name: "r5.xlarge", Description: "4 vCPU, 32GB RAM", CPUs: 4, Memory: 32768, Available: true},
		{ID: "r5.2xlarge", Name: "r5.2xlarge", Description: "8 vCPU, 64GB RAM", CPUs: 8, Memory: 65536, Available: true},
	}

	return types, nil
}

// File cache operations

func (f *TypesFetcher) getCacheFilePath(provider string) string {
	return filepath.Join(f.cacheDir, fmt.Sprintf("server_types_%s.json", provider))
}

func (f *TypesFetcher) loadFromFileCache(provider string) (*ServerTypeCache, error) {
	path := f.getCacheFilePath(provider)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cache ServerTypeCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}

	return &cache, nil
}

func (f *TypesFetcher) saveToFileCache(provider string, cache *ServerTypeCache) error {
	if err := os.MkdirAll(f.cacheDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(f.getCacheFilePath(provider), data, 0644)
}

// ClearCache clears the cache for a provider or all providers
func (f *TypesFetcher) ClearCache(provider string) error {
	f.mu.Lock()
	if provider == "" {
		f.memoryCache = make(map[string]*ServerTypeCache)
	} else {
		delete(f.memoryCache, provider)
	}
	f.mu.Unlock()

	if provider == "" {
		// Clear all cache files
		entries, err := os.ReadDir(f.cacheDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
				os.Remove(filepath.Join(f.cacheDir, entry.Name()))
			}
		}
	} else {
		os.Remove(f.getCacheFilePath(provider))
	}

	return nil
}

// ListAvailableTypes returns a formatted list of available server types
func (f *TypesFetcher) ListAvailableTypes(provider, token string) (string, error) {
	types, err := f.GetServerTypes(provider, token)
	if err != nil {
		return "", err
	}

	result := fmt.Sprintf("Available server types for %s:\n\n", provider)
	result += fmt.Sprintf("%-20s %-40s %6s %8s %8s %10s\n", "ID", "Description", "CPUs", "Memory", "Disk", "$/hour")
	result += fmt.Sprintf("%-20s %-40s %6s %8s %8s %10s\n", "----", "----", "----", "----", "----", "----")

	for _, t := range types {
		if !t.Available {
			continue
		}
		memStr := fmt.Sprintf("%dMB", t.Memory)
		if t.Memory >= 1024 {
			memStr = fmt.Sprintf("%.1fGB", float64(t.Memory)/1024)
		}
		result += fmt.Sprintf("%-20s %-40s %6d %8s %6dGB %10.4f\n",
			t.ID, truncate(t.Description, 40), t.CPUs, memStr, t.Disk, t.PriceHourly)
	}

	return result, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// Global fetcher instance
var globalFetcher *TypesFetcher
var fetcherOnce sync.Once

// GetTypesFetcher returns the global types fetcher instance
func GetTypesFetcher() *TypesFetcher {
	fetcherOnce.Do(func() {
		cacheDir := filepath.Join(os.TempDir(), "tako-server-types")
		globalFetcher = NewTypesFetcher(cacheDir)
	})
	return globalFetcher
}

// DynamicValidateServerType validates a server type dynamically using provider API
func DynamicValidateServerType(provider, serverType, token string) error {
	if token == "" {
		// Fall back to static validation if no token provided
		return ValidateServerType(provider, serverType)
	}

	fetcher := GetTypesFetcher()
	valid, msg, err := fetcher.ValidateServerType(provider, serverType, token)
	if err != nil {
		// Fall back to static validation on API error
		return ValidateServerType(provider, serverType)
	}

	if !valid {
		return fmt.Errorf("%s", msg)
	}

	return nil
}
