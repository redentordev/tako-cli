package swarm

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Manager handles Docker Swarm operations
type Manager struct {
	config      *config.Config
	sshPool     *ssh.Pool
	environment string
	verbose     bool
}

// NewManager creates a new Swarm manager
func NewManager(cfg *config.Config, sshPool *ssh.Pool, environment string, verbose bool) *Manager {
	return &Manager{
		config:      cfg,
		sshPool:     sshPool,
		environment: environment,
		verbose:     verbose,
	}
}

// IsSwarmInitialized checks if Docker Swarm is already initialized on a node
func (m *Manager) IsSwarmInitialized(client *ssh.Client) (bool, error) {
	// Check if node is part of a swarm
	output, err := client.Execute("docker info --format '{{.Swarm.LocalNodeState}}'")
	if err != nil {
		return false, fmt.Errorf("failed to check swarm status: %w", err)
	}

	state := strings.TrimSpace(output)
	return state == "active", nil
}

// GetSwarmNodeRole returns the role of the current node (manager or worker)
func (m *Manager) GetSwarmNodeRole(client *ssh.Client) (string, error) {
	output, err := client.Execute("docker info --format '{{.Swarm.ControlAvailable}}'")
	if err != nil {
		return "", fmt.Errorf("failed to get node role: %w", err)
	}

	controlAvailable := strings.TrimSpace(output)
	if controlAvailable == "true" {
		return "manager", nil
	}
	return "worker", nil
}

// InitializeSwarm initializes Docker Swarm on the manager node
func (m *Manager) InitializeSwarm(client *ssh.Client, advertiseAddr string) error {
	if m.verbose {
		fmt.Printf("  Initializing Docker Swarm on manager node...\n")
	}

	// Initialize swarm with advertise address
	initCmd := fmt.Sprintf("docker swarm init --advertise-addr %s", advertiseAddr)
	output, err := client.Execute(initCmd)
	if err != nil {
		// Check if already initialized
		if strings.Contains(output, "already part of a swarm") {
			if m.verbose {
				fmt.Printf("  Swarm already initialized\n")
			}
			return nil
		}
		return fmt.Errorf("failed to initialize swarm: %w, output: %s", err, output)
	}

	if m.verbose {
		fmt.Printf("  ✓ Swarm initialized successfully\n")
	}

	return nil
}

// GetJoinToken retrieves the join token for workers
func (m *Manager) GetJoinToken(managerClient *ssh.Client, role string) (string, error) {
	if role != "worker" && role != "manager" {
		return "", fmt.Errorf("invalid role: %s (must be 'worker' or 'manager')", role)
	}

	cmd := fmt.Sprintf("docker swarm join-token -q %s", role)
	output, err := managerClient.Execute(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to get join token: %w", err)
	}

	token := strings.TrimSpace(output)
	if token == "" {
		return "", fmt.Errorf("received empty join token")
	}

	return token, nil
}

// JoinSwarm joins a worker node to the swarm
func (m *Manager) JoinSwarm(workerClient *ssh.Client, managerAddr string, token string) error {
	if m.verbose {
		fmt.Printf("  Joining node to swarm...\n")
	}

	joinCmd := fmt.Sprintf("docker swarm join --token %s %s:2377", token, managerAddr)
	output, err := workerClient.Execute(joinCmd)
	if err != nil {
		// Check if already joined
		if strings.Contains(output, "already part of a swarm") {
			if m.verbose {
				fmt.Printf("  Node already part of swarm\n")
			}
			return nil
		}
		return fmt.Errorf("failed to join swarm: %w, output: %s", err, output)
	}

	if m.verbose {
		fmt.Printf("  ✓ Node joined swarm successfully\n")
	}

	return nil
}

// SetNodeLabels sets labels on a swarm node
func (m *Manager) SetNodeLabels(managerClient *ssh.Client, nodeID string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}

	if m.verbose {
		fmt.Printf("  Setting node labels...\n")
	}

	for key, value := range labels {
		cmd := fmt.Sprintf("docker node update --label-add %s=%s %s", key, value, nodeID)
		if _, err := managerClient.Execute(cmd); err != nil {
			return fmt.Errorf("failed to set label %s=%s: %w", key, value, err)
		}
		if m.verbose {
			fmt.Printf("    %s=%s\n", key, value)
		}
	}

	if m.verbose {
		fmt.Printf("  ✓ Node labels set successfully\n")
	}

	return nil
}

// GetNodeID retrieves the node ID of the current machine
func (m *Manager) GetNodeID(client *ssh.Client) (string, error) {
	output, err := client.Execute("docker info --format '{{.Swarm.NodeID}}'")
	if err != nil {
		return "", fmt.Errorf("failed to get node ID: %w", err)
	}

	nodeID := strings.TrimSpace(output)
	if nodeID == "" {
		return "", fmt.Errorf("node ID is empty (not in swarm?)")
	}

	return nodeID, nil
}

// ListNodes lists all nodes in the swarm
func (m *Manager) ListNodes(managerClient *ssh.Client) (string, error) {
	output, err := managerClient.Execute("docker node ls --format 'table {{.Hostname}}\\t{{.Status}}\\t{{.Availability}}\\t{{.ManagerStatus}}'")
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	return output, nil
}

// LeaveSwarm removes a node from the swarm
func (m *Manager) LeaveSwarm(client *ssh.Client, force bool) error {
	if m.verbose {
		fmt.Printf("  Leaving swarm...\n")
	}

	cmd := "docker swarm leave"
	if force {
		cmd += " --force"
	}

	output, err := client.Execute(cmd)
	if err != nil {
		if strings.Contains(output, "not part of a swarm") {
			if m.verbose {
				fmt.Printf("  Node is not part of a swarm\n")
			}
			return nil
		}
		return fmt.Errorf("failed to leave swarm: %w, output: %s", err, output)
	}

	if m.verbose {
		fmt.Printf("  ✓ Left swarm successfully\n")
	}

	return nil
}

// EnsureSwarmNetwork creates an overlay network for the environment if it doesn't exist
func (m *Manager) EnsureSwarmNetwork(managerClient *ssh.Client, networkName string) error {
	if m.verbose {
		fmt.Printf("  Ensuring overlay network: %s\n", networkName)
	}

	// Check if network exists
	checkCmd := fmt.Sprintf("docker network ls --filter name=^%s$ --format '{{.Name}}'", networkName)
	output, err := managerClient.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check network: %w", err)
	}

	// Network already exists
	if strings.TrimSpace(output) == networkName {
		if m.verbose {
			fmt.Printf("  Network already exists\n")
		}
		return nil
	}

	// Create overlay network
	createCmd := fmt.Sprintf(
		"docker network create --driver overlay --attachable --label project=%s --label environment=%s %s 2>&1",
		m.config.Project.Name,
		m.environment,
		networkName,
	)

	createOutput, createErr := managerClient.Execute(createCmd)
	if createErr != nil {
		// Check if error is because network already exists
		if strings.Contains(createOutput, "already exists") {
			if m.verbose {
				fmt.Printf("  Network already exists\n")
			}
			return nil
		}
		return fmt.Errorf("failed to create overlay network: %w, output: %s", createErr, createOutput)
	}

	if m.verbose {
		fmt.Printf("  ✓ Overlay network created\n")
	}

	return nil
}
