package deployer

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// ReplicaManager handles replica cleanup and management
type ReplicaManager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewReplicaManager creates a new replica manager
func NewReplicaManager(client *ssh.Client, projectName, environment string, verbose bool) *ReplicaManager {
	return &ReplicaManager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// CleanupOld removes replica containers that exceed the desired count
func (rm *ReplicaManager) CleanupOld(serviceName string, desiredCount int) error {
	if rm.verbose {
		fmt.Printf("\n  Cleaning up old replicas...\n")
	}

	// Find all containers for this service in this environment
	// Pattern: {project}_{environment}_{service}_{number}
	containerPattern := fmt.Sprintf("%s_%s_%s_", rm.projectName, rm.environment, serviceName)

	// List all containers matching the pattern
	listCmd := fmt.Sprintf("docker ps -a --filter name=%s --format '{{.Names}}'", containerPattern)
	output, err := rm.client.Execute(listCmd)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		if rm.verbose {
			fmt.Printf("  No old replicas to clean up\n")
		}
		return nil
	}

	// Parse container names and find those exceeding desired count
	containers := strings.Split(strings.TrimSpace(output), "\n")
	removed := 0

	for _, containerName := range containers {
		containerName = strings.TrimSpace(containerName)
		if containerName == "" {
			continue
		}

		// Extract replica number from container name
		// Format: {project}_{environment}_{service}_{replica}
		parts := strings.Split(containerName, "_")
		if len(parts) < 4 {
			continue
		}

		var replicaNum int
		_, err := fmt.Sscanf(parts[len(parts)-1], "%d", &replicaNum)
		if err != nil {
			// Not a numbered replica, skip
			continue
		}

		// Remove if replica number exceeds desired count
		if replicaNum > desiredCount {
			if rm.verbose {
				fmt.Printf("  Removing old replica: %s\n", containerName)
			}

			stopCmd := fmt.Sprintf("docker stop %s 2>/dev/null || true", containerName)
			rm.client.Execute(stopCmd)

			rmCmd := fmt.Sprintf("docker rm %s 2>/dev/null || true", containerName)
			if _, err := rm.client.Execute(rmCmd); err != nil {
				if rm.verbose {
					fmt.Printf("  Warning: Failed to remove %s: %v\n", containerName, err)
				}
				continue
			}

			removed++
		}
	}

	if removed > 0 {
		if rm.verbose {
			fmt.Printf("  ✓ Removed %d old replica(s)\n", removed)
		}
	} else {
		if rm.verbose {
			fmt.Printf("  No old replicas to remove\n")
		}
	}

	return nil
}

// GetContainerName returns the standard container name for a replica
func (rm *ReplicaManager) GetContainerName(serviceName string, replicaNum int) string {
	return fmt.Sprintf("%s_%s_%s_%d", rm.projectName, rm.environment, serviceName, replicaNum)
}
