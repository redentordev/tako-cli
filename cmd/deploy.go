package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/cleanup"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/dependency"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/network"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/registry"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/spf13/cobra"
)

var (
	deployServer  string
	deployService string
	skipBuild     bool
	skipHooks     bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy your application to configured servers",
	Long: `Deploy your application using a zero-downtime blue-green deployment strategy.

The deployment process:
  1. Run pre-deploy hooks (tests, builds)
  2. Build Docker image
  3. Push image to registry
  4. Deploy new version alongside current version (blue-green)
  5. Run health checks
  6. Switch traffic to new version
  7. Remove old version
  8. Run post-deploy hooks (migrations, etc.)

If any step fails, the deployment is automatically rolled back.`,
	RunE: runDeploy,
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().StringVarP(&deployServer, "server", "s", "", "Deploy to specific server")
	deployCmd.Flags().StringVar(&deployService, "service", "", "Deploy specific service")
	deployCmd.Flags().BoolVar(&skipBuild, "skip-build", false, "Skip building Docker image")
	deployCmd.Flags().BoolVar(&skipHooks, "skip-hooks", false, "Skip pre/post deploy hooks")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Initialize Git client
	gitClient := git.NewClient(".")

	// Check if project is a Git repository
	if !gitClient.IsRepository() {
		return fmt.Errorf("❌ This project is not a Git repository.\n\nPlease initialize Git first:\n  git init\n  git add .\n  git commit -m \"Initial commit\"\n\nGit is required for deployment tracking and rollback functionality.")
	}

	// Check for uncommitted changes
	hasChanges, err := gitClient.HasUncommittedChanges()
	if err != nil {
		return fmt.Errorf("failed to check git status: %w", err)
	}

	var commitInfo *git.CommitInfo

	if hasChanges {
		// Show uncommitted changes
		status, err := gitClient.GetStatus()
		if err == nil && verbose {
			fmt.Printf("\n⚠️  Uncommitted changes detected:\n%s\n", status)
		}

		// Prompt for commit message
		commitMsg, err := git.PromptCommitMessage()
		if err != nil {
			return fmt.Errorf("deployment cancelled: %w", err)
		}

		fmt.Printf("\n→ Creating commit...\n")

		// Stage all changes
		if err := gitClient.AddAll(); err != nil {
			return fmt.Errorf("failed to stage changes: %w", err)
		}

		// Create commit
		if err := gitClient.Commit(commitMsg); err != nil {
			return fmt.Errorf("failed to create commit: %w", err)
		}

		// Get commit info
		commitInfo, err = gitClient.GetCommitInfo("")
		if err != nil {
			return fmt.Errorf("failed to get commit info: %w", err)
		}

		fmt.Printf("  ✓ Created commit: %s\n", commitInfo.ShortHash)
	} else {
		// No uncommitted changes, get current commit info
		commitInfo, err = gitClient.GetCommitInfo("")
		if err != nil {
			return fmt.Errorf("failed to get commit info: %w", err)
		}
	}

	// Display commit info
	fmt.Printf("\n📦 Deploying commit:\n")
	fmt.Printf("  Hash:    %s\n", commitInfo.ShortHash)
	fmt.Printf("  Branch:  %s\n", commitInfo.Branch)
	fmt.Printf("  Author:  %s\n", commitInfo.Author)
	fmt.Printf("  Message: %s\n", commitInfo.Message)

	// Get environment and services
	envName := getEnvironmentName(cfg)
	allServices, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Determine which servers to deploy to
	servers := cfg.Servers
	if deployServer != "" {
		server, exists := cfg.Servers[deployServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", deployServer)
		}
		servers = map[string]config.ServerConfig{deployServer: server}
	}

	// Determine which services to deploy
	services := allServices
	if deployService != "" {
		service, exists := allServices[deployService]
		if !exists {
			return fmt.Errorf("service %s not found in environment %s", deployService, envName)
		}
		services = map[string]config.ServiceConfig{deployService: service}
	}

	fmt.Printf("\n=== Starting deployment ===\n\n")
	fmt.Printf("Project: %s v%s\n", cfg.Project.Name, cfg.Project.Version)
	fmt.Printf("Environment: %s\n", envName)
	fmt.Printf("Servers: %d\n", len(servers))
	fmt.Printf("Services: %d\n\n", len(services))

	// Get first server for initial operations
	firstServerName := ""
	var firstServer config.ServerConfig
	for name, srv := range servers {
		firstServerName = name
		firstServer = srv
		break
	}

	if firstServerName == "" {
		return fmt.Errorf("no servers configured")
	}

	// Get or create SSH client for first server
	firstClient, err := sshPool.GetOrCreate(firstServer.Host, firstServer.Port, firstServer.User, firstServer.SSHKey)
	if err != nil {
		return fmt.Errorf("failed to connect to server %s: %w", firstServerName, err)
	}

	// Create deployer with pool for Swarm support
	deploy := deployer.NewDeployerWithPool(firstClient, cfg, envName, sshPool, verbose)

	// Always setup Swarm cluster (works for single or multi-server)
	// This provides consistent deployment model and easy scaling
	if err := deploy.SetupSwarmCluster(); err != nil {
		return fmt.Errorf("failed to setup swarm cluster: %w", err)
	}

	// === STATE RECONCILIATION ===
	// Compare desired state (config) with actual state (running services)
	// This ensures we properly handle service removals and updates

	if verbose {
		fmt.Printf("\n→ Computing deployment plan...\n")
	}

	// Initialize state manager to track deployments
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize local state: %v\n", err)
		}
		localStateMgr = nil // Continue without state management
	}

	// Gather actual state from running services
	actualState, err := reconcile.GatherActualState(firstClient, cfg.Project.Name, envName, localStateMgr)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to gather actual state: %v\n", err)
		}
		actualState = make(map[string]*reconcile.ActualService) // Continue with empty state
	}

	if verbose && len(actualState) > 0 {
		fmt.Printf("  Found %d running service(s)\n", len(actualState))
	}

	// Compute reconciliation plan
	plan := reconcile.ComputePlan(cfg.Project.Name, envName, services, actualState)

	// Show plan to user
	fmt.Println()
	fmt.Print(plan.FormatPlan())

	// Ask for confirmation if there are destructive changes
	if plan.NeedsConfirmation() && !isNonInteractive() {
		fmt.Printf("\nProceed with deployment? (y/N): ")
		var response string
		fmt.Scanln(&response)
		if response != "y" && response != "Y" && response != "yes" {
			fmt.Println("Deployment cancelled")
			return nil
		}
	}

	if plan.IsEmpty() {
		fmt.Println("\n✓ All services are up-to-date. Nothing to deploy.")
		return nil
	}

	// Pre-deployment network verification
	if verbose {
		fmt.Printf("\n→ Verifying network configuration...\n")
	}
	if err := deploy.VerifyNetworkSetup(); err != nil {
		if verbose {
			fmt.Printf("  Warning: Network verification failed: %v\n", err)
		}
	} else if verbose {
		fmt.Printf("  ✓ Network configuration verified\n")
	}

	// Check if using Swarm mode
	useSwarmMode := deploy.IsSwarmMode()

	if useSwarmMode {
		// Swarm mode: Deploy services once to the cluster
		if len(servers) == 1 {
			fmt.Printf("\n🐝 Using Docker Swarm mode (single-server)\n\n")
		} else {
			fmt.Printf("\n🐝 Using Docker Swarm mode (multi-server cluster)\n\n")
		}

		// Initialize local state manager
		localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
		if err != nil && verbose {
			fmt.Printf("Warning: failed to initialize local state: %v\n", err)
		} else if localStateMgr != nil {
			// Log deployment start
			localStateMgr.LogDeployment(fmt.Sprintf("Starting Swarm deployment to %s", envName))
			localStateMgr.LogDeployment(fmt.Sprintf("Git commit: %s", commitInfo.ShortHash))
		}

		// Deploy to manager node only
		stateManager := remotestate.NewStateManager(firstClient, cfg.Project.Name, firstServer.Host)
		if err := stateManager.Initialize(); err != nil {
			if verbose {
				fmt.Printf("Warning: failed to initialize state directory: %v\n", err)
			}
		}

		startTime := time.Now()
		deployment := &remotestate.DeploymentState{
			Timestamp:      startTime,
			ProjectName:    cfg.Project.Name,
			Version:        cfg.Project.Version,
			Status:         remotestate.StatusInProgress,
			Services:       make(map[string]remotestate.ServiceState),
			User:           remotestate.GetCurrentUser(),
			Host:           firstServer.Host,
			GitCommit:      commitInfo.Hash,
			GitCommitShort: commitInfo.ShortHash,
			GitBranch:      commitInfo.Branch,
			GitCommitMsg:   commitInfo.Message,
			GitAuthor:      commitInfo.Author,
		}

		deploymentFailed := false

		// Resolve service deployment order based on dependencies
		resolver := dependency.NewResolver(services, verbose)

		// Optionally infer dependencies from environment variables
		inferredDeps := resolver.InferDependencies()
		resolver.MergeDependencies(inferredDeps)

		// Get deployment order
		deploymentOrder, err := resolver.ResolveOrder()
		if err != nil {
			return fmt.Errorf("failed to resolve service dependencies: %w", err)
		}

		// Deploy each service to Swarm in dependency order
		for _, serviceName := range deploymentOrder {
			service := services[serviceName]
			fmt.Printf("→ Deploying service: %s\n", serviceName)

			// Get full image name
			fullImageName := cfg.GetFullImageName(serviceName, envName)

			// Build image if needed (but don't deploy with docker run)
			if !skipBuild && service.Build != "" {
				// Build the image on manager node
				builtImageName, err := deploy.BuildImage(serviceName, &service)
				if err != nil {
					fmt.Printf("  ✗ Build failed: %v\n", err)
					deploymentFailed = true
					deployment.Status = remotestate.StatusFailed
					deployment.Error = err.Error()
					break
				}
				// Use the built image name
				fullImageName = builtImageName
			} else if service.Image != "" {
				// Use pre-built image
				fullImageName = service.Image
			}

			// Always deploy to Swarm using docker service create
			if err := deploy.DeployServiceSwarm(serviceName, &service, fullImageName); err != nil {
				fmt.Printf("  ✗ Swarm deployment failed: %v\n", err)
				deploymentFailed = true
				deployment.Status = remotestate.StatusFailed
				deployment.Error = err.Error()
				break
			}

			fmt.Printf("  ✓ Service %s deployed to swarm\n", serviceName)

			// Save service state
			deployment.Services[serviceName] = remotestate.ServiceState{
				Name:     serviceName,
				Image:    fullImageName,
				Port:     service.Port,
				Replicas: service.Replicas,
				Env:      service.Env,
			}
		}

		if !deploymentFailed {
			deployment.Status = remotestate.StatusSuccess
			deployment.Duration = time.Since(startTime)
			if err := stateManager.SaveDeployment(deployment); err != nil && verbose {
				fmt.Printf("Warning: failed to save remote deployment state: %v\n", err)
			}

			// Save local deployment state
			if localStateMgr != nil {
				localDeployment := &localstate.DeploymentState{
					DeploymentID:    fmt.Sprintf("deploy-%s", time.Now().Format("20060102-150405")),
					Timestamp:       startTime,
					Environment:     envName,
					Mode:            "swarm",
					Status:          "success",
					DurationSeconds: int(time.Since(startTime).Seconds()),
					GitCommit:       commitInfo.Hash,
					TriggeredBy:     remotestate.GetCurrentUser(),
					Notes:           fmt.Sprintf("Deployed %d services to swarm", len(services)),
				}
				if err := localStateMgr.SaveDeployment(localDeployment); err != nil && verbose {
					fmt.Printf("Warning: failed to save local deployment state: %v\n", err)
				}
			}

			// Register project in registry
			networkMgr := network.NewManager(firstClient, cfg.Project.Name, envName, verbose)
			reg := registry.NewRegistry(firstClient, verbose)
			projectInfo := registry.ProjectInfo{
				Name:        cfg.Project.Name,
				Environment: envName,
				Network:     networkMgr.GetNetworkName(),
				Services:    deploymentOrder,
				Domains:     extractDomains(services),
				DeployedAt:  time.Now(),
			}
			if err := reg.RegisterProject(projectInfo); err != nil && verbose {
				fmt.Printf("Warning: failed to register project: %v\n", err)
			}
		}

		if deploymentFailed {
			return fmt.Errorf("swarm deployment failed")
		}

		fmt.Printf("\n✓ Swarm deployment completed!\n")

	} else {
		// Single server mode: Deploy to each server individually
		for serverName, server := range servers {
			fmt.Printf("\n=== Deploying to server: %s (%s) ===\n\n", serverName, server.Host)

			// Get or create SSH client
			client, err := sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
			if err != nil {
				return fmt.Errorf("failed to connect to server %s: %w", serverName, err)
			}

			// Initialize local state manager (once per environment)
			localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
			if err != nil && verbose {
				fmt.Printf("Warning: failed to initialize local state: %v\n", err)
			}

			// Create remote state manager
			stateManager := remotestate.NewStateManager(client, cfg.Project.Name, server.Host)

			// Initialize state directory on server
			if err := stateManager.Initialize(); err != nil {
				if verbose {
					fmt.Printf("Warning: failed to initialize state directory: %v\n", err)
				}
			}

			// Create deployment state
			startTime := time.Now()
			deployment := &remotestate.DeploymentState{
				Timestamp:      startTime,
				ProjectName:    cfg.Project.Name,
				Version:        cfg.Project.Version,
				Status:         remotestate.StatusInProgress,
				Services:       make(map[string]remotestate.ServiceState),
				User:           remotestate.GetCurrentUser(),
				Host:           server.Host,
				GitCommit:      commitInfo.Hash,
				GitCommitShort: commitInfo.ShortHash,
				GitBranch:      commitInfo.Branch,
				GitCommitMsg:   commitInfo.Message,
				GitAuthor:      commitInfo.Author,
			}

			// Track deployment success
			deploymentFailed := false

			// Resolve service deployment order based on dependencies
			resolver := dependency.NewResolver(services, verbose)

			// Optionally infer dependencies from environment variables
			inferredDeps := resolver.InferDependencies()
			resolver.MergeDependencies(inferredDeps)

			// Check if parallel deployment is enabled
			if cfg.IsParallelDeployment() {
				// Use parallel orchestrator
				if verbose {
					fmt.Printf("Using parallel deployment strategy\n\n")
				}

				orchestrator := deployer.NewDeploymentOrchestrator(deploy, deployer.OrchestratorConfig{
					MaxConcurrentBuilds:  cfg.GetMaxConcurrentBuilds(),
					MaxConcurrentDeploys: cfg.GetMaxConcurrentDeploys(),
					EnableCache:          cfg.IsCacheEnabled(),
					BuildTimeout:         10 * time.Minute,
					DeployTimeout:        5 * time.Minute,
				})

				// Deploy using orchestrator
				ctx := context.Background()
				if err := orchestrator.Deploy(ctx, services, skipBuild); err != nil {
					fmt.Printf("  ✗ Parallel deployment failed: %v\n", err)
					deploymentFailed = true
					deployment.Status = remotestate.StatusFailed
					deployment.Error = err.Error()
					deployment.Duration = time.Since(startTime)
					if saveErr := stateManager.SaveDeployment(deployment); saveErr != nil && verbose {
						fmt.Printf("Warning: failed to save deployment state: %v\n", saveErr)
					}
					return fmt.Errorf("parallel deployment failed: %w", err)
				}

				// Get metrics from orchestrator
				metrics := orchestrator.GetMetrics()

				// Update deployment record
				deployment.Status = remotestate.StatusSuccess
				deployment.Duration = metrics.TotalDuration

				// Save deployment state (services already deployed by orchestrator)
				if err := stateManager.SaveDeployment(deployment); err != nil && verbose {
					fmt.Printf("Warning: failed to save deployment state: %v\n", err)
				}

			} else {
				// Use sequential deployment (original logic)
				if verbose {
					fmt.Printf("Using sequential deployment strategy\n\n")
				}

				// Get deployment order
				deploymentOrder, err := resolver.ResolveOrder()
				if err != nil {
					return fmt.Errorf("failed to resolve service dependencies: %w", err)
				}

				// Deploy each service in dependency order
				for _, serviceName := range deploymentOrder {
					service := services[serviceName]
					fmt.Printf("→ Deploying service: %s\n", serviceName)

					if err := deploy.DeployService(serviceName, &service, skipBuild); err != nil {
						fmt.Printf("  ✗ Deployment failed: %v\n", err)
						deploymentFailed = true

						// Save failed state
						deployment.Status = remotestate.StatusFailed
						deployment.Error = err.Error()
						deployment.Duration = time.Since(startTime)
						if saveErr := stateManager.SaveDeployment(deployment); saveErr != nil && verbose {
							fmt.Printf("Warning: failed to save deployment state: %v\n", saveErr)
						}

						fmt.Printf("\n→ Rolling back...\n")
						if rbErr := deploy.Rollback(serviceName); rbErr != nil {
							return fmt.Errorf("deployment failed and rollback failed: %w", rbErr)
						}
						fmt.Printf("  ✓ Rolled back successfully\n")
						return fmt.Errorf("deployment failed: %w", err)
					}

					// Get container info for state using correct naming pattern
					// Pattern: {project}_{env}_{service}_{replica}
					containerName := fmt.Sprintf("%s_%s_%s_1", cfg.Project.Name, envName, serviceName)

					// Get actual image name from running container (format: name:tag)
					imageName, _ := client.Execute(fmt.Sprintf("docker inspect -f '{{index .Config.Image}}' %s 2>/dev/null", containerName))
					// Get image ID (SHA)
					imageID, _ := client.Execute(fmt.Sprintf("docker inspect -f '{{.Image}}' %s 2>/dev/null", containerName))
					// Get container ID
					containerID, _ := client.Execute(fmt.Sprintf("docker ps -q -f name=^%s$ 2>/dev/null", containerName))

					// Save service state
					deployment.Services[serviceName] = remotestate.ServiceState{
						Name:        serviceName,
						Image:       strings.TrimSpace(imageName),
						ImageID:     strings.TrimSpace(imageID),
						ContainerID: strings.TrimSpace(containerID),
						Port:        service.Port,
						Replicas:    1,
						Env:         service.Env,
						HealthCheck: remotestate.HealthCheckState{
							Enabled:   service.HealthCheck.Path != "",
							Path:      service.HealthCheck.Path,
							Healthy:   true,
							LastCheck: time.Now(),
						},
					}

					fmt.Printf("  ✓ Service %s deployed successfully\n", serviceName)
				}
			} // End sequential deployment else block

			// Mark deployment as successful
			if !deploymentFailed {
				deployment.Status = remotestate.StatusSuccess
				deployment.Duration = time.Since(startTime)

				// Save successful deployment state
				if err := stateManager.SaveDeployment(deployment); err != nil {
					if verbose {
						fmt.Printf("Warning: failed to save remote deployment state: %v\n", err)
					}
				} else if verbose {
					fmt.Printf("✓ Deployment state saved (ID: %s)\n", deployment.ID)
				}

				// Save local deployment state
				if localStateMgr != nil {
					localDeployment := &localstate.DeploymentState{
						DeploymentID:    fmt.Sprintf("deploy-%s", time.Now().Format("20060102-150405")),
						Timestamp:       startTime,
						Environment:     envName,
						Mode:            "single",
						Status:          "success",
						DurationSeconds: int(time.Since(startTime).Seconds()),
						GitCommit:       commitInfo.Hash,
						TriggeredBy:     remotestate.GetCurrentUser(),
						Notes:           fmt.Sprintf("Deployed %d services to %s", len(services), serverName),
					}
					if err := localStateMgr.SaveDeployment(localDeployment); err != nil && verbose {
						fmt.Printf("Warning: failed to save local deployment state: %v\n", err)
					}
				}

				// Cleanup old deployments
				if err := stateManager.CleanupOldDeployments(); err != nil && verbose {
					fmt.Printf("Warning: failed to cleanup old deployments: %v\n", err)
				}

				// Register project in registry
				networkMgr := network.NewManager(client, cfg.Project.Name, envName, verbose)
				reg := registry.NewRegistry(client, verbose)
				serviceNames := make([]string, 0, len(services))
				for name := range services {
					serviceNames = append(serviceNames, name)
				}
				projectInfo := registry.ProjectInfo{
					Name:        cfg.Project.Name,
					Environment: envName,
					Network:     networkMgr.GetNetworkName(),
					Services:    serviceNames,
					Domains:     extractDomains(services),
					DeployedAt:  time.Now(),
				}
				if err := reg.RegisterProject(projectInfo); err != nil && verbose {
					fmt.Printf("Warning: failed to register project: %v\n", err)
				}
			}

			fmt.Printf("\n✓ Server %s deployment completed!\n", serverName)
		}
	}

	// Automatic cleanup after successful deployment (per-service hooks now handled by deployer)
	if verbose {
		fmt.Printf("\n→ Running automatic cleanup...\n")
	}
	for serverName, server := range servers {
		client, err := sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
		if err == nil {
			cleaner := cleanup.NewCleaner(client, cfg.Project.Name, verbose)
			// Keep 3 latest images, clean stopped containers and dangling images
			cleaner.CleanOldImages(3)
			cleaner.CleanStoppedContainers()
			cleaner.CleanDanglingImages()
			if verbose {
				fmt.Printf("  ✓ Cleaned up %s\n", serverName)
			}
		}
	}

	fmt.Printf("\n✓ Deployment completed successfully!\n\n")

	// Show service URLs (iterate through services with proxy configured)
	hasPublicServices := false
	servicesWithProxy := []struct {
		name    string
		domains []string
	}{}

	for serviceName, service := range services {
		if service.Proxy != nil && len(service.Proxy.Domains) > 0 {
			if !hasPublicServices {
				fmt.Printf("Your application is available at:\n")
				hasPublicServices = true
			}
			fmt.Printf("\n%s:\n", serviceName)
			for _, domain := range service.Proxy.Domains {
				fmt.Printf("  https://%s\n", domain)
			}
			servicesWithProxy = append(servicesWithProxy, struct {
				name    string
				domains []string
			}{serviceName, service.Proxy.Domains})
		}
	}

	// Monitor SSL certificate provisioning if there are public services
	if hasPublicServices && firstClient != nil {
		fmt.Printf("\n")
		healthChecker := health.NewHealthChecker(firstClient)

		for _, svc := range servicesWithProxy {
			for _, domain := range svc.domains {
				// Monitor SSL provisioning (max 2 minutes wait)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := healthChecker.MonitorSSLProvisioning(ctx, svc.name, domain, 2*time.Minute); err != nil {
					if verbose {
						fmt.Printf("\n⚠️  SSL certificate not yet available for %s\n", domain)
						fmt.Printf("   This is normal for first deployment. Certificate will be provisioned automatically.\n")
						fmt.Printf("   You can check status at: http://%s:8080/dashboard/\n", firstServer.Host)
					}
				}
				cancel()
				break // Only check first domain per service
			}
		}
	}

	return nil
}

// isNonInteractive checks if running in non-interactive mode
func isNonInteractive() bool {
	return os.Getenv("TAKO_NONINTERACTIVE") == "1" || os.Getenv("CI") == "true"
}

// extractDomains extracts all domains from service configurations
func extractDomains(services map[string]config.ServiceConfig) []string {
	domains := []string{}
	for _, service := range services {
		if service.Proxy != nil && len(service.Proxy.Domains) > 0 {
			domains = append(domains, service.Proxy.Domains...)
		}
	}
	return domains
}
