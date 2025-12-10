package network

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// EncryptionConfig defines network encryption settings
type EncryptionConfig struct {
	Enabled    bool   `yaml:"enabled"`
	IPSecMode  string `yaml:"ipsecMode,omitempty"` // "default", "strict"
	DataPath   string `yaml:"dataPath,omitempty"`  // "encrypted", "unencrypted"
}

// NetworkEncryption manages Docker Swarm network encryption
type NetworkEncryption struct {
	client  *ssh.Client
	verbose bool
}

// NewNetworkEncryption creates a new network encryption manager
func NewNetworkEncryption(client *ssh.Client, verbose bool) *NetworkEncryption {
	return &NetworkEncryption{
		client:  client,
		verbose: verbose,
	}
}

// EnableEncryption enables encryption on a Docker overlay network
// Docker Swarm uses IPsec encryption for overlay networks when --opt encrypted is set
func (n *NetworkEncryption) EnableEncryption(networkName string) error {
	if n.verbose {
		fmt.Printf("  → Enabling encryption on network: %s\n", networkName)
	}

	// Check if network exists and get current options
	inspectCmd := fmt.Sprintf("docker network inspect %s --format '{{.Options}}'", networkName)
	output, err := n.client.Execute(inspectCmd)
	if err != nil {
		return fmt.Errorf("network %s not found: %w", networkName, err)
	}

	// Check if already encrypted
	if strings.Contains(output, "encrypted:true") {
		if n.verbose {
			fmt.Printf("  ✓ Network already encrypted\n")
		}
		return nil
	}

	// Docker doesn't allow modifying network options after creation
	// We need to recreate the network with encryption enabled
	return fmt.Errorf("cannot enable encryption on existing network. Network must be recreated with --opt encrypted=true")
}

// CreateEncryptedNetwork creates a new encrypted overlay network
func (n *NetworkEncryption) CreateEncryptedNetwork(networkName string, subnet string) error {
	if n.verbose {
		fmt.Printf("  → Creating encrypted overlay network: %s\n", networkName)
	}

	// Build network create command with encryption
	cmd := fmt.Sprintf("docker network create --driver overlay --opt encrypted=true --attachable")

	// Add subnet if specified
	if subnet != "" {
		cmd += fmt.Sprintf(" --subnet %s", subnet)
	}

	cmd += fmt.Sprintf(" %s", networkName)

	if _, err := n.client.Execute(cmd); err != nil {
		// Check if network already exists
		if strings.Contains(err.Error(), "already exists") {
			if n.verbose {
				fmt.Printf("  ✓ Network already exists\n")
			}
			return nil
		}
		return fmt.Errorf("failed to create encrypted network: %w", err)
	}

	if n.verbose {
		fmt.Printf("  ✓ Encrypted network created\n")
	}

	return nil
}

// RecreateNetworkWithEncryption recreates a network with encryption enabled
// WARNING: This will disconnect all services from the network
func (n *NetworkEncryption) RecreateNetworkWithEncryption(networkName string) error {
	if n.verbose {
		fmt.Printf("  → Recreating network with encryption: %s\n", networkName)
	}

	// Get network details
	inspectCmd := fmt.Sprintf("docker network inspect %s --format '{{.IPAM.Config}}'", networkName)
	output, err := n.client.Execute(inspectCmd)
	if err != nil {
		return fmt.Errorf("failed to inspect network: %w", err)
	}

	// Parse subnet (basic parsing)
	subnet := ""
	if strings.Contains(output, "Subnet:") {
		// Extract subnet from output like [{Subnet:10.0.0.0/24}]
		parts := strings.Split(output, "Subnet:")
		if len(parts) > 1 {
			subnet = strings.Split(strings.TrimSpace(parts[1]), " ")[0]
			subnet = strings.TrimSuffix(subnet, "]")
			subnet = strings.TrimSuffix(subnet, "}")
		}
	}

	// Remove the existing network
	removeCmd := fmt.Sprintf("docker network rm %s", networkName)
	if _, err := n.client.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove network: %w", err)
	}

	// Create new encrypted network
	return n.CreateEncryptedNetwork(networkName, subnet)
}

