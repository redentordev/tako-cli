package takod

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
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
	ConfigHash string   `json:"configHash,omitempty"`
	RuntimeID  string   `json:"runtimeId,omitempty"`
	Persistent bool     `json:"persistent,omitempty"`
}

func GatherActualState(ctx context.Context, project string, environment string) (*ActualStateResponse, error) {
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if environment == "" {
		return nil, fmt.Errorf("environment is required")
	}
	if !isSafeProjectName(project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid environment name")
	}

	format := fmt.Sprintf(
		`{{.Names}}|{{.Image}}|{{.ID}}|{{.Label "tako.configHash"}}|{{.Label %q}}|{{.Label "tako.project"}}|{{.Label "tako.environment"}}|{{.Label "tako.service"}}|{{.Label "tako.persistent"}}`,
		runtimeid.ServiceIdentityLabel,
	)
	cmd := actualDockerCommandContext(ctx, "docker", "ps", "--format", format)
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

	for _, line := range strings.Split(strings.TrimSpace(dockerPSOutput), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}

		image := parts[1]
		containerID := parts[2]
		configHash := ""
		if len(parts) >= 4 {
			configHash = strings.TrimSpace(parts[3])
		}
		runtimeID := ""
		if len(parts) >= 5 {
			runtimeID = strings.TrimSpace(parts[4])
		}
		serviceName := ""
		if len(parts) >= 8 &&
			strings.TrimSpace(parts[5]) == project &&
			strings.TrimSpace(parts[6]) == environment {
			serviceName = strings.TrimSpace(parts[7])
		}
		persistent := false
		if len(parts) >= 9 {
			persistent = strings.EqualFold(strings.TrimSpace(parts[8]), "true")
		}
		if serviceName == "" {
			continue
		}
		if serviceName == "" || !isSafeServiceName(serviceName) {
			continue
		}

		if existing, ok := response.Services[serviceName]; ok {
			existing.Containers = append(existing.Containers, containerID)
			existing.Replicas++
			if existing.ConfigHash == "" {
				existing.ConfigHash = configHash
			} else if configHash != "" && existing.ConfigHash != configHash {
				existing.ConfigHash = ""
			}
			existing.RuntimeID = mergeRuntimeID(existing.RuntimeID, runtimeID)
			existing.Persistent = existing.Persistent || persistent
			continue
		}

		response.Services[serviceName] = &ActualService{
			Name:       serviceName,
			Image:      image,
			Replicas:   1,
			Containers: []string{containerID},
			ConfigHash: configHash,
			RuntimeID:  runtimeID,
			Persistent: persistent,
		}
	}

	return response
}

func mergeRuntimeID(existing string, incoming string) string {
	if existing == incoming {
		return existing
	}
	return ""
}
