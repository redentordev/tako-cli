package swarm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// DeployService deploys a service to the Docker Swarm
func (m *Manager) DeployService(
	managerClient *ssh.Client,
	serviceName string,
	service *config.ServiceConfig,
	imageRef string,
	networkName string,
	traefikLabels []string, // Optional Traefik labels to add during creation
) error {
	fullServiceName := fmt.Sprintf("%s_%s_%s", m.config.Project.Name, m.environment, serviceName)

	if m.verbose {
		fmt.Printf("\n→ Deploying service: %s\n", serviceName)
		fmt.Printf("  Swarm service name: %s\n", fullServiceName)
	}

	// Validate hooks before deployment
	if service.Hooks != nil {
		if err := hooks.ValidateHooks(service.Hooks); err != nil {
			return fmt.Errorf("hook validation failed: %w", err)
		}
	}

	// Create hook executor
	hookExecutor := hooks.NewExecutor(managerClient, m.config.Project.Name, m.environment, serviceName, m.verbose)

	// Execute pre-deploy hooks
	if service.Hooks != nil && len(service.Hooks.PreDeploy) > 0 {
		if err := hookExecutor.ExecutePreDeploy(service.Hooks.PreDeploy, service.Env); err != nil {
			return fmt.Errorf("pre-deploy hooks failed: %w", err)
		}
	}

	// Check if service already exists
	exists, err := m.serviceExists(managerClient, fullServiceName)
	if err != nil {
		return fmt.Errorf("failed to check if service exists: %w", err)
	}

	var deployErr error
	if exists {
		// Update existing service
		deployErr = m.updateService(managerClient, serviceName, service, fullServiceName, imageRef, networkName, traefikLabels)
	} else {
		// Create new service
		deployErr = m.createService(managerClient, serviceName, service, fullServiceName, imageRef, networkName, traefikLabels)
	}

	if deployErr != nil {
		return deployErr
	}

	// Execute post-deploy hooks
	if service.Hooks != nil && len(service.Hooks.PostDeploy) > 0 {
		if err := hookExecutor.ExecutePostDeploy(service.Hooks.PostDeploy, service.Env); err != nil {
			return fmt.Errorf("post-deploy hooks failed: %w", err)
		}
	}

	// Execute post-start hooks (after service is running)
	if service.Hooks != nil && len(service.Hooks.PostStart) > 0 {
		if err := hookExecutor.ExecutePostStart(service.Hooks.PostStart, fullServiceName, service.Env); err != nil {
			return fmt.Errorf("post-start hooks failed: %w", err)
		}
	}

	return nil
}

