package deployer

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// ContainerManager handles container lifecycle operations
type ContainerManager struct {
	client  *ssh.Client
	verbose bool
}

// NewContainerManager creates a new container manager
func NewContainerManager(client *ssh.Client, verbose bool) *ContainerManager {
	return &ContainerManager{
		client:  client,
		verbose: verbose,
	}
}

// Exists checks if a container with the given name exists (running or stopped)
func (cm *ContainerManager) Exists(containerName string) (bool, error) {
	checkCmd := fmt.Sprintf("docker ps -a --filter name=^%s$ --format '{{.Names}}'", containerName)
	output, err := cm.client.Execute(checkCmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// WaitForTrafficDrain waits for in-flight requests to complete before stopping a container
func (cm *ContainerManager) WaitForTrafficDrain(containerName string, drainSeconds int) {
	if drainSeconds <= 0 {
		drainSeconds = 30 // Default 30 seconds
	}

	if cm.verbose {
		fmt.Printf("  Draining traffic from %s (%ds)...\n", containerName, drainSeconds)
	}

	time.Sleep(time.Duration(drainSeconds) * time.Second)
}

// StopGracefully stops a container with a grace period for cleanup
func (cm *ContainerManager) StopGracefully(containerName string, gracePeriodSeconds int) error {
	if gracePeriodSeconds <= 0 {
		gracePeriodSeconds = 30 // Default 30 seconds
	}

	if cm.verbose {
		fmt.Printf("  Stopping %s gracefully (%ds grace period)...\n", containerName, gracePeriodSeconds)
	}

	// Docker stop with timeout (SIGTERM, then SIGKILL after timeout)
	stopCmd := fmt.Sprintf("docker stop -t %d %s 2>/dev/null || true", gracePeriodSeconds, containerName)
	if _, err := cm.client.Execute(stopCmd); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}

	// Remove the container
	rmCmd := fmt.Sprintf("docker rm %s 2>/dev/null || true", containerName)
	if _, err := cm.client.Execute(rmCmd); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	return nil
}

// Remove forcefully removes a container
func (cm *ContainerManager) Remove(containerName string) error {
	rmCmd := fmt.Sprintf("docker rm -f %s 2>/dev/null || true", containerName)
	_, err := cm.client.Execute(rmCmd)
	return err
}

// IsRunning checks if a container is currently running
func (cm *ContainerManager) IsRunning(containerName string) (bool, error) {
	checkCmd := fmt.Sprintf("docker ps --filter name=^%s$ --format '{{.Names}}'", containerName)
	output, err := cm.client.Execute(checkCmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == containerName, nil
}

// GetStatus returns the status of a container
func (cm *ContainerManager) GetStatus(containerName string) (string, error) {
	checkCmd := fmt.Sprintf("docker inspect %s --format '{{.State.Status}}' 2>/dev/null", containerName)
	output, err := cm.client.Execute(checkCmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Rename renames a container
func (cm *ContainerManager) Rename(oldName, newName string) error {
	renameCmd := fmt.Sprintf("docker rename %s %s", oldName, newName)
	_, err := cm.client.Execute(renameCmd)
	return err
}
