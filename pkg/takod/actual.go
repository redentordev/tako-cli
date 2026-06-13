package takod

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

var actualDockerCommandContext = exec.CommandContext

type ActualStateResponse struct {
	Project     string                    `json:"project"`
	Environment string                    `json:"environment"`
	Services    map[string]*ActualService `json:"services"`
}

type ActualService struct {
	Name       string   `json:"name"`
	Image      string   `json:"image,omitempty"`
	Replicas   int      `json:"replicas"`
	Containers []string `json:"containers,omitempty"`
}

func GatherActualState(ctx context.Context, project string, environment string) (*ActualStateResponse, error) {
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if environment == "" {
		return nil, fmt.Errorf("environment is required")
	}

	cmd := actualDockerCommandContext(ctx, "docker", "ps", "--format", "{{.Names}}|{{.Image}}|{{.ID}}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	return ParseActualState(project, environment, string(output)), nil
}

func ParseActualState(project string, environment string, dockerPSOutput string) *ActualStateResponse {
	response := &ActualStateResponse{
		Project:     project,
		Environment: environment,
		Services:    make(map[string]*ActualService),
	}

	prefix := fmt.Sprintf("%s_%s_", project, environment)
	for _, line := range strings.Split(strings.TrimSpace(dockerPSOutput), "\n") {
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
		if !strings.HasPrefix(containerName, prefix) {
			continue
		}

		remainder := strings.TrimPrefix(containerName, prefix)
		nameParts := strings.Split(remainder, "_")
		if len(nameParts) < 2 {
			continue
		}
		serviceName := strings.Join(nameParts[:len(nameParts)-1], "_")

		if existing, ok := response.Services[serviceName]; ok {
			existing.Containers = append(existing.Containers, containerID)
			existing.Replicas++
			continue
		}

		response.Services[serviceName] = &ActualService{
			Name:       serviceName,
			Image:      image,
			Replicas:   1,
			Containers: []string{containerID},
		}
	}

	return response
}