// getServiceEnvVars retrieves current environment variables from a running service
func (m *Manager) getServiceEnvVars(client *ssh.Client, fullServiceName string) (map[string]string, error) {
	// Get env vars from service spec using docker inspect
	cmd := fmt.Sprintf("docker service inspect %s --format '{{json .Spec.TaskTemplate.ContainerSpec.Env}}'", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect service: %w", err)
	}

	output = strings.TrimSpace(output)
	if output == "" || output == "null" || output == "[]" {
		return make(map[string]string), nil
	}

	// Parse JSON array of env vars (format: ["KEY=value", "KEY2=value2"])
	var envArray []string
	if err := json.Unmarshal([]byte(output), &envArray); err != nil {
		return nil, fmt.Errorf("failed to parse env vars: %w", err)
	}

	// Convert to map
	envMap := make(map[string]string)
	for _, envVar := range envArray {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	return envMap, nil
}

// serviceExists checks if a service already exists in the swarm
func (m *Manager) serviceExists(client *ssh.Client, serviceName string) (bool, error) {
	cmd := fmt.Sprintf("docker service ls --filter name=%s --format '{{.Name}}'", serviceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(output) == serviceName, nil
}

// getServiceLabels retrieves current labels from a running service
func (m *Manager) getServiceLabels(client *ssh.Client, fullServiceName string) (map[string]string, error) {
	// Get labels from service spec using docker inspect
	cmd := fmt.Sprintf("docker service inspect %s --format '{{json .Spec.Labels}}'", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect service labels: %w", err)
	}

	output = strings.TrimSpace(output)
	if output == "" || output == "null" || output == "{}" {
		return make(map[string]string), nil
	}

	// Parse JSON object of labels
	var labels map[string]string
	if err := json.Unmarshal([]byte(output), &labels); err != nil {
		return nil, fmt.Errorf("failed to parse labels: %w", err)
	}

	return labels, nil
}

// createService creates a new Docker service
func (m *Manager) createService(
	client *ssh.Client,
	serviceName string,
	service *config.ServiceConfig,
	fullServiceName string,
	imageRef string,
	networkName string,
	traefikLabels []string,
) error {
	if m.verbose {
		fmt.Printf("  Creating new service...\n")
	}

	// Build docker service create command
	cmd := fmt.Sprintf("docker service create --detach --name %s", fullServiceName)

	// Add Traefik labels if provided (avoids service update disruptions)
	if len(traefikLabels) > 0 {
		for _, label := range traefikLabels {
			cmd += fmt.Sprintf(" %s", label)
		}
		if m.verbose {
			fmt.Printf("  Adding Traefik labels during creation (zero-disruption)\n")
		}
	}

	// Add replicas or global mode
	// Check if service should run in global mode (one instance per node)
	isGlobalMode := service.Placement != nil && service.Placement.Strategy == "global"

	if isGlobalMode {
		cmd += " --mode global"
	} else {
		replicas := service.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		cmd += fmt.Sprintf(" --replicas %d", replicas)
	}

	// Add network with alias for service discovery
	// This allows services to reference each other by short name (e.g., "postgres" instead of "web-db_production_postgres")
	cmd += fmt.Sprintf(" --network name=%s,alias=%s", networkName, serviceName)

	// Always use env file for environment variables to prevent secrets in command lines
	// This applies even if no explicit secrets are defined, as env vars may contain sensitive data
	var envFilePath string
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || len(service.DockerSecrets) > 0

	if hasEnvVars {
		// Create secrets manager
		secretsMgr, err := secrets.NewManager(m.environment)
		if err != nil {
			return fmt.Errorf("failed to create secrets manager: %w", err)
		}

		// Create env file with all environment variables and secrets
		envFile, err := secretsMgr.CreateEnvFile(service)
		if err != nil {
			return fmt.Errorf("failed to create env file: %w", err)
		}

		// Get the remote path for the env file
		envFilePath = envFile.GetPath(m.config.Project.Name, serviceName)

		// Upload env file to server with secure permissions (0600)
		if err := client.UploadReader(envFile.ToReader(), envFilePath, 0600); err != nil {
			return fmt.Errorf("failed to upload env file: %w", err)
		}

		if m.verbose {
			totalVars := envFile.Count()
			keys := envFile.GetKeys()
			fmt.Printf("  ✓ Env file created with %d variables\n", totalVars)
			if len(keys) > 0 {
				fmt.Printf("  Variables: %s\n", strings.Join(keys, ", "))
			}
		}

		// Add --env-file flag to use the uploaded file
		cmd += fmt.Sprintf(" --env-file %s", envFilePath)

		// Schedule cleanup after service is created
		defer func() {
			cleanupCmd := fmt.Sprintf("rm -f %s", envFilePath)
			if _, err := client.Execute(cleanupCmd); err != nil && m.verbose {
				fmt.Printf("  Warning: Failed to cleanup env file: %v\n", err)
			}
		}()
	}

	// Publish ports directly when:
	// 1. No Traefik/domains configured (traefikLabels is empty)
	// 2. Service has a port specified
	// When domains are configured, Traefik handles routing and we don't publish ports directly
	if len(traefikLabels) == 0 && service.Port > 0 {
		// Publish port directly for external access
		cmd += fmt.Sprintf(" --publish published=%d,target=%d,mode=ingress", service.Port, service.Port)
		if m.verbose {
			fmt.Printf("  Publishing port %d (no domain configured)\n", service.Port)
		}
	}

	// Add volumes (with project and environment scoping)
	for _, volume := range service.Volumes {
		// Check if this is an NFS volume (nfs:export_name:/container/path format)
		if config.IsNFSVolume(volume) {
			exportName, containerPath, readOnly, err := config.ParseNFSVolumeSpec(volume)
			if err != nil {
				return fmt.Errorf("invalid NFS volume spec for service %s: %w", serviceName, err)
			}

			// Determine mount source based on deployment mode
			// Multi-server: Use NFS mount point
			// Single-server: Fall back to direct path from NFS export config
			var mountSource string
			isMultiServer := m.config.IsMultiServer()

			if isMultiServer {
				// Multi-server: Use NFS mount point
				mountSource = fmt.Sprintf("/mnt/tako-nfs/%s_%s_%s", m.config.Project.Name, m.environment, exportName)
			} else {
				// Single-server: Use direct path from NFS export config (no NFS overhead)
				// Look up the export path from config
				export, err := m.config.GetNFSExport(exportName)
				if err != nil {
					return fmt.Errorf("NFS export '%s' not found in config for service %s: %w", exportName, serviceName, err)
				}
				mountSource = export.Path

				// Ensure directory exists on single server
				if m.verbose {
					fmt.Printf("  Single-server mode: using direct path %s instead of NFS\n", mountSource)
				}
			}

			// Use bind mount from source to container
			mountOpts := fmt.Sprintf("type=bind,source=%s,target=%s", mountSource, containerPath)
			if readOnly {
				mountOpts += ",readonly"
			}
			cmd += fmt.Sprintf(" --mount %s", mountOpts)

			if m.verbose {
				accessMode := "rw"
				if readOnly {
					accessMode = "ro"
				}
				if isMultiServer {
					fmt.Printf("  NFS volume: %s -> %s (%s)\n", exportName, containerPath, accessMode)
				} else {
					fmt.Printf("  Local volume (NFS fallback): %s -> %s (%s)\n", mountSource, containerPath, accessMode)
				}
			}
			continue
		}

		source, target := m.parseVolumeSpec(volume)
		if target == "" {
			// If only one path specified, it's the target (container path)
			// Create a named volume with project/environment prefix
			target = source
			source = fmt.Sprintf("%s_%s_%s", m.config.Project.Name, m.environment, strings.ReplaceAll(target, "/", "_"))
			cmd += fmt.Sprintf(" --mount type=volume,source=%s,target=%s", source, target)
		} else {
			// Both source and target specified
			// Check if source is an absolute path (bind mount) or volume name (named volume)
			if strings.HasPrefix(source, "/") || strings.HasPrefix(source, "\\") || (len(source) > 1 && source[1] == ':') {
				// Absolute path = bind mount - validate for security
				if err := validateBindMountPath(source); err != nil {
					return fmt.Errorf("invalid bind mount for service %s: %w", serviceName, err)
				}
				cmd += fmt.Sprintf(" --mount type=bind,source=%s,target=%s", source, target)
			} else {
				// Relative name = named volume (add project/environment prefix)
				namedVolume := fmt.Sprintf("%s_%s_%s", m.config.Project.Name, m.environment, source)
				cmd += fmt.Sprintf(" --mount type=volume,source=%s,target=%s", namedVolume, target)
			}
		}
	}

	// Add placement constraints
	if service.Placement != nil {
		placementArgs, err := m.buildPlacementConstraints(service.Placement)
		if err != nil {
			return fmt.Errorf("failed to build placement constraints: %w", err)
		}
		cmd += placementArgs
	}

	// Add restart policy
	restartPolicy := service.Restart
	if restartPolicy == "" {
		restartPolicy = "unless-stopped"
	}
	// Map Docker Compose restart policies to Swarm restart conditions
	restartCondition := "any"
	switch restartPolicy {
	case "no":
		restartCondition = "none"
	case "on-failure":
		restartCondition = "on-failure"
	case "always", "unless-stopped":
		restartCondition = "any"
	}
	cmd += fmt.Sprintf(" --restart-condition %s", restartCondition)

	// Add Docker health check if configured
	cmd += m.buildHealthCheckFlags(service)

	// Add update config for rolling updates
	cmd += " --update-parallelism 1 --update-delay 10s --update-failure-action rollback"

	// Add rollback config
	cmd += " --rollback-parallelism 1 --rollback-delay 5s"

	// Add image
	cmd += fmt.Sprintf(" %s", imageRef)

	// Add command if specified
	if service.Command != "" {
		cmd += fmt.Sprintf(" %s", service.Command)
	}

	// Redirect stderr to stdout and ensure command returns immediately
	cmd += " 2>&1"

	if m.verbose {
		fmt.Printf("  Command: %s\n", cmd)
	}

	output, err := client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to create service: %w, output: %s", err, output)
	}

	if m.verbose && strings.TrimSpace(output) != "" {
		fmt.Printf("  Output: %s\n", strings.TrimSpace(output))
	}

	if m.verbose {
		fmt.Printf("  ✓ Service created successfully\n")
	}

	return nil
}

// updateService updates an existing Docker service
func (m *Manager) updateService(
	client *ssh.Client,
	serviceName string,
	service *config.ServiceConfig,
	fullServiceName string,
	imageRef string,
	networkName string,
	traefikLabels []string,
) error {
	if m.verbose {
		fmt.Printf("  Updating existing service...\n")
	}

	// Build docker service update command
	cmd := fmt.Sprintf("docker service update --detach")

	// Force update to ensure new image is used even if tag hasn't changed
	// This is important when using same tags (like :production) with new builds
	cmd += " --force"

	// Update image
	cmd += fmt.Sprintf(" --image %s", imageRef)

	// Update Docker health check if configured
	cmd += m.buildHealthCheckFlags(service)

	// Update replicas (skip for global mode services)
	// Global mode services cannot have replicas changed
	isGlobalMode := service.Placement != nil && service.Placement.Strategy == "global"

	if !isGlobalMode {
		replicas := service.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		cmd += fmt.Sprintf(" --replicas %d", replicas)
	}

	// Update environment variables with proper expansion
	// Note: docker service update doesn't support --env-file, so we must use --env-add and --env-rm
	// Strategy: Get current env vars, remove ones that are no longer needed, then add/update all new ones

	// Get current environment variables from the running service
	currentEnvVars, err := m.getServiceEnvVars(client, fullServiceName)
	if err != nil {
		return fmt.Errorf("failed to get current env vars: %w", err)
	}

	if m.verbose && len(currentEnvVars) > 0 {
		fmt.Printf("  Current env vars: %d\n", len(currentEnvVars))
	}

	// Create secrets manager to expand environment variables
	secretsMgr, err := secrets.NewManager(m.environment)
	if err != nil {
		return fmt.Errorf("failed to create secrets manager: %w", err)
	}

	// Create env file to get expanded variables (we won't upload it, just use it for expansion)
	envFile, err := secretsMgr.CreateEnvFile(service)
	if err != nil {
		return fmt.Errorf("failed to create env file: %w", err)
	}

	// Get all expanded key-value pairs
	expandedEnvVars := envFile.GetAll()

	if m.verbose {
		fmt.Printf("  ✓ Expanded %d environment variables\n", len(expandedEnvVars))
	}

	// Strategy: Remove ALL current env vars, then add all new ones
	// This ensures values are updated and removed vars are gone
	// Note: --env-add doesn't update existing values, only adds new ones

	// First, remove all current env vars
	for currentKey := range currentEnvVars {
		cmd += fmt.Sprintf(" --env-rm %s", currentKey)
	}

	if m.verbose && len(currentEnvVars) > 0 {
		fmt.Printf("  Removing %d current env vars for clean update\n", len(currentEnvVars))
	}

	// Then add all new env vars
	for key, value := range expandedEnvVars {
		// Escape single quotes in value for shell
		escapedValue := strings.ReplaceAll(value, "'", "'\"'\"'")
		cmd += fmt.Sprintf(" --env-add %s='%s'", key, escapedValue)
	}

	// Add placement constraints if changed
	if service.Placement != nil {
		placementArgs, err := m.buildPlacementConstraintsForUpdate(service.Placement)
		if err != nil {
			return fmt.Errorf("failed to build placement constraints: %w", err)
		}
		cmd += placementArgs
	}

	// Update volume mounts
	// Get current mounts from the service
	currentMounts, err := m.getServiceMounts(client, fullServiceName)
	if err != nil {
		if m.verbose {
			fmt.Printf("  Warning: failed to get current mounts: %v\n", err)
		}
		currentMounts = make(map[string]VolumeMount)
	}

	// Build desired mounts from config
	desiredMounts, err := m.buildDesiredMounts(serviceName, service)
	if err != nil {
		return fmt.Errorf("failed to build desired mounts: %w", err)
	}

	// Compute mount changes
	mountAdds, mountRemoves := m.computeMountChanges(currentMounts, desiredMounts)

	// Ensure any new volumes exist before mounting
	if err := m.EnsureVolumesExist(client, serviceName, service); err != nil {
		return fmt.Errorf("failed to ensure volumes exist: %w", err)
	}

	// Apply mount removals
	for _, target := range mountRemoves {
		cmd += fmt.Sprintf(" --mount-rm %s", target)
	}

	// Apply mount additions
	for _, mountSpec := range mountAdds {
		cmd += fmt.Sprintf(" --mount-add %s", mountSpec)
	}

	if m.verbose && (len(mountAdds) > 0 || len(mountRemoves) > 0) {
		fmt.Printf("  Updating volumes: +%d -%d\n", len(mountAdds), len(mountRemoves))
	}

	// Update Traefik labels if provided
	// First get current labels to remove them, then add new ones
	if len(traefikLabels) > 0 {
		// Get current traefik labels
		currentLabels, err := m.getServiceLabels(client, fullServiceName)
		if err != nil {
			if m.verbose {
				fmt.Printf("  Warning: failed to get current labels: %v\n", err)
			}
		} else {
			// Remove existing traefik labels
			for key := range currentLabels {
				if strings.HasPrefix(key, "traefik.") {
					cmd += fmt.Sprintf(" --label-rm %s", key)
				}
			}
		}

		// Add new traefik labels
		for _, label := range traefikLabels {
			// Convert --label "key=value" to --label-add "key=value"
			// Handle both --label and 'label formats
			label = strings.TrimPrefix(label, "--label ")
			label = strings.TrimPrefix(label, "--label ")
			// Remove surrounding quotes if present
			label = strings.Trim(label, "'\"")
			if label != "" {
				// Quote labels containing special characters like parentheses
				if strings.ContainsAny(label, "()\"' ") {
					cmd += fmt.Sprintf(" --label-add '%s'", label)
				} else {
					cmd += fmt.Sprintf(" --label-add %s", label)
				}
			}
		}

		if m.verbose {
			fmt.Printf("  Updating %d Traefik labels\n", len(traefikLabels))
		}
	}

	// Publish ports directly when no Traefik/domains configured
	// For updates, we use --publish-add to ensure port is published
	if len(traefikLabels) == 0 && service.Port > 0 {
		// Check if port is already published
		portCheck := fmt.Sprintf("docker service inspect %s --format '{{range .Endpoint.Ports}}{{.PublishedPort}}{{end}}'", fullServiceName)
		currentPorts, _ := client.Execute(portCheck)
		if strings.TrimSpace(currentPorts) == "" {
			// No ports published, add it
			cmd += fmt.Sprintf(" --publish-add published=%d,target=%d,mode=ingress", service.Port, service.Port)
			if m.verbose {
				fmt.Printf("  Publishing port %d (no domain configured)\n", service.Port)
			}
		}
	}

	// Add service name at the end
	cmd += fmt.Sprintf(" %s", fullServiceName)

	if m.verbose {
		fmt.Printf("  Command: %s\n", cmd)
	}

	output, err := client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to update service: %w, output: %s", err, output)
	}

	if m.verbose {
		fmt.Printf("  ✓ Service updated successfully\n")
		fmt.Printf("  Rolling update in progress...\n")
	}

	return nil
}

