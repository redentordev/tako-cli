package reconcile

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// GatherActualState collects information about currently running services
// Returns a map of serviceName -> ActualService
func GatherActualState(
	client *ssh.Client,
	projectName string,
	environment string,
	stateMgr *localstate.Manager,
) (map[string]*ActualService, error) {

	return gatherContainerState(client, projectName, environment, stateMgr)
}

// GatherActualStateFromServers collects takod container state from every
// selected node and aggregates replicas by service.
func GatherActualStateFromServers(
	sshPool *ssh.Pool,
	cfg *config.Config,
	environment string,
	serverNames []string,
	stateMgr *localstate.Manager,
) (map[string]*ActualService, error) {
	actualServices := make(map[string]*ActualService)

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found", serverName)
		}

		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		nodeState, err := gatherActualStateFromTakod(client, cfg, environment)
		if err != nil {
			nodeState, err = GatherActualState(client, cfg.Project.Name, environment, stateMgr)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to gather actual state from %s: %w", serverName, err)
		}

		for serviceName, serviceState := range nodeState {
			if existing, ok := actualServices[serviceName]; ok {
				existing.Replicas += serviceState.Replicas
				existing.Containers = append(existing.Containers, serviceState.Containers...)
				continue
			}
			actualServices[serviceName] = serviceState
		}
	}

	return actualServices, nil
}

func gatherActualStateFromTakod(client *ssh.Client, cfg *config.Config, environment string) (map[string]*ActualService, error) {
	socket := "/run/tako/takod.sock"
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		socket = cfg.Runtime.Agent.Socket
	}

	url := fmt.Sprintf(
		"http://takod/v1/actual?project=%s&environment=%s",
		queryEscape(cfg.Project.Name),
		queryEscape(environment),
	)
	cmd := fmt.Sprintf(
		"if test -S %[1]s && command -v curl >/dev/null 2>&1; then curl --fail --silent --show-error --unix-socket %[1]s %[2]s; else exit 42; fi",
		shellQuote(socket),
		shellQuote(url),
	)
	output, err := client.Execute(cmd)
	if err != nil {
		return nil, err
	}

	var response takod.ActualStateResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod actual state: %w", err)
	}

	actualServices := make(map[string]*ActualService, len(response.Services))
	for serviceName, service := range response.Services {
		if service == nil {
			continue
		}
		actualServices[serviceName] = &ActualService{
			Name:       service.Name,
			Image:      service.Image,
			Replicas:   service.Replicas,
			Containers: append([]string(nil), service.Containers...),
			ConfigSnapshot: &config.ServiceConfig{
				Image: service.Image,
			},
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

func queryEscape(value string) string {
	return url.QueryEscape(value)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
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

	containerCmd := fmt.Sprintf("docker ps --filter 'name=%s_' --format '{{.Names}}'", fullServiceName)
	containersOutput, err := client.Execute(containerCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list service containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(containersOutput))
	if len(containers) == 0 {
		return nil, fmt.Errorf("no running containers found for %s", fullServiceName)
	}
	details.Replicas = len(containers)

	var serviceData struct {
		Config struct {
			Image  string            `json:"Image"`
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		Mounts []struct {
			Type   string `json:"Type"`
			Source string `json:"Source"`
			Target string `json:"Destination"`
		} `json:"Mounts"`
	}

	output, err := client.Execute(fmt.Sprintf("docker inspect %s --format '{{json .}}'", containers[0]))
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	if err := json.Unmarshal([]byte(output), &serviceData); err != nil {
		return nil, fmt.Errorf("failed to parse container data: %w", err)
	}

	details.Image = serviceData.Config.Image
	details.Labels = serviceData.Config.Labels

	// Parse environment variables
	for _, envVar := range serviceData.Config.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) == 2 {
			details.Env[parts[0]] = parts[1]
		}
	}

	// Extract mount targets
	for _, mount := range serviceData.Mounts {
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
