package swarm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// RegistryConfig holds registry configuration
type RegistryConfig struct {
	Host     string
	Port     int
	Insecure bool
}

// EnsureRegistry sets up a Docker registry for the swarm
func (m *Manager) EnsureRegistry(managerClient *ssh.Client) (*RegistryConfig, error) {
	registryName := fmt.Sprintf("registry_%s", m.config.Project.Name)
	registryPort := 5000

	if m.verbose {
		fmt.Printf("\n→ Setting up Docker registry for image distribution...\n")
	}

	// Check if registry is already running
	checkCmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Names}}'", registryName)
	output, err := managerClient.Execute(checkCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to check registry: %w", err)
	}

	if strings.TrimSpace(output) == registryName {
		if m.verbose {
			fmt.Printf("  Registry already running\n")
		}

		// Get manager host
		managerHost, err := m.getManagerHost()
		if err != nil {
			return nil, err
		}

		return &RegistryConfig{
			Host:     managerHost,
			Port:     registryPort,
			Insecure: true,
		}, nil
	}

	if m.verbose {
		fmt.Printf("  Starting registry container...\n")
	}

	// Create registry container
	registryCmd := fmt.Sprintf(
		"docker run -d --name %s --restart=always "+
			"-p %d:5000 "+
			"-v registry_data:/var/lib/registry "+
			"registry:2",
		registryName,
		registryPort,
	)

	if _, err := managerClient.Execute(registryCmd); err != nil {
		return nil, fmt.Errorf("failed to start registry: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Registry started successfully\n")
	}

	// Get manager host
	managerHost, err := m.getManagerHost()
	if err != nil {
		return nil, err
	}

	registryConfig := &RegistryConfig{
		Host:     managerHost,
		Port:     registryPort,
		Insecure: true,
	}

	// Wait for registry to be ready
	if err := m.waitForRegistryReady(managerClient, registryConfig); err != nil {
		return nil, fmt.Errorf("registry failed to become ready: %w", err)
	}

	// Configure insecure registry on all nodes
	if err := m.configureInsecureRegistry(managerClient, registryConfig); err != nil {
		return nil, fmt.Errorf("failed to configure insecure registry: %w", err)
	}

	return registryConfig, nil
}

// getManagerHost returns the host address of the manager node
func (m *Manager) getManagerHost() (string, error) {
	// Get manager server from environment
	managerName, err := m.config.GetManagerServer(m.environment)
	if err != nil {
		return "", err
	}

	server, exists := m.config.Servers[managerName]
	if !exists {
		return "", fmt.Errorf("manager server %s not found", managerName)
	}

	return server.Host, nil
}

// waitForRegistryReady waits for the registry to become healthy
func (m *Manager) waitForRegistryReady(client *ssh.Client, registry *RegistryConfig) error {
	if m.verbose {
		fmt.Printf("  Waiting for registry to be ready...\n")
	}

	registryAddr := fmt.Sprintf("%s:%d", registry.Host, registry.Port)
	maxAttempts := 30
	retryInterval := 2 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Check if registry container is running
		checkCmd := "docker ps --filter name=registry --filter status=running --format '{{.Names}}' | grep -q registry"
		if _, err := client.Execute(checkCmd); err == nil {
			// Container is running, now check if API responds
			healthCmd := fmt.Sprintf("curl -sf http://%s/v2/ > /dev/null 2>&1", registryAddr)
			if _, err := client.Execute(healthCmd); err == nil {
				if m.verbose {
					fmt.Printf("  ✓ Registry is ready and responding\n")
				}
				return nil
			}
		}

		if attempt < maxAttempts {
			if m.verbose && attempt%5 == 0 {
				fmt.Printf("    Still waiting... (attempt %d/%d)\n", attempt, maxAttempts)
			}
			time.Sleep(retryInterval)
		}
	}

	return fmt.Errorf("registry did not become ready within %d seconds", maxAttempts*int(retryInterval.Seconds()))
}