// buildPlacementConstraints builds placement constraint arguments for docker service create
func (m *Manager) buildPlacementConstraints(placement *config.PlacementConfig) (string, error) {
	return m.buildPlacementArgs(placement, false)
}

// buildPlacementConstraintsForUpdate builds placement constraint arguments for docker service update
func (m *Manager) buildPlacementConstraintsForUpdate(placement *config.PlacementConfig) (string, error) {
	return m.buildPlacementArgs(placement, true)
}

// buildPlacementArgs builds placement constraint arguments
// isUpdate: if true, uses --constraint-add/--placement-pref-add (for service update)
//
//	if false, uses --constraint/--placement-pref (for service create)
func (m *Manager) buildPlacementArgs(placement *config.PlacementConfig, isUpdate bool) (string, error) {
	var args string

	// Select appropriate flags based on operation type
	constraintFlag := "--constraint"
	placementPrefFlag := "--placement-pref"
	if isUpdate {
		constraintFlag = "--constraint-add"
		placementPrefFlag = "--placement-pref-add"
	}

	// Handle placement strategy
	switch placement.Strategy {
	case "spread":
		// Spread across all available nodes
		args += fmt.Sprintf(" %s 'spread=node.hostname'", placementPrefFlag)

	case "pinned":
		// Pin to specific servers
		if len(placement.Servers) == 0 {
			return "", fmt.Errorf("pinned strategy requires servers to be specified")
		}
		for _, server := range placement.Servers {
			args += fmt.Sprintf(" %s 'node.hostname==%s'", constraintFlag, server)
		}

	case "global":
		// Global mode is handled separately in deployService
		// Don't add any constraints here
		return args, nil

	case "any":
		// No specific placement constraints (default Swarm behavior)
		// Services can run on any node

	default:
		if placement.Strategy != "" {
			return "", fmt.Errorf("unknown placement strategy: %s (valid: spread, pinned, any, global)", placement.Strategy)
		}
	}

	// Add custom constraints
	for _, constraint := range placement.Constraints {
		args += fmt.Sprintf(" %s '%s'", constraintFlag, constraint)
	}

	// Add placement preferences
	for _, pref := range placement.Preferences {
		args += fmt.Sprintf(" %s '%s'", placementPrefFlag, pref)
	}

	return args, nil
}

