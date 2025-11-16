package deployer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"golang.org/x/sync/semaphore"
)

// ParallelBuilder handles parallel service builds with dependency-aware grouping
type ParallelBuilder struct {
	deployer      *Deployer
	maxConcurrent int64
	semaphore     *semaphore.Weighted
	verbose       bool
}

// BuildJob represents a single service build job
type BuildJob struct {
	ServiceName string
	Service     *config.ServiceConfig
	ImageName   string
}

// BuildResult contains the result of a build job
type BuildResult struct {
	ServiceName string
	ImageName   string
	Duration    time.Duration
	Error       error
}

// NewParallelBuilder creates a new parallel builder
func NewParallelBuilder(deployer *Deployer, maxConcurrent int) *ParallelBuilder {
	if maxConcurrent <= 0 {
		maxConcurrent = 4 // Default to 4 concurrent builds
	}
	return &ParallelBuilder{
		deployer:      deployer,
		maxConcurrent: int64(maxConcurrent),
		semaphore:     semaphore.NewWeighted(int64(maxConcurrent)),
		verbose:       deployer.verbose,
	}
}

// GroupByDependencyLevel groups services by dependency level
// Level 0: services with no dependencies
// Level 1: services that depend only on level 0 services
// Level N: services that depend on services up to level N-1
func (pb *ParallelBuilder) GroupByDependencyLevel(services map[string]config.ServiceConfig) [][]string {
	levels := [][]string{}
	deployed := make(map[string]int) // service name -> level

	// Keep iterating until all services are assigned a level
	for len(deployed) < len(services) {
		currentLevel := []string{}

		for name, service := range services {
			// Skip if already deployed
			if _, exists := deployed[name]; exists {
				continue
			}

			// Check if all dependencies are satisfied
			canDeploy := true
			maxDepLevel := -1

			for _, dep := range service.DependsOn {
				depLevel, exists := deployed[dep]
				if !exists {
					canDeploy = false
					break
				}
				if depLevel > maxDepLevel {
					maxDepLevel = depLevel
				}
			}

			if canDeploy {
				currentLevel = append(currentLevel, name)
				deployed[name] = len(levels)
			}
		}

		// If no services can be deployed, we have a circular dependency
		if len(currentLevel) == 0 {
			// This shouldn't happen if dependencies were validated
			break
		}

		levels = append(levels, currentLevel)
	}

	if pb.verbose {
		fmt.Printf("\n=== Parallel Build Groups ===\n")
		for i, level := range levels {
			fmt.Printf("  Level %d (%d services in parallel): %v\n", i, len(level), level)
		}
		fmt.Println()
	}

	return levels
}

// BuildAllLevels builds all services in dependency order with parallelization
func (pb *ParallelBuilder) BuildAllLevels(ctx context.Context, services map[string]config.ServiceConfig, skipBuild bool) (map[string]string, error) {
	levels := pb.GroupByDependencyLevel(services)
	imageMap := make(map[string]string) // service name -> image name

	// Build each level sequentially, but parallelize within each level
	for levelNum, level := range levels {
		if pb.verbose {
			fmt.Printf("\n→ Building level %d (%d services)...\n", levelNum, len(level))
		}

		results, err := pb.buildLevel(ctx, services, level, skipBuild)
		if err != nil {
			return imageMap, fmt.Errorf("failed to build level %d: %w", levelNum, err)
		}

		// Store successful results
		for _, result := range results {
			if result.Error == nil {
				imageMap[result.ServiceName] = result.ImageName
			}
		}
	}

	return imageMap, nil
}

