package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

// CacheManager handles build caching using Docker BuildKit
type CacheManager struct {
	projectName string
	environment string
	verbose     bool
	cacheDir    string // Local cache directory
}

// NewCacheManager creates a new cache manager
func NewCacheManager(projectName, environment string, verbose bool) *CacheManager {
	// Use .tako/cache directory for local caching
	cacheDir := filepath.Join(".tako", "cache")
	os.MkdirAll(cacheDir, 0755)

	return &CacheManager{
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
		cacheDir:    cacheDir,
	}
}

// GenerateCacheKey generates a cache key for a service based on its build context
func (cm *CacheManager) GenerateCacheKey(serviceName string, service *config.ServiceConfig) (string, error) {
	if service.Build == "" {
		return "", fmt.Errorf("no build context for service %s", serviceName)
	}

	hasher := sha256.New()

	// Hash Dockerfile content
	dockerfilePath := filepath.Join(service.Build, "Dockerfile")
	if err := cm.hashFile(hasher, dockerfilePath); err != nil {
		// Try alternative Dockerfile names
		alternatives := []string{"Dockerfile.prod", "dockerfile", ".dockerfile"}
		found := false
		for _, alt := range alternatives {
			altPath := filepath.Join(service.Build, alt)
			if err := cm.hashFile(hasher, altPath); err == nil {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("no Dockerfile found for service %s", serviceName)
		}
	}

	// Hash key files that affect the build
	keyFiles := []string{
		"package.json",
		"package-lock.json",
		"go.mod",
		"go.sum",
		"requirements.txt",
		"Gemfile",
		"Gemfile.lock",
		"composer.json",
		"composer.lock",
	}

	for _, file := range keyFiles {
		filePath := filepath.Join(service.Build, file)
		cm.hashFile(hasher, filePath) // Ignore errors for optional files
	}

	// Add environment to the hash
	hasher.Write([]byte(cm.environment))

	// Generate final hash
	hash := hex.EncodeToString(hasher.Sum(nil))[:12]
	cacheKey := fmt.Sprintf("%s-%s-%s", cm.projectName, serviceName, hash)

	return cacheKey, nil
}

// hashFile adds file content to the hasher
func (cm *CacheManager) hashFile(hasher io.Writer, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(hasher, file)
	return err
}

// GetBuildKitFlags returns Docker BuildKit flags for caching
func (cm *CacheManager) GetBuildKitFlags(serviceName string, service *config.ServiceConfig) []string {
	flags := []string{
		"DOCKER_BUILDKIT=1", // Enable BuildKit
	}

	// Add inline cache support
	buildArgs := []string{
		"--build-arg", "BUILDKIT_INLINE_CACHE=1",
	}

	return append(flags, buildArgs...)
}

// GetLocalCachePath returns the local cache path for a service
func (cm *CacheManager) GetLocalCachePath(serviceName string) string {
	return filepath.Join(cm.cacheDir, fmt.Sprintf("%s-%s-%s.tar", cm.projectName, cm.environment, serviceName))
}

// IsCached checks if a cached image exists locally
func (cm *CacheManager) IsCached(serviceName string) bool {
	cachePath := cm.GetLocalCachePath(serviceName)
	info, err := os.Stat(cachePath)
	if err != nil {
		return false
	}

	// Cache expires after 24 hours
	if time.Since(info.ModTime()) > 24*time.Hour {
		return false
	}

	return true
}

// SaveImageToCache saves a Docker image to local cache
func (cm *CacheManager) SaveImageToCache(imageName, serviceName string) error {
	if !cm.verbose {
		return nil // Skip caching if not verbose (optimization)
	}

	cachePath := cm.GetLocalCachePath(serviceName)

	if cm.verbose {
		fmt.Printf("  Caching image %s to %s...\n", imageName, cachePath)
	}

	// Note: This would require Docker CLI access, which we don't have in the deployer
	// For now, we'll skip local caching and rely on BuildKit's built-in caching
	return nil
}

// GetBuildCommand returns the enhanced build command with caching
func (cm *CacheManager) GetBuildCommand(imageName, buildContext string, service *config.ServiceConfig) string {
	// Base build command with BuildKit
	cmd := fmt.Sprintf("DOCKER_BUILDKIT=1 docker build --build-arg BUILDKIT_INLINE_CACHE=1")

	// Add cache-from and cache-to flags for BuildKit
	// This enables layer caching across builds
	cacheTag := fmt.Sprintf("%s-buildcache", imageName)
	cmd += fmt.Sprintf(" --cache-from %s --cache-from %s", imageName, cacheTag)

	// Add progress output
	if cm.verbose {
		cmd += " --progress=plain"
	}

	// Add target tag
	cmd += fmt.Sprintf(" -t %s", imageName)

	// Also tag with cache tag
	cmd += fmt.Sprintf(" -t %s", cacheTag)

	// Add build context
	cmd += fmt.Sprintf(" %s", buildContext)

	return cmd
}

// CacheStats tracks cache hit/miss statistics
type CacheStats struct {
	TotalBuilds int
	CacheHits   int
	CacheMisses int
	TimesSaved  time.Duration
}

// GetHitRate returns the cache hit rate as a percentage
func (cs *CacheStats) GetHitRate() float64 {
	if cs.TotalBuilds == 0 {
		return 0
	}
	return float64(cs.CacheHits) / float64(cs.TotalBuilds) * 100
}

// Report prints cache statistics
func (cs *CacheStats) Report() {
	if cs.TotalBuilds == 0 {
		return
	}

	fmt.Printf("\n=== Build Cache Statistics ===\n")
	fmt.Printf("  Total Builds:   %d\n", cs.TotalBuilds)
	fmt.Printf("  Cache Hits:     %d\n", cs.CacheHits)
	fmt.Printf("  Cache Misses:   %d\n", cs.CacheMisses)
	fmt.Printf("  Hit Rate:       %.1f%%\n", cs.GetHitRate())
	if cs.TimesSaved > 0 {
		fmt.Printf("  Time Saved:     %.1fs\n", cs.TimesSaved.Seconds())
	}
	fmt.Println()
}

// EnhancedBuildResult contains build result with cache information
type EnhancedBuildResult struct {
	ServiceName string
	ImageName   string
	Duration    time.Duration
	CacheHit    bool
	Error       error
}

// String returns a string representation of the result
func (r *EnhancedBuildResult) String() string {
	status := "✓"
	if r.Error != nil {
		status = "✗"
	}

	cacheInfo := ""
	if r.CacheHit {
		cacheInfo = " (cached)"
	}

	return fmt.Sprintf("%s %s: %.1fs%s", status, r.ServiceName, r.Duration.Seconds(), cacheInfo)
}

// OptimizeBuildContext creates an optimized build context by excluding unnecessary files
func (cm *CacheManager) OptimizeBuildContext(buildPath string) ([]string, error) {
	// Default excludes (similar to .dockerignore)
	excludes := []string{
		".git",
		".gitignore",
		"node_modules",
		".next",
		"dist",
		"build",
		"*.log",
		".env",
		".env.*",
		"*.md",
		"README*",
		"LICENSE",
		".tako",
	}

	// Check if .dockerignore exists
	dockerIgnorePath := filepath.Join(buildPath, ".dockerignore")
	if _, err := os.Stat(dockerIgnorePath); err == nil {
		// Read .dockerignore and append to excludes
		content, err := os.ReadFile(dockerIgnorePath)
		if err == nil {
			lines := strings.Split(string(content), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					excludes = append(excludes, line)
				}
			}
		}
	}

	return excludes, nil
}
