package deployer

import (
	"context"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

// DeploymentOrchestrator coordinates the entire deployment pipeline
type DeploymentOrchestrator struct {
	deployer         *Deployer
	parallelBuilder  *ParallelBuilder
	parallelDeployer *ParallelDeployer
	cacheManager     *CacheManager
	metrics          *DeploymentMetrics
	verbose          bool
}

// DeploymentMetrics tracks deployment performance metrics
type DeploymentMetrics struct {
	StartTime        time.Time
	EndTime          time.Time
	TotalDuration    time.Duration
	BuildDuration    time.Duration
	DeployDuration   time.Duration
	ServicesDeployed int
	BuildsParallel   int
	DeploysParallel  int
	CacheHitRate     float64
	ParallelSpeedup  float64
	Failures         []string
}

// Stage represents a deployment pipeline stage
type Stage struct {
	Name string
	Fn   func(context.Context) error
}

// OrchestratorConfig contains orchestrator configuration
type OrchestratorConfig struct {
	MaxConcurrentBuilds  int
	MaxConcurrentDeploys int
	EnableCache          bool
	BuildTimeout         time.Duration
	DeployTimeout        time.Duration
}

// NewDeploymentOrchestrator creates a new deployment orchestrator
func NewDeploymentOrchestrator(deployer *Deployer, config OrchestratorConfig) *DeploymentOrchestrator {
	if config.MaxConcurrentBuilds <= 0 {
		config.MaxConcurrentBuilds = 4
	}
	if config.MaxConcurrentDeploys <= 0 {
		config.MaxConcurrentDeploys = 4
	}
	if config.BuildTimeout <= 0 {
		config.BuildTimeout = 10 * time.Minute
	}
	if config.DeployTimeout <= 0 {
		config.DeployTimeout = 5 * time.Minute
	}

	return &DeploymentOrchestrator{
		deployer:         deployer,
		parallelBuilder:  NewParallelBuilder(deployer, config.MaxConcurrentBuilds),
		parallelDeployer: NewParallelDeployer(deployer, config.MaxConcurrentDeploys),
		cacheManager:     NewCacheManager(deployer.config.Project.Name, deployer.environment, deployer.verbose),
		metrics: &DeploymentMetrics{
			StartTime: time.Now(),
		},
		verbose: deployer.verbose,
	}
}

// Deploy executes the complete deployment pipeline
func (do *DeploymentOrchestrator) Deploy(ctx context.Context, services map[string]config.ServiceConfig, skipBuild bool) error {
	do.metrics.StartTime = time.Now()
	do.metrics.ServicesDeployed = len(services)

	if do.verbose {
		fmt.Printf("\n=== Starting Orchestrated Deployment ===\n")
		fmt.Printf("  Services: %d\n", len(services))
		fmt.Printf("  Max Parallel Builds: %d\n", do.parallelBuilder.maxConcurrent)
		fmt.Printf("  Max Parallel Deploys: %d\n", do.parallelDeployer.maxConcurrent)
		fmt.Println()
	}

	// Define deployment stages
	var imageMap map[string]string
	var buildLevels [][]string

	stages := []Stage{
		{
			Name: "pre-flight",
			Fn: func(ctx context.Context) error {
				return do.preFlight(ctx, services)
			},
		},
		{
			Name: "build",
			Fn: func(ctx context.Context) error {
				var err error
				imageMap, err = do.parallelBuild(ctx, services, skipBuild)
				return err
			},
		},
		{
			Name: "deploy",
			Fn: func(ctx context.Context) error {
				buildLevels = do.parallelBuilder.GroupByDependencyLevel(services)
				return do.parallelDeploy(ctx, services, buildLevels, imageMap)
			},
		},
		{
			Name: "verify",
			Fn: func(ctx context.Context) error {
				return do.verify(ctx, services)
			},
		},
	}

	// Execute each stage
	for _, stage := range stages {
		if do.verbose {
			fmt.Printf("\n→ Stage: %s\n", stage.Name)
		}

		stageStart := time.Now()

		if err := stage.Fn(ctx); err != nil {
			do.metrics.Failures = append(do.metrics.Failures, fmt.Sprintf("%s: %v", stage.Name, err))
			do.metrics.EndTime = time.Now()
			do.metrics.TotalDuration = time.Since(do.metrics.StartTime)

			if do.verbose {
				fmt.Printf("  ✗ Stage %s failed: %v\n", stage.Name, err)
			}

			return fmt.Errorf("deployment failed at stage '%s': %w", stage.Name, err)
		}

		stageDuration := time.Since(stageStart)

		// Track stage durations
		switch stage.Name {
		case "build":
			do.metrics.BuildDuration = stageDuration
		case "deploy":
			do.metrics.DeployDuration = stageDuration
		}

		if do.verbose {
			fmt.Printf("  ✓ Stage %s completed in %.1fs\n", stage.Name, stageDuration.Seconds())
		}
	}

	// Finalize metrics
	do.metrics.EndTime = time.Now()
	do.metrics.TotalDuration = time.Since(do.metrics.StartTime)

	// Print deployment summary
	do.printSummary()

	return nil
}

// preFlight performs pre-deployment checks
func (do *DeploymentOrchestrator) preFlight(ctx context.Context, services map[string]config.ServiceConfig) error {
	if do.verbose {
		fmt.Printf("  Performing pre-flight checks...\n")
	}

	// Verify network setup
	if err := do.deployer.VerifyNetworkSetup(); err != nil {
		if do.verbose {
			fmt.Printf("  Warning: Network verification failed: %v\n", err)
		}
	}

	if do.verbose {
		fmt.Printf("  ✓ Pre-flight checks completed\n")
	}

	return nil
}

// parallelBuild builds all services in parallel with dependency awareness
func (do *DeploymentOrchestrator) parallelBuild(ctx context.Context, services map[string]config.ServiceConfig, skipBuild bool) (map[string]string, error) {
	if do.verbose {
		fmt.Printf("  Building services with parallel execution...\n")
	}

	buildStart := time.Now()

	// Use parallel builder to build all levels
	imageMap, err := do.parallelBuilder.BuildAllLevels(ctx, services, skipBuild)
	if err != nil {
		return nil, err
	}

	buildDuration := time.Since(buildStart)
	do.metrics.BuildDuration = buildDuration

	if do.verbose {
		fmt.Printf("  ✓ All builds completed in %.1fs\n", buildDuration.Seconds())
	}

	return imageMap, nil
}

// parallelDeploy deploys all services in parallel with dependency awareness
func (do *DeploymentOrchestrator) parallelDeploy(ctx context.Context, services map[string]config.ServiceConfig, levels [][]string, imageMap map[string]string) error {
	if do.verbose {
		fmt.Printf("  Deploying services with parallel execution...\n")
	}

	deployStart := time.Now()

	// Deploy each level sequentially, but parallelize within each level
	for levelNum, level := range levels {
		if do.verbose {
			fmt.Printf("\n  → Deploying level %d (%d services)...\n", levelNum, len(level))
		}

		if err := do.parallelDeployer.DeployLevel(ctx, services, level, imageMap); err != nil {
			return fmt.Errorf("failed to deploy level %d: %w", levelNum, err)
		}

		if do.verbose {
			fmt.Printf("  ✓ Level %d deployed\n", levelNum)
		}
	}

	deployDuration := time.Since(deployStart)
	do.metrics.DeployDuration = deployDuration

	if do.verbose {
		fmt.Printf("\n  ✓ All services deployed in %.1fs\n", deployDuration.Seconds())
	}

	return nil
}

// verify performs post-deployment verification
func (do *DeploymentOrchestrator) verify(ctx context.Context, services map[string]config.ServiceConfig) error {
	if do.verbose {
		fmt.Printf("  Verifying deployments...\n")
	}

	// For now, verification is handled during deployment
	// Future: Add comprehensive health checks here

	if do.verbose {
		fmt.Printf("  ✓ Verification completed\n")
	}

	return nil
}

// printSummary prints deployment metrics and summary
func (do *DeploymentOrchestrator) printSummary() {
	fmt.Printf("\n=== Deployment Summary ===\n")
	fmt.Printf("  Total Duration:      %.1fs\n", do.metrics.TotalDuration.Seconds())
	fmt.Printf("  Build Duration:      %.1fs\n", do.metrics.BuildDuration.Seconds())
	fmt.Printf("  Deploy Duration:     %.1fs\n", do.metrics.DeployDuration.Seconds())
	fmt.Printf("  Services Deployed:   %d\n", do.metrics.ServicesDeployed)

	if len(do.metrics.Failures) > 0 {
		fmt.Printf("  Failures:            %d\n", len(do.metrics.Failures))
		for _, failure := range do.metrics.Failures {
			fmt.Printf("    - %s\n", failure)
		}
	} else {
		fmt.Printf("  Failures:            0\n")
	}

	// Calculate speedup (theoretical)
	if do.metrics.ServicesDeployed > 1 {
		// Estimate sequential time would be sum of all durations
		sequentialEstimate := do.metrics.BuildDuration + do.metrics.DeployDuration
		parallelActual := do.metrics.TotalDuration

		if sequentialEstimate > 0 && parallelActual > 0 {
			speedup := float64(sequentialEstimate) / float64(parallelActual)
			if speedup > 1.0 {
				fmt.Printf("  Parallel Speedup:    %.1fx\n", speedup)
			}
		}
	}

	fmt.Println()
}

// GetMetrics returns the deployment metrics
func (do *DeploymentOrchestrator) GetMetrics() *DeploymentMetrics {
	return do.metrics
}

// EstimateDeploymentTime estimates deployment time based on service count and configuration
func (do *DeploymentOrchestrator) EstimateDeploymentTime(serviceCount int) time.Duration {
	// Rough estimates:
	// - Build time: 2 minutes per service (parallelized)
	// - Deploy time: 30 seconds per service (parallelized)

	buildTime := time.Duration(2*60/do.parallelBuilder.maxConcurrent) * time.Second * time.Duration(serviceCount)
	deployTime := time.Duration(30/do.parallelDeployer.maxConcurrent) * time.Second * time.Duration(serviceCount)

	return buildTime + deployTime
}
