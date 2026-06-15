package takod

import (
	"context"
	"fmt"
	"strings"
)

const maxVolumeInspectNames = 256

type VolumeInspectRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Volumes     []string `json:"volumes"`
}

type VolumeInspectResponse struct {
	Project     string          `json:"project"`
	Environment string          `json:"environment"`
	Volumes     map[string]bool `json:"volumes"`
}

func InspectVolumes(ctx context.Context, req VolumeInspectRequest) (*VolumeInspectResponse, error) {
	if err := validateVolumeInspectRequest(req); err != nil {
		return nil, err
	}
	output, err := runDocker(ctx, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("failed to list docker volumes: %w", err)
	}

	existing := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			existing[name] = true
		}
	}

	volumes := make(map[string]bool, len(req.Volumes))
	for _, name := range req.Volumes {
		volumes[name] = existing[name]
	}
	return &VolumeInspectResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Volumes:     volumes,
	}, nil
}

func validateVolumeInspectRequest(req VolumeInspectRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if len(req.Volumes) > maxVolumeInspectNames {
		return fmt.Errorf("too many volumes")
	}
	seen := make(map[string]bool, len(req.Volumes))
	for _, name := range req.Volumes {
		if !isSafeDockerVolumeName(name) {
			return fmt.Errorf("invalid volume name")
		}
		if seen[name] {
			return fmt.Errorf("duplicate volume name %s", name)
		}
		seen[name] = true
	}
	return nil
}