// IsNetworkEncrypted checks if a network has encryption enabled
func (n *NetworkEncryption) IsNetworkEncrypted(networkName string) (bool, error) {
	inspectCmd := fmt.Sprintf("docker network inspect %s --format '{{.Options}}'", networkName)
	output, err := n.client.Execute(inspectCmd)
	if err != nil {
		return false, fmt.Errorf("failed to inspect network: %w", err)
	}

	return strings.Contains(output, "encrypted:true"), nil
}

// GetNetworkStatus returns the encryption status of all overlay networks
func (n *NetworkEncryption) GetNetworkStatus() (map[string]bool, error) {
	// List all overlay networks
	listCmd := "docker network ls --filter driver=overlay --format '{{.Name}}'"
	output, err := n.client.Execute(listCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}

	status := make(map[string]bool)
	for _, name := range strings.Split(output, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == "ingress" {
			continue
		}

		encrypted, err := n.IsNetworkEncrypted(name)
		if err != nil {
			continue
		}
		status[name] = encrypted
	}

	return status, nil
}

// EnsureProjectNetworkEncrypted ensures the project network is encrypted
func (n *NetworkEncryption) EnsureProjectNetworkEncrypted(projectName, environment string) error {
	networkName := fmt.Sprintf("%s_%s", projectName, environment)

	// Check if network exists
	checkCmd := fmt.Sprintf("docker network inspect %s >/dev/null 2>&1 && echo 'exists'", networkName)
	output, _ := n.client.Execute(checkCmd)

	if strings.Contains(output, "exists") {
		// Network exists, check if encrypted
		encrypted, err := n.IsNetworkEncrypted(networkName)
		if err != nil {
			return err
		}

		if !encrypted {
			if n.verbose {
				fmt.Printf("  ⚠ Network %s is not encrypted. Consider recreating with encryption.\n", networkName)
			}
			// Don't automatically recreate as it would disrupt services
			return nil
		}

		if n.verbose {
			fmt.Printf("  ✓ Network %s is encrypted\n", networkName)
		}
		return nil
	}

	// Network doesn't exist, create with encryption
	return n.CreateEncryptedNetwork(networkName, "")
}

// EnableSwarmAutolock enables Swarm autolock for manager node encryption
func (n *NetworkEncryption) EnableSwarmAutolock() (string, error) {
	if n.verbose {
		fmt.Printf("  → Enabling Swarm autolock...\n")
	}

	// Enable autolock
	cmd := "docker swarm update --autolock=true 2>&1"
	output, err := n.client.Execute(cmd)
	if err != nil {
		if strings.Contains(output, "already locked") {
			if n.verbose {
				fmt.Printf("  ✓ Swarm autolock already enabled\n")
			}
			return "", nil
		}
		return "", fmt.Errorf("failed to enable autolock: %w", err)
	}

	// Extract unlock key from output
	// Output format: "To unlock a swarm manager...\n\n    SWMKEY-1-...\n\n..."
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SWMKEY-") {
			if n.verbose {
				fmt.Printf("  ✓ Swarm autolock enabled\n")
				fmt.Printf("  ⚠ SAVE THIS KEY SECURELY - it's needed to unlock managers after restart\n")
			}
			return line, nil
		}
	}

	return "", nil
}

// GetSwarmUnlockKey retrieves the current Swarm unlock key
func (n *NetworkEncryption) GetSwarmUnlockKey() (string, error) {
	cmd := "docker swarm unlock-key -q 2>&1"
	output, err := n.client.Execute(cmd)
	if err != nil {
		if strings.Contains(output, "not locked") {
			return "", fmt.Errorf("swarm is not locked (autolock not enabled)")
		}
		return "", fmt.Errorf("failed to get unlock key: %w", err)
	}

	return strings.TrimSpace(output), nil
}

// RotateSwarmUnlockKey rotates the Swarm unlock key
func (n *NetworkEncryption) RotateSwarmUnlockKey() (string, error) {
	if n.verbose {
		fmt.Printf("  → Rotating Swarm unlock key...\n")
	}

	cmd := "docker swarm unlock-key --rotate -q 2>&1"
	output, err := n.client.Execute(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to rotate unlock key: %w", err)
	}

	if n.verbose {
		fmt.Printf("  ✓ Swarm unlock key rotated\n")
	}

	return strings.TrimSpace(output), nil
}
