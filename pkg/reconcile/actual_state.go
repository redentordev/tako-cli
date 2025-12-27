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

// ActualServiceDetails contains detailed actual state of a service
type ActualServiceDetails struct {
	Name     string
	Image    string
	Replicas int
	Env      map[string]string
	Volumes  []string // Mount targets
	Labels   map[string]string
}

// GatherActualServiceDetails retrieves full details for a service
func GatherActualServiceDetails(client *ssh.Client, fullServiceName string) (*ActualServiceDetails, error) {
	details := &ActualServiceDetails{
		Name:    fullServiceName,
		Env:     make(map[string]string),
		Volumes: []string{},
		Labels:  make(map[string]string),
	}

	// Get service inspect data
	cmd := fmt.Sprintf("docker service inspect %s --format '{{json .}}'", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect service: %w", err)
	}

	var serviceData struct {
		Spec struct {
			Mode struct {
				Replicated struct {
					Replicas int `json:"Replicas"`
				} `json:"Replicated"`
			} `json:"Mode"`
			TaskTemplate struct {
				ContainerSpec struct {
					Image  string   `json:"Image"`
					Env    []string `json:"Env"`
					Mounts []struct {
						Type   string `json:"Type"`
						Source string `json:"Source"`
						Target string `json:"Target"`
					} `json:"Mounts"`
				} `json:"ContainerSpec"`
			} `json:"TaskTemplate"`
			Labels map[string]string `json:"Labels"`
		} `json:"Spec"`
	}

	if err := json.Unmarshal([]byte(output), &serviceData); err != nil {
		return nil, fmt.Errorf("failed to parse service data: %w", err)
	}

	details.Image = serviceData.Spec.TaskTemplate.ContainerSpec.Image
	details.Replicas = serviceData.Spec.Mode.Replicated.Replicas
	details.Labels = serviceData.Spec.Labels

	// Parse environment variables
	for _, envVar := range serviceData.Spec.TaskTemplate.ContainerSpec.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) == 2 {
			details.Env[parts[0]] = parts[1]
		}
	}

	// Extract mount targets
	for _, mount := range serviceData.Spec.TaskTemplate.ContainerSpec.Mounts {
		details.Volumes = append(details.Volumes, mount.Target)
	}

	return details, nil
}

// DriftChange represents a detected drift in a service
type DriftChange struct {
	Field        string // "env", "volume", "label", "replicas", "image"
	Key          string // Environment variable name, volume path, label key, etc.
	ActualValue  string
	DesiredValue string
	IsManual     bool // True if this appears to be a manual change
}

// DetectDrift compares actual state with desired state and returns drift changes
func DetectDrift(actual *ActualServiceDetails, desired *config.ServiceConfig) []DriftChange {
	var drifts []DriftChange

	// Compare environment variables
	// Check for env vars in actual that aren't in desired (manual additions)
	for key, actualValue := range actual.Env {
		desiredValue, exists := desired.Env[key]
		if !exists {
			drifts = append(drifts, DriftChange{
				Field:        "env",
				Key:          key,
				ActualValue:  actualValue,
				DesiredValue: "",
				IsManual:     true,
			})
		} else if actualValue != desiredValue {
			drifts = append(drifts, DriftChange{
				Field:        "env",
				Key:          key,
				ActualValue:  actualValue,
				DesiredValue: desiredValue,
				IsManual:     true,
			})
		}
	}

	// Check for env vars in desired that aren't in actual (missing)
	for key, desiredValue := range desired.Env {
		if _, exists := actual.Env[key]; !exists {
			drifts = append(drifts, DriftChange{
				Field:        "env",
				Key:          key,
				ActualValue:  "",
				DesiredValue: desiredValue,
				IsManual:     false,
			})
		}
	}

	// Compare replicas
	desiredReplicas := desired.Replicas
	if desiredReplicas <= 0 {
		desiredReplicas = 1
	}
	if actual.Replicas != desiredReplicas {
		drifts = append(drifts, DriftChange{
			Field:        "replicas",
			Key:          "",
			ActualValue:  fmt.Sprintf("%d", actual.Replicas),
			DesiredValue: fmt.Sprintf("%d", desiredReplicas),
			IsManual:     true,
		})
	}

	// Compare volumes - check for manually added volumes
	desiredVolumesSet := make(map[string]bool)
	for _, vol := range desired.Volumes {
		parts := strings.Split(vol, ":")
		if len(parts) >= 2 {
			desiredVolumesSet[parts[1]] = true // Target path
		} else if len(parts) == 1 {
			desiredVolumesSet[parts[0]] = true
		}
	}

	for _, actualTarget := range actual.Volumes {
		if !desiredVolumesSet[actualTarget] {
			drifts = append(drifts, DriftChange{
				Field:        "volume",
				Key:          actualTarget,
				ActualValue:  actualTarget,
				DesiredValue: "",
				IsManual:     true,
			})
		}
	}

	// Compare labels (only non-traefik, non-tako labels)
	for key, actualValue := range actual.Labels {
		if strings.HasPrefix(key, "traefik.") || strings.HasPrefix(key, "tako.") {
			continue // Skip managed labels
		}
		// This is a manually added label
		drifts = append(drifts, DriftChange{
			Field:        "label",
			Key:          key,
			ActualValue:  actualValue,
			DesiredValue: "",
			IsManual:     true,
		})
	}

	return drifts
}
