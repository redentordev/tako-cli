package registry

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// ProjectInfo holds metadata about a deployed project
type ProjectInfo struct {
	Name        string    `json:"name"`
	Environment string    `json:"environment"`
	Network     string    `json:"network"`
	Services    []string  `json:"services"`
	Domains     []string  `json:"domains"`
	DeployedAt  time.Time `json:"deployed_at"`
}

// Registry manages the list of deployed projects on a server
type Registry struct {
	client  *ssh.Client
	verbose bool
}

// NewRegistry creates a new project registry
func NewRegistry(client *ssh.Client, verbose bool) *Registry {
	return &Registry{
		client:  client,
		verbose: verbose,
	}
}

// RegisterProject adds or updates a project in the registry
func (r *Registry) RegisterProject(info ProjectInfo) error {
	if r.verbose {
		fmt.Printf("  Registering project %s (%s)...\n", info.Name, info.Environment)
	}

	// Store project info using Docker labels on a marker container
	// This is a lightweight way to persist metadata without external storage
	markerName := fmt.Sprintf("tako_registry_%s_%s", info.Name, info.Environment)

	// Check if marker already exists
	checkCmd := fmt.Sprintf("docker ps -a --filter name=^%s$ --format '{{.Names}}'", markerName)
	existing, _ := r.client.Execute(checkCmd)

	// Remove existing marker if present
	if strings.TrimSpace(existing) == markerName {
		r.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", markerName))
	}

	// Serialize project info to JSON
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to serialize project info: %w", err)
	}

	// Create marker container with project metadata as labels
	// Using busybox:latest as it's small and universally available
	createCmd := fmt.Sprintf(
		`docker create --name %s --label "tako.registry=true" --label "tako.project=%s" --label "tako.environment=%s" --label "tako.network=%s" --label "tako.info=%s" busybox:latest true`,
		markerName,
		info.Name,
		info.Environment,
		info.Network,
		strings.ReplaceAll(string(infoJSON), `"`, `\"`),
	)

	if _, err := r.client.Execute(createCmd); err != nil {
		return fmt.Errorf("failed to create registry marker: %w", err)
	}

	if r.verbose {
		fmt.Printf("  ✓ Project registered\n")
	}

	return nil
}

// GetProject retrieves project information from the registry
func (r *Registry) GetProject(name, environment string) (*ProjectInfo, error) {
	markerName := fmt.Sprintf("tako_registry_%s_%s", name, environment)

	// Get project info from container labels
	infoCmd := fmt.Sprintf("docker inspect %s --format '{{index .Config.Labels \"tako.info\"}}' 2>/dev/null", markerName)
	infoJSON, err := r.client.Execute(infoCmd)
	if err != nil {
		return nil, fmt.Errorf("project %s (%s) not found in registry", name, environment)
	}

	// Deserialize JSON
	var info ProjectInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(infoJSON)), &info); err != nil {
		return nil, fmt.Errorf("failed to parse project info: %w", err)
	}

	return &info, nil
}

// ListProjects returns a list of all registered projects
func (r *Registry) ListProjects() ([]ProjectInfo, error) {
	// Find all registry marker containers
	listCmd := "docker ps -a --filter label=tako.registry=true --format '{{.Names}}'"
	output, err := r.client.Execute(listCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list registry markers: %w", err)
	}

	markers := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			markers = append(markers, line)
		}
	}

	if len(markers) == 0 {
		return []ProjectInfo{}, nil
	}

	// Get info for each marker
	projects := []ProjectInfo{}
	for _, marker := range markers {
		infoCmd := fmt.Sprintf("docker inspect %s --format '{{index .Config.Labels \"tako.info\"}}' 2>/dev/null", marker)
		infoJSON, err := r.client.Execute(infoCmd)
		if err != nil {
			if r.verbose {
				fmt.Printf("  Warning: Failed to get info for %s: %v\n", marker, err)
			}
			continue
		}

		var info ProjectInfo
		if err := json.Unmarshal([]byte(strings.TrimSpace(infoJSON)), &info); err != nil {
			if r.verbose {
				fmt.Printf("  Warning: Failed to parse info for %s: %v\n", marker, err)
			}
			continue
		}

		projects = append(projects, info)
	}

	return projects, nil
}

// UnregisterProject removes a project from the registry
func (r *Registry) UnregisterProject(name, environment string) error {
	markerName := fmt.Sprintf("tako_registry_%s_%s", name, environment)

	if r.verbose {
		fmt.Printf("  Unregistering project %s (%s)...\n", name, environment)
	}

	// Remove marker container
	r.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", markerName))

	if r.verbose {
		fmt.Printf("  ✓ Project unregistered\n")
	}

	return nil
}

// GetAllProjectNetworks returns a list of all networks used by registered projects
func (r *Registry) GetAllProjectNetworks() ([]string, error) {
	projects, err := r.ListProjects()
	if err != nil {
		return nil, err
	}

	networks := []string{}
	networkMap := make(map[string]bool)
	for _, project := range projects {
		if project.Network != "" && !networkMap[project.Network] {
			networks = append(networks, project.Network)
			networkMap[project.Network] = true
		}
	}

	return networks, nil
}