// configureInsecureRegistry configures Docker to allow insecure registry access
// Safely merges with existing daemon.json configuration
func (m *Manager) configureInsecureRegistry(managerClient *ssh.Client, registry *RegistryConfig) error {
	if m.verbose {
		fmt.Printf("  Configuring insecure registry access...\n")
	}

	registryAddr := fmt.Sprintf("%s:%d", registry.Host, registry.Port)

	// Step 1: Read existing daemon.json if it exists
	readCmd := "sudo cat /etc/docker/daemon.json 2>/dev/null || echo '{}'"
	existingJSON, err := managerClient.Execute(readCmd)
	if err != nil {
		return fmt.Errorf("failed to read daemon.json: %w", err)
	}

	// Step 2: Parse existing configuration
	var daemonConfig map[string]interface{}
	if err := json.Unmarshal([]byte(existingJSON), &daemonConfig); err != nil {
		// If parsing fails, start with empty config
		if m.verbose {
			fmt.Printf("    Warning: Could not parse existing daemon.json, creating new\n")
		}
		daemonConfig = make(map[string]interface{})
	}

	// Step 3: Merge insecure-registries
	var insecureRegistries []string
	if existing, ok := daemonConfig["insecure-registries"]; ok {
		// Convert existing entries
		if registryList, ok := existing.([]interface{}); ok {
			for _, reg := range registryList {
				if regStr, ok := reg.(string); ok {
					// Don't add duplicate
					if regStr != registryAddr {
						insecureRegistries = append(insecureRegistries, regStr)
					}
				}
			}
		}
	}
	// Add our registry
	insecureRegistries = append(insecureRegistries, registryAddr)
	daemonConfig["insecure-registries"] = insecureRegistries

	// Step 4: Create backup of existing daemon.json
	if m.verbose {
		fmt.Printf("    Creating backup of daemon.json...\n")
	}
	backupCmd := "sudo cp /etc/docker/daemon.json /etc/docker/daemon.json.backup 2>/dev/null || true"
	managerClient.Execute(backupCmd)

	// Step 5: Marshal merged configuration
	mergedJSON, err := json.MarshalIndent(daemonConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal daemon.json: %w", err)
	}

	// Step 6: Write merged configuration
	// Escape JSON for shell (replace ' with '\'' for safe single-quote wrapping)
	escapedJSON := strings.ReplaceAll(string(mergedJSON), "'", "'\\''")
	writeCmd := fmt.Sprintf("echo '%s' | sudo tee /etc/docker/daemon.json > /dev/null", escapedJSON)
	if _, err := managerClient.Execute(writeCmd); err != nil {
		// Try to restore backup on failure
		managerClient.Execute("sudo mv /etc/docker/daemon.json.backup /etc/docker/daemon.json 2>/dev/null || true")
		return fmt.Errorf("failed to write daemon.json: %w", err)
	}

	// Step 7: Reload Docker daemon
	if m.verbose {
		fmt.Printf("    Reloading Docker daemon...\n")
	}

	if _, err := managerClient.Execute("sudo systemctl reload docker"); err != nil {
		// Try restart if reload fails
		if _, err := managerClient.Execute("sudo systemctl restart docker"); err != nil {
			// Restore backup on failure
			managerClient.Execute("sudo mv /etc/docker/daemon.json.backup /etc/docker/daemon.json 2>/dev/null || true")
			managerClient.Execute("sudo systemctl restart docker")
			return fmt.Errorf("failed to reload Docker daemon: %w", err)
		}
	}

	// Step 8: Verify Docker is running
	if _, err := managerClient.Execute("sudo systemctl is-active docker"); err != nil {
		return fmt.Errorf("Docker daemon failed to start after configuration change")
	}

	if m.verbose {
		fmt.Printf("  ✓ Insecure registry configured\n")
	}

	return nil
}

// PushImageToRegistry pushes an image to the swarm registry
func (m *Manager) PushImageToRegistry(client *ssh.Client, localImage string, registry *RegistryConfig) (string, error) {
	registryAddr := fmt.Sprintf("%s:%d", registry.Host, registry.Port)
	remoteImage := fmt.Sprintf("%s/%s", registryAddr, localImage)

	if m.verbose {
		fmt.Printf("  Tagging image for registry...\n")
		fmt.Printf("    Local: %s\n", localImage)
		fmt.Printf("    Remote: %s\n", remoteImage)

		// Debug: List all images to see what's actually available
		fmt.Printf("  Listing available images:\n")
		imagesOut, _ := client.Execute("docker images --format 'table {{.Repository}}:{{.Tag}}\t{{.ID}}' | head -20")
		fmt.Printf("%s\n", imagesOut)
	}

	// Tag image for registry
	tagCmd := fmt.Sprintf("docker tag %s %s 2>&1", localImage, remoteImage)
	output, err := client.Execute(tagCmd)
	if err != nil {
		return "", fmt.Errorf("failed to tag image: %w, command: %s, output: %s", err, tagCmd, output)
	}

	if m.verbose {
		fmt.Printf("  Pushing image to registry...\n")
	}

	// Push to registry
	pushCmd := fmt.Sprintf("docker push %s", remoteImage)
	output, err = client.Execute(pushCmd)
	if err != nil {
		return "", fmt.Errorf("failed to push image: %w, output: %s", err, output)
	}

	if m.verbose {
		fmt.Printf("  ✓ Image pushed successfully\n")
	}

	return remoteImage, nil
}

// ConfigureWorkersForRegistry configures worker nodes to use the insecure registry
func (m *Manager) ConfigureWorkersForRegistry(registry *RegistryConfig) error {
	// Get all worker servers
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return err
	}

	// Get manager server to skip it
	managerName, err := m.config.GetManagerServer(m.environment)
	if err != nil {
		return err
	}

	if m.verbose {
		fmt.Printf("\n→ Configuring worker nodes for registry access...\n")
	}

	for _, serverName := range servers {
		// Skip manager node (already configured)
		if serverName == managerName {
			continue
		}

		server := m.config.Servers[serverName]
		if m.verbose {
			fmt.Printf("  Configuring %s (%s)...\n", serverName, server.Host)
		}

		// Get or create client
		client, err := m.sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		// Configure insecure registry
		if err := m.configureInsecureRegistry(client, registry); err != nil {
			return fmt.Errorf("failed to configure registry on %s: %w", serverName, err)
		}

		if m.verbose {
			fmt.Printf("  ✓ %s configured\n", serverName)
		}
	}

	if m.verbose {
		fmt.Printf("  ✓ All workers configured for registry\n")
	}

	return nil
}
