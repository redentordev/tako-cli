package network

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Manager handles Docker network operations
type Manager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewManager creates a new network manager
func NewManager(client *ssh.Client, projectName string, environment string, verbose bool) *Manager {
	return &Manager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// GetNetworkName returns the Docker network name for the project and environment
func (m *Manager) GetNetworkName() string {
	return fmt.Sprintf("tako_%s_%s", m.projectName, m.environment)
}

// EnsureNetwork creates the Docker network if it doesn't exist
func (m *Manager) EnsureNetwork() error {
	networkName := m.GetNetworkName()

	// Check if network exists
	checkCmd := fmt.Sprintf("docker network ls --filter name=^%s$ --format '{{.Name}}'", networkName)
	output, err := m.client.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check network: %w", err)
	}

	// Network already exists
	if strings.TrimSpace(output) == networkName {
		if m.verbose {
			fmt.Printf("  Docker network '%s' already exists\n", networkName)
		}
		return nil
	}

	// Create network
	if m.verbose {
		fmt.Printf("  Creating Docker network '%s'...\n", networkName)
	}

	createCmd := fmt.Sprintf(
		"docker network create --driver bridge --label project=%s --label environment=%s %s",
		m.projectName,
		m.environment,
		networkName,
	)

	if _, err := m.client.Execute(createCmd); err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Network created\n")
	}

	return nil
}

// ConnectContainer connects a container to the project network
func (m *Manager) ConnectContainer(containerName string, aliases []string) error {
	networkName := m.GetNetworkName()

	cmd := fmt.Sprintf("docker network connect")

	// Add aliases for service discovery
	for _, alias := range aliases {
		cmd += fmt.Sprintf(" --alias %s", alias)
	}

	cmd += fmt.Sprintf(" %s %s 2>/dev/null || true", networkName, containerName)

	if _, err := m.client.Execute(cmd); err != nil {
		// Ignore errors if already connected
		if m.verbose {
			fmt.Printf("  Note: %v\n", err)
		}
	}

	return nil
}

// DisconnectContainer disconnects a container from the project network
func (m *Manager) DisconnectContainer(containerName string) error {
	networkName := m.GetNetworkName()

	cmd := fmt.Sprintf("docker network disconnect %s %s 2>/dev/null || true", networkName, containerName)
	m.client.Execute(cmd)

	return nil
}

// GetNetworkInfo returns information about the network
func (m *Manager) GetNetworkInfo() (string, error) {
	networkName := m.GetNetworkName()

	cmd := fmt.Sprintf("docker network inspect %s", networkName)
	output, err := m.client.Execute(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to inspect network: %w", err)
	}

	return output, nil
}

// RemoveNetwork removes the Docker network
func (m *Manager) RemoveNetwork() error {
	networkName := m.GetNetworkName()

	if m.verbose {
		fmt.Printf("  Removing Docker network '%s'...\n", networkName)
	}

	cmd := fmt.Sprintf("docker network rm %s 2>/dev/null || true", networkName)
	m.client.Execute(cmd)

	return nil
}

// GetContainerIP returns the IP address of a container in the network
func (m *Manager) GetContainerIP(containerName string) (string, error) {
	cmd := fmt.Sprintf(
		"docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' %s",
		containerName,
	)

	output, err := m.client.Execute(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to get container IP: %w", err)
	}

	return strings.TrimSpace(output), nil
}

// ListConnectedContainers lists all containers connected to the network
func (m *Manager) ListConnectedContainers() ([]string, error) {
	networkName := m.GetNetworkName()

	cmd := fmt.Sprintf(
		"docker network inspect %s --format '{{range .Containers}}{{.Name}} {{end}}'",
		networkName,
	)

	output, err := m.client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	containers := strings.Fields(strings.TrimSpace(output))
	return containers, nil
}

// ConnectToExternalNetwork connects a container to another project's network
// This enables cross-project service communication via Docker DNS
// targetEnvironment: environment of the target project (defaults to current environment if empty)
func (m *Manager) ConnectToExternalNetwork(containerName, targetProject string, targetEnvironment string) error {
	// Default to current environment if not specified
	if targetEnvironment == "" {
		targetEnvironment = m.environment
	}
	targetNetwork := fmt.Sprintf("tako_%s_%s", targetProject, targetEnvironment)

	// Check if target network exists
	checkCmd := fmt.Sprintf("docker network ls --filter name=^%s$ --format '{{.Name}}'", targetNetwork)
	output, err := m.client.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check target network: %w", err)
	}

	if strings.TrimSpace(output) != targetNetwork {
		return fmt.Errorf("target network %s does not exist (project %s not deployed?)", targetNetwork, targetProject)
	}

	// Connect container to target network
	connectCmd := fmt.Sprintf("docker network connect %s %s 2>/dev/null || true", targetNetwork, containerName)
	if _, err := m.client.Execute(connectCmd); err != nil {
		// Ignore if already connected
		if m.verbose {
			fmt.Printf("  Note: %v\n", err)
		}
	}

	if m.verbose {
		fmt.Printf("  ✓ Connected to %s network\n", targetProject)
	}

	return nil
}

// GetAllProjectNetworks returns a list of all Tako project networks
func (m *Manager) GetAllProjectNetworks() ([]string, error) {
	cmd := "docker network ls --filter label=project --format '{{.Name}}'"
	output, err := m.client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list project networks: %w", err)
	}

	networks := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			networks = append(networks, line)
		}
	}

	return networks, nil
}

// GetContainerNetworks returns a list of networks a container is connected to
func (m *Manager) GetContainerNetworks(containerName string) ([]string, error) {
	cmd := fmt.Sprintf("docker inspect %s --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'", containerName)
	output, err := m.client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container networks: %w", err)
	}

	networks := []string{}
	for _, network := range strings.Fields(strings.TrimSpace(output)) {
		if network != "" {
			networks = append(networks, network)
		}
	}

	return networks, nil
}

// EnsureContainerConnectedToAllNetworks ensures a container (like Traefik) is connected to all project networks
func (m *Manager) EnsureContainerConnectedToAllNetworks(containerName string) error {
	// Get all Tako project networks
	projectNetworks, err := m.GetAllProjectNetworks()
	if err != nil {
		return fmt.Errorf("failed to get project networks: %w", err)
	}

	if len(projectNetworks) == 0 {
		if m.verbose {
			fmt.Printf("  No project networks found\n")
		}
		return nil
	}

	// Get networks the container is currently connected to
	containerNetworks, err := m.GetContainerNetworks(containerName)
	if err != nil {
		return fmt.Errorf("failed to get container networks: %w", err)
	}

	// Create a map for quick lookup
	connectedMap := make(map[string]bool)
	for _, network := range containerNetworks {
		connectedMap[network] = true
	}

	// Connect to any missing networks
	connected := 0
	for _, network := range projectNetworks {
		if !connectedMap[network] {
			if m.verbose {
				fmt.Printf("  Connecting %s to network %s...\n", containerName, network)
			}
			connectCmd := fmt.Sprintf("docker network connect %s %s 2>/dev/null || true", network, containerName)
			if _, err := m.client.Execute(connectCmd); err != nil {
				if m.verbose {
					fmt.Printf("  Warning: Failed to connect to %s: %v\n", network, err)
				}
			} else {
				connected++
			}
		}
	}

	if m.verbose && connected > 0 {
		fmt.Printf("  ✓ Connected %s to %d additional network(s)\n", containerName, connected)
	}

	return nil
}