// parseVolumeSpec parses a volume specification into source and target
// Supports formats:
//   - "/path" -> (target only, create named volume)
//   - "source:/target" -> (bind mount or named volume)
func (m *Manager) parseVolumeSpec(volume string) (source, target string) {
	parts := strings.Split(volume, ":")
	if len(parts) == 1 {
		// Only target specified
		return parts[0], ""
	}
	// Source and target specified
	return parts[0], parts[1]
}

// GetServiceStatus retrieves the status of a service
func (m *Manager) GetServiceStatus(client *ssh.Client, fullServiceName string) (string, error) {
	cmd := fmt.Sprintf("docker service ps %s --format '{{.CurrentState}}'", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to get service status: %w", err)
	}

	return strings.TrimSpace(output), nil
}

// RemoveService removes a service from the swarm
func (m *Manager) RemoveService(client *ssh.Client, fullServiceName string) error {
	if m.verbose {
		fmt.Printf("  Removing service: %s\n", fullServiceName)
	}

	cmd := fmt.Sprintf("docker service rm %s", fullServiceName)
	if _, err := client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to remove service: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Service removed\n")
	}

	return nil
}

// ScaleService scales a service to the specified number of replicas
func (m *Manager) ScaleService(client *ssh.Client, fullServiceName string, replicas int) error {
	if m.verbose {
		fmt.Printf("  Scaling service to %d replicas...\n", replicas)
	}

	cmd := fmt.Sprintf("docker service scale %s=%d", fullServiceName, replicas)
	if _, err := client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to scale service: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Service scaled successfully\n")
	}

	return nil
}

