package reconcile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
)

// GatherActualState collects information about currently running services
// Returns a map of serviceName -> ActualService
func GatherActualState(
	client *ssh.Client,
	projectName string,
	environment string,
	stateMgr *localstate.Manager,
) (map[string]*ActualService, error) {

	// Check if using Swarm mode
	swarmCheck, _ := client.Execute("docker info --format '{{.Swarm.LocalNodeState}}'")
	isSwarm := strings.TrimSpace(swarmCheck) == "active"

	if isSwarm {
		// Gather from Swarm services
		return gatherSwarmState(client, projectName, environment, stateMgr)
	} else {
		// Gather from standalone containers
		return gatherContainerState(client, projectName, environment, stateMgr)
	}
}

// gatherSwarmState collects state from Docker Swarm services
func gatherSwarmState(
	client *ssh.Client,
	projectName string,
	environment string,
	stateMgr *localstate.Manager,
) (map[string]*ActualService, error) {

	actualServices := make(map[string]*ActualService)

	// List all swarm services for this project
	// Service naming: {project}_{env}_{service}
	prefix := fmt.Sprintf("%s_%s_", projectName, environment)

	output, err := client.Execute("docker service ls --format '{{.Name}}|{{.Replicas}}|{{.Image}}'")
	if err != nil {
		// If error is just "no services", return empty map (not an error)
		if strings.Contains(err.Error(), "No such") || strings.Contains(output, "Nothing found") {
			return actualServices, nil
		}
		return nil, fmt.Errorf("failed to list swarm services: %w", err)
	}

	// Handle empty output (no services running)
	output = strings.TrimSpace(output)
	if output == "" {
		return actualServices, nil
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}

		fullServiceName := parts[0]
		replicasStr := parts[1]
		image := parts[2]

		// Only process services from our project/environment
		if !strings.HasPrefix(fullServiceName, prefix) {
			continue
		}

		// Extract service name (remove prefix)
		serviceName := strings.TrimPrefix(fullServiceName, prefix)

		// Parse replicas (format: "2/2" or "1/3")
		var running, desired int
		fmt.Sscanf(replicasStr, "%d/%d", &running, &desired)

		// Get last deployed config from state (for better change detection)
		var configSnapshot *config.ServiceConfig
		if stateMgr != nil {
			deployment, err := stateMgr.GetCurrentDeployment()
			if err == nil && deployment != nil && deployment.Services != nil {
				if svcDeploy, exists := deployment.Services[serviceName]; exists && svcDeploy != nil {
					// Reconstruct config from deployment state
					configSnapshot = &config.ServiceConfig{
						Image:    svcDeploy.Image,
						Replicas: svcDeploy.Replicas,
						Port:     0, // Will be populated if available
					}
					if len(svcDeploy.Ports) > 0 {
						configSnapshot.Port = svcDeploy.Ports[0]
					}
					// Note: Env vars and other fields may not be stored in state
				}
			}
		}

		// If no snapshot from state, create basic one from runtime info
		if configSnapshot == nil {
			configSnapshot = &config.ServiceConfig{
				Image:    image,
				Replicas: desired,
			}
		}

		actualServices[serviceName] = &ActualService{
			Name:           serviceName,
			Image:          image,
			Replicas:       desired,
			Containers:     []string{}, // TODO: Get container IDs
			ConfigSnapshot: configSnapshot,
		}
	}

	return actualServices, nil
}

// gatherContainerState collects state from standalone Docker containers
func gatherContainerState(
	client *ssh.Client,
	projectName string,
	environment string,
	stateMgr *localstate.Manager,
) (map[string]*ActualService, error) {

	actualServices := make(map[string]*ActualService)

	// Container naming: {project}_{env}_{service}_{replica}
	prefix := fmt.Sprintf("%s_%s_", projectName, environment)

	output, err := client.Execute("docker ps --format '{{.Names}}|{{.Image}}|{{.ID}}'")
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	// Handle empty output (no containers running)
	output = strings.TrimSpace(output)
	if output == "" {
		return actualServices, nil
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}

		containerName := parts[0]
		image := parts[1]
		containerID := parts[2]

		// Only process containers from our project/environment
		if !strings.HasPrefix(containerName, prefix) {
			continue
		}

		// Parse container name: {project}_{env}_{service}_{replica}
		// Remove prefix to get: {service}_{replica}
		remainder := strings.TrimPrefix(containerName, prefix)
		nameParts := strings.Split(remainder, "_")

		if len(nameParts) < 2 {
			continue
		}

		// Service name is all parts except the last (replica number)
		serviceName := strings.Join(nameParts[:len(nameParts)-1], "_")

		// Add or update service entry
		if existing, exists := actualServices[serviceName]; exists {
			existing.Containers = append(existing.Containers, containerID)
			existing.Replicas++
		} else {
			// Get last deployed config from state (for better change detection)
			var configSnapshot *config.ServiceConfig
			if stateMgr != nil {
				deployment, err := stateMgr.GetCurrentDeployment()
				if err == nil && deployment != nil && deployment.Services != nil {
					if svcDeploy, exists := deployment.Services[serviceName]; exists && svcDeploy != nil {
						configSnapshot = &config.ServiceConfig{
							Image: svcDeploy.Image,
						}
						if len(svcDeploy.Ports) > 0 {
							configSnapshot.Port = svcDeploy.Ports[0]
						}
					}
				}
			}

			if configSnapshot == nil {
				configSnapshot = &config.ServiceConfig{
					Image: image,
				}
			}

			actualServices[serviceName] = &ActualService{
				Name:           serviceName,
				Image:          image,
				Replicas:       1,
				Containers:     []string{containerID},
				ConfigSnapshot: configSnapshot,
			}
		}
	}

	return actualServices, nil
}

// GetServiceInspectData retrieves detailed service information
func GetServiceInspectData(client *ssh.Client, serviceName string, isSwarm bool) (map[string]interface{}, error) {
	var cmd string
	if isSwarm {
		cmd = fmt.Sprintf("docker service inspect %s", serviceName)
	} else {
		cmd = fmt.Sprintf("docker inspect %s", serviceName)
	}

	output, err := client.Execute(cmd)
	if err != nil {
		return nil, err
	}

	var data []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &data); err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("no data returned")
	}

	return data[0], nil
}