// buildLevel builds all services in a single level in parallel
func (pb *ParallelBuilder) buildLevel(ctx context.Context, allServices map[string]config.ServiceConfig, serviceNames []string, skipBuild bool) ([]BuildResult, error) {
	results := make([]BuildResult, len(serviceNames))
	errors := make(chan error, len(serviceNames))
	var wg sync.WaitGroup

	for i, serviceName := range serviceNames {
		wg.Add(1)

		// Launch goroutine for each service
		go func(index int, name string) {
			defer wg.Done()

			// Acquire semaphore
			if err := pb.semaphore.Acquire(ctx, 1); err != nil {
				results[index] = BuildResult{
					ServiceName: name,
					Error:       fmt.Errorf("failed to acquire semaphore: %w", err),
				}
				errors <- err
				return
			}
			defer pb.semaphore.Release(1)

			// Build the service
			result := pb.buildService(ctx, name, allServices[name], skipBuild)
			results[index] = result

			if result.Error != nil {
				errors <- result.Error
			}
		}(i, serviceName)
	}

	// Wait for all builds to complete
	wg.Wait()
	close(errors)

	// Check for errors
	var buildErrors []string
	for err := range errors {
		buildErrors = append(buildErrors, err.Error())
	}

	if len(buildErrors) > 0 {
		return results, fmt.Errorf("build failures: %v", buildErrors)
	}

	return results, nil
}

// buildService builds a single service
func (pb *ParallelBuilder) buildService(ctx context.Context, serviceName string, service config.ServiceConfig, skipBuild bool) BuildResult {
	start := time.Now()

	if pb.verbose {
		fmt.Printf("  → Building %s...\n", serviceName)
	}

	// Skip if no build path and using pre-built image
	if skipBuild || (service.Build == "" && service.Image != "") {
		imageName := service.Image
		if imageName == "" {
			imageName = pb.deployer.config.GetFullImageName(serviceName, pb.deployer.environment)
		}

		if pb.verbose {
			fmt.Printf("  ✓ Skipping build for %s (using %s)\n", serviceName, imageName)
		}

		return BuildResult{
			ServiceName: serviceName,
			ImageName:   imageName,
			Duration:    time.Since(start),
			Error:       nil,
		}
	}

	// Build the image
	imageName, err := pb.deployer.BuildImage(serviceName, &service)

	duration := time.Since(start)

	if err != nil {
		if pb.verbose {
			fmt.Printf("  ✗ Build failed for %s: %v\n", serviceName, err)
		}
		return BuildResult{
			ServiceName: serviceName,
			Duration:    duration,
			Error:       err,
		}
	}

	if pb.verbose {
		fmt.Printf("  ✓ Built %s in %.1fs\n", serviceName, duration.Seconds())
	}

	return BuildResult{
		ServiceName: serviceName,
		ImageName:   imageName,
		Duration:    duration,
		Error:       nil,
	}
}

// ParallelDeployer handles parallel service deployments
type ParallelDeployer struct {
	deployer      *Deployer
	maxConcurrent int64
	semaphore     *semaphore.Weighted
	verbose       bool
}

// NewParallelDeployer creates a new parallel deployer
func NewParallelDeployer(deployer *Deployer, maxConcurrent int) *ParallelDeployer {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &ParallelDeployer{
		deployer:      deployer,
		maxConcurrent: int64(maxConcurrent),
		semaphore:     semaphore.NewWeighted(int64(maxConcurrent)),
		verbose:       deployer.verbose,
	}
}

// DeployLevel deploys all services in a level in parallel
func (pd *ParallelDeployer) DeployLevel(ctx context.Context, services map[string]config.ServiceConfig, serviceNames []string, imageMap map[string]string) error {
	errors := make(chan error, len(serviceNames))
	var wg sync.WaitGroup

	for _, serviceName := range serviceNames {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()

			// Acquire semaphore
			if err := pd.semaphore.Acquire(ctx, 1); err != nil {
				errors <- fmt.Errorf("service %s: failed to acquire semaphore: %w", name, err)
				return
			}
			defer pd.semaphore.Release(1)

			// Deploy the service
			if pd.verbose {
				fmt.Printf("  → Deploying %s...\n", name)
			}

			service := services[name]
			if err := pd.deployer.DeployService(name, &service, true); err != nil {
				errors <- fmt.Errorf("service %s: %w", name, err)
				return
			}

			if pd.verbose {
				fmt.Printf("  ✓ Deployed %s\n", name)
			}
		}(serviceName)
	}

	// Wait for all deployments
	wg.Wait()
	close(errors)

	// Check for errors
	var deployErrors []string
	for err := range errors {
		deployErrors = append(deployErrors, err.Error())
	}

	if len(deployErrors) > 0 {
		return fmt.Errorf("deployment failures: %v", deployErrors)
	}

	return nil
}