// buildHealthCheckFlags builds Docker health check flags for service create/update commands.
// Returns an empty string if no health check is configured.
func (m *Manager) buildHealthCheckFlags(service *config.ServiceConfig) string {
	if service.HealthCheck.Path == "" || service.Port <= 0 {
		return ""
	}

	var flags string

	healthCmd := fmt.Sprintf("curl -sf http://localhost:%d%s || exit 1", service.Port, service.HealthCheck.Path)
	flags += fmt.Sprintf(" --health-cmd '%s'", healthCmd)

	interval := service.HealthCheck.Interval
	if interval == "" {
		interval = "10s"
	}
	flags += fmt.Sprintf(" --health-interval %s", interval)

	timeout := service.HealthCheck.Timeout
	if timeout == "" {
		timeout = "5s"
	}
	flags += fmt.Sprintf(" --health-timeout %s", timeout)

	retries := service.HealthCheck.Retries
	if retries <= 0 {
		retries = 3
	}
	flags += fmt.Sprintf(" --health-retries %d", retries)

	startPeriod := service.HealthCheck.StartPeriod
	if startPeriod == "" {
		startPeriod = "0s"
	}
	flags += fmt.Sprintf(" --health-start-period %s", startPeriod)

	if m.verbose {
		fmt.Printf("  Health check: GET http://localhost:%d%s (interval=%s, timeout=%s, retries=%d)\n",
			service.Port, service.HealthCheck.Path, interval, timeout, retries)
	}

	return flags
}

// validateBindMountPath checks if a bind mount path is safe to use
// Blocks access to sensitive system directories
func validateBindMountPath(path string) error {
	// Normalize path
	cleanPath := strings.TrimSuffix(path, "/")

	// Blocked paths - these should never be bind mounted
	blockedPaths := []string{
		"/",
		"/etc",
		"/etc/passwd",
		"/etc/shadow",
		"/etc/sudoers",
		"/etc/ssh",
		"/root",
		"/root/.ssh",
		"/var/run/docker.sock",
		"/proc",
		"/sys",
		"/dev",
		"/boot",
		"/lib",
		"/lib64",
		"/usr/bin",
		"/usr/sbin",
		"/bin",
		"/sbin",
	}

	// Check exact matches and prefixes
	for _, blocked := range blockedPaths {
		if cleanPath == blocked {
			return fmt.Errorf("bind mounting '%s' is not allowed (security risk)", path)
		}
		// Also block subdirectories of sensitive paths
		if blocked != "/" && strings.HasPrefix(cleanPath, blocked+"/") {
			return fmt.Errorf("bind mounting paths under '%s' is not allowed (security risk)", blocked)
		}
	}

	// Block paths that could escape containment
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal (..) not allowed in bind mounts")
	}

	return nil
}
