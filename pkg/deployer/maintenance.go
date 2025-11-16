package deployer

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// MaintenanceManager handles maintenance mode operations
type MaintenanceManager struct {
	client      *ssh.Client
	projectName string
	verbose     bool
}

// NewMaintenanceManager creates a new maintenance manager
func NewMaintenanceManager(client *ssh.Client, projectName string, verbose bool) *MaintenanceManager {
	return &MaintenanceManager{
		client:      client,
		projectName: projectName,
		verbose:     verbose,
	}
}

// Remove removes the maintenance page container if it exists
// This is called automatically during deployment to restore normal traffic
func (mm *MaintenanceManager) Remove(serviceName string) error {
	containerName := fmt.Sprintf("%s_%s_maintenance", mm.projectName, serviceName)

	// Check if maintenance container exists
	checkCmd := fmt.Sprintf("docker ps -a --filter name=^%s$ --format '{{.Names}}'", containerName)
	output, err := mm.client.Execute(checkCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		// No maintenance container found, nothing to do
		return nil
	}

	if mm.verbose {
		fmt.Printf("\n  Removing active maintenance mode...\n")
	}

	// Stop and remove maintenance container
	removeCmd := fmt.Sprintf("docker stop %s 2>/dev/null && docker rm %s 2>/dev/null || true", containerName, containerName)
	if _, err := mm.client.Execute(removeCmd); err != nil {
		// Log warning but don't fail deployment
		if mm.verbose {
			fmt.Printf("  Warning: Failed to remove maintenance container: %v\n", err)
		}
		return nil
	}

	// Remove maintenance directory
	maintenanceDir := fmt.Sprintf("/opt/%s/maintenance", mm.projectName)
	mm.client.Execute(fmt.Sprintf("sudo rm -rf %s 2>/dev/null || true", maintenanceDir))

	if mm.verbose {
		fmt.Printf("  ✓ Maintenance mode removed\n")
	}

	return nil
}

// IsActive checks if maintenance mode is currently active for a service
func (mm *MaintenanceManager) IsActive(serviceName string) (bool, error) {
	containerName := fmt.Sprintf("%s_%s_maintenance", mm.projectName, serviceName)
	checkCmd := fmt.Sprintf("docker ps --filter name=^%s$ --format '{{.Names}}'", containerName)
	output, err := mm.client.Execute(checkCmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == containerName, nil
}
