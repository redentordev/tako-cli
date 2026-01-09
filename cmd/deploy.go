package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/acmedns"
	"github.com/redentordev/tako-cli/pkg/cleanup"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/dependency"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/network"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/registry"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/ssl"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/spf13/cobra"
)

var (
	deployServer  string
	deployService string
	skipBuild     bool
	skipHooks     bool
	deployYes     bool
	commitMessage string
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
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "Skip confirmation prompts (non-interactive mode)")
	deployCmd.Flags().StringVarP(&commitMessage, "message", "m", "", "Commit message for uncommitted changes")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Acquire state lock to prevent concurrent deployments
	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("deploy")
	if err != nil {
		return fmt.Errorf("cannot deploy: %w", err)
	}
	defer stateLock.Release(lockInfo)

	if verbose {
		fmt.Printf("→ Acquired deployment lock (ID: %s)\n", lockInfo.ID)
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

		// Get commit message - from flag, prompt, or auto-generate
		var commitMsg string
		if commitMessage != "" {
			// Use provided commit message
			commitMsg = commitMessage
		} else if deployYes || isNonInteractive() {
			// Auto-generate commit message in non-interactive mode
			commitMsg = fmt.Sprintf("Deploy: %s", time.Now().Format("2006-01-02 15:04:05"))
			fmt.Printf("\n→ Auto-committing changes with message: %q\n", commitMsg)
		} else {
			// Prompt for commit message
			commitMsg, err = git.PromptCommitMessage()
			if err != nil {
				return fmt.Errorf("deployment cancelled: %w", err)
			}
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
	firstClient, err := sshPool.GetOrCreateWithAuth(firstServer.Host, firstServer.Port, firstServer.User, firstServer.SSHKey, firstServer.Password)
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

	// === AUTO-SYNC STATE ===
	// If local .tako directory doesn't exist but remote state does,
	// automatically sync from remote to help users who cloned the project
	if err := SyncStateOnDeploy(cfg, firstClient, envName); err != nil {
		if verbose {
			fmt.Printf("Warning: auto-sync failed: %v\n", err)
		}
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

	// Detect drift (manual changes) in running services
	var driftWarnings []string
	for serviceName, actual := range actualState {
		if svc, exists := services[serviceName]; exists {
			fullServiceName := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, serviceName)
			details, err := reconcile.GatherActualServiceDetails(firstClient, fullServiceName)
			if err == nil {
				drifts := reconcile.DetectDrift(details, &svc)
				for _, drift := range drifts {
					if drift.IsManual {
						var msg string
						switch drift.Field {
						case "env":
							if drift.DesiredValue == "" {
								msg = fmt.Sprintf("  %s: env %s (manually added, will be removed)", serviceName, drift.Key)
							} else {
								msg = fmt.Sprintf("  %s: env %s (manually changed, will be overwritten)", serviceName, drift.Key)
							}
						case "volume":
							msg = fmt.Sprintf("  %s: volume %s (manually added, will be removed)", serviceName, drift.Key)
						case "label":
							msg = fmt.Sprintf("  %s: label %s (manually added, will be removed)", serviceName, drift.Key)
						case "replicas":
							msg = fmt.Sprintf("  %s: replicas %s → %s (manually changed, will be overwritten)", serviceName, drift.ActualValue, drift.DesiredValue)
						}
						if msg != "" {
							driftWarnings = append(driftWarnings, msg)
						}
					}
				}
			}
			_ = actual // Silence unused warning
		}
	}

	// Show drift warnings
	if len(driftWarnings) > 0 {
		fmt.Println("\n⚠ Manual changes detected - will be overwritten:")
		for _, warning := range driftWarnings {
			fmt.Println(warning)
		}
		fmt.Println()

		// In non-interactive mode, just warn and proceed
		if !deployYes && !isNonInteractive() {
			fmt.Printf("Proceed with deployment? (y/N): ")
			var response string
			fmt.Scanln(&response)
			if response != "y" && response != "Y" && response != "yes" {
				fmt.Println("Deployment cancelled")
				return nil
			}
		} else {
			fmt.Println("(Non-interactive mode: proceeding with deployment)")
		}
	} else if plan.NeedsConfirmation() && !deployYes && !isNonInteractive() {
		// Ask for confirmation if there are destructive changes
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

	// === WILDCARD SSL DETECTION ===
	// Check for wildcard domains and setup acme-dns if needed
	sslReqs := ssl.DetectRequirements(services)
	if ssl.HasWildcards(sslReqs) {
		wildcardDomains := ssl.GroupWildcards(sslReqs)
		fmt.Printf("\n🔐 Wildcard SSL certificates detected:\n")
		for _, domain := range wildcardDomains {
			fmt.Printf("   *.%s\n", domain)
		}

		// Setup acme-dns for DNS-01 challenge
		acmeMgr := acmedns.NewManager(firstClient, firstServer.Host, verbose)
		if err := acmeMgr.Setup(); err != nil {
			fmt.Printf("\n⚠ Warning: Failed to setup acme-dns: %v\n", err)
			fmt.Printf("  Wildcard SSL certificates may not be issued automatically.\n")
			fmt.Printf("  You may need to configure DNS-01 challenge manually.\n\n")
		} else {
			// Register each wildcard domain
			var registrations []*acmedns.Registration
			for _, baseDomain := range wildcardDomains {
				reg, err := acmeMgr.Register(baseDomain)
				if err != nil {
					fmt.Printf("  ⚠ Failed to register %s: %v\n", baseDomain, err)
					continue
				}
				registrations = append(registrations, reg)
			}

			// Show CNAME instructions if we have new registrations
			if len(registrations) > 0 {
				fmt.Print(acmeMgr.GetCNAMEInstructions(registrations))
				fmt.Printf("\n⚠ IMPORTANT: Add the CNAME records above to your DNS provider.\n")
				fmt.Printf("  Wildcard certificates will be issued automatically once DNS propagates.\n")
				fmt.Printf("  You can check status with: tako ssl status\n\n")

				// Check if DNS is already configured (for returning users)
				dnsChecker := ssl.NewDNSChecker()
				allConfigured := true
				for _, reg := range registrations {
					verified, _ := dnsChecker.CheckCNAME(reg.Domain, reg.CNAMETarget)
					if verified {
						fmt.Printf("  ✓ DNS already configured for *.%s\n", reg.Domain)
					} else {
						allConfigured = false
					}
				}

				if !allConfigured && !deployYes && !isNonInteractive() {
					fmt.Printf("\nDNS records not yet configured. Continue deployment anyway? (y/N): ")
					var response string
					fmt.Scanln(&response)
					if response != "y" && response != "Y" && response != "yes" {
						fmt.Println("Deployment paused. Add DNS records and run deploy again.")
						return nil
					}
				}
			}
		}
	}

	// Always use Swarm mode for consistent deployment model
	// Benefits: unified code path, seamless scaling, built-in rolling updates
	if len(servers) == 1 {
		fmt.Printf("\n🐝 Using Docker Swarm mode (single-server)\n\n")
	} else {
		fmt.Printf("\n🐝 Using Docker Swarm mode (multi-server cluster)\n\n")
	}

	// Log deployment start
	if localStateMgr != nil {
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
		CLIVersion:     Version,
		CLICommit:      GitCommit,
	}

	// Setup notifications if configured
	var notifier *notification.Notifier
	if cfg.Notifications != nil && (cfg.Notifications.Slack != "" || cfg.Notifications.Discord != "" || cfg.Notifications.Webhook != "") {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			SlackWebhook:   cfg.Notifications.Slack,
			DiscordWebhook: cfg.Notifications.Discord,
			Webhook:        cfg.Notifications.Webhook,
		}, verbose)

		// Send deployment started notification
		if err := notifier.Notify(notification.Event{
			Type:        notification.EventDeployStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Starting deployment of `%s` v%s to `%s`\nCommit: `%s` - %s", cfg.Project.Name, cfg.Project.Version, envName, commitInfo.ShortHash, commitInfo.Message),
			Details: map[string]string{
				"version":   cfg.Project.Version,
				"commit":    commitInfo.ShortHash,
				"branch":    commitInfo.Branch,
				"author":    commitInfo.Author,
				"user":      remotestate.GetCurrentUser(),
				"services":  fmt.Sprintf("%d", len(services)),
			},
		}); err != nil && verbose {
			fmt.Printf("  Warning: failed to send start notification: %v\n", err)
		}
	}

	deploymentFailed := false
	var deploymentError error

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
				deploymentError = fmt.Errorf("build failed for %s: %w", serviceName, err)
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
			deploymentError = fmt.Errorf("swarm deployment failed for %s: %w", serviceName, err)
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

	// Calculate deployment duration
	deploymentDuration := time.Since(startTime)

	if deploymentFailed {
		// Send failure notification
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Message:     fmt.Sprintf("Deployment of `%s` to `%s` failed after %s", cfg.Project.Name, envName, deploymentDuration.Round(time.Second)),
				Error:       deploymentError.Error(),
				Duration:    deploymentDuration,
				Details: map[string]string{
					"version": cfg.Project.Version,
					"commit":  commitInfo.ShortHash,
					"user":    remotestate.GetCurrentUser(),
				},
			})
		}
		return fmt.Errorf("swarm deployment failed")
	}

	fmt.Printf("\n✓ Swarm deployment completed!\n")

	// Automatic cleanup after successful deployment (per-service hooks now handled by deployer)
	if verbose {
		fmt.Printf("\n→ Running automatic cleanup...\n")
	}
	for serverName, server := range servers {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
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

	// Send success notification
	if notifier != nil {
		// Collect deployed service URLs
		var urls []string
		for _, svc := range services {
			if svc.Proxy != nil {
				for _, domain := range svc.Proxy.GetAllDomains() {
					urls = append(urls, fmt.Sprintf("https://%s", domain))
				}
			}
		}

		notifier.Notify(notification.Event{
			Type:        notification.EventDeploySucceeded,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Successfully deployed `%s` v%s to `%s` in %s", cfg.Project.Name, cfg.Project.Version, envName, deploymentDuration.Round(time.Second)),
			Duration:    deploymentDuration,
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   commitInfo.ShortHash,
				"branch":   commitInfo.Branch,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
				"urls":     fmt.Sprintf("%v", urls),
			},
		})
	}

	// Show service URLs (iterate through services with proxy configured)
	hasPublicServices := false
	servicesWithProxy := []struct {
		name    string
		domains []string
	}{}

	for serviceName, service := range services {
		if service.Proxy != nil && service.Proxy.GetPrimaryDomain() != "" {
			allDomains := service.Proxy.GetAllDomains()
			if !hasPublicServices {
				fmt.Printf("Your application is available at:\n")
				hasPublicServices = true
			}
			fmt.Printf("\n%s:\n", serviceName)
			for _, domain := range allDomains {
				fmt.Printf("  https://%s\n", domain)
			}
			servicesWithProxy = append(servicesWithProxy, struct {
				name    string
				domains []string
			}{serviceName, allDomains})
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
