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
	// Jobs carries the node's scheduled jobs: kind:job services have no
	// long-running containers, so their actual state is the cron schedule.
	Jobs map[string]*JobStatus `json:"jobs,omitempty"`
}

type ActualService struct {
	Name              string            `json:"name"`
	Image             string            `json:"image,omitempty"`
	RevisionImages    map[string]string `json:"revisionImages,omitempty"`
	Replicas          int               `json:"replicas"`
	Containers        []string          `json:"containers,omitempty"`
	ConfigHash        string            `json:"configHash,omitempty"`
	RuntimeID         string            `json:"runtimeId,omitempty"`
	Persistent        bool              `json:"persistent,omitempty"`
	CurrentRevision   string            `json:"currentRevision,omitempty"`
	PreviousRevision  string            `json:"previousRevision,omitempty"`
	WarmingRevisions  []string          `json:"warmingRevisions,omitempty"`
	DeployStrategy    string            `json:"deployStrategy,omitempty"`
	ActiveContainers  []string          `json:"activeContainers,omitempty"`
	WarmingContainers []string          `json:"warmingContainers,omitempty"`
	// Health aggregates the docker health-check state of the service's
	// active containers (worst wins: unhealthy > starting > healthy).
	// Empty when no active container defines a health check, or when the
	// reporting node agent predates health capture.
	Health string `json:"health,omitempty"`
}

// Docker health-check states surfaced in actual state and status rows.
const (
	HealthStateHealthy   = "healthy"
	HealthStateStarting  = "starting"
	HealthStateUnhealthy = "unhealthy"
)

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
		`{{.Names}}|{{.Image}}|{{.ID}}|{{.Label "tako.configHash"}}|{{.Label %q}}|{{.Label "tako.project"}}|{{.Label "tako.environment"}}|{{.Label "tako.service"}}|{{.Label "tako.persistent"}}|{{.Label "tako.revision"}}|{{.Label "tako.deployStrategy"}}|{{.Label "tako.slot"}}|{{.Label "tako.active"}}|{{.Status}}`,
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
		revision := ""
		if len(parts) >= 10 {
			revision = strings.TrimSpace(parts[9])
		}
		strategy := ""
		if len(parts) >= 11 {
			strategy = strings.TrimSpace(parts[10])
		}
		active := true
		if len(parts) >= 13 && strings.TrimSpace(parts[12]) != "" {
			active = strings.EqualFold(strings.TrimSpace(parts[12]), "true")
		}
		health := ""
		if len(parts) >= 14 {
			health = ParseContainerHealth(parts[13])
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
			existing.RevisionImages = mergeRevisionImage(existing.RevisionImages, revision, image)
			existing.RuntimeID = mergeRuntimeID(existing.RuntimeID, runtimeID)
			existing.Persistent = existing.Persistent || persistent
			existing.DeployStrategy = mergeOptionalLabel(existing.DeployStrategy, strategy)
			if active {
				existing.ActiveContainers = append(existing.ActiveContainers, containerID)
				existing.CurrentRevision = mergeOptionalLabel(existing.CurrentRevision, revision)
				existing.Health = MergeHealthStates(existing.Health, health)
			} else {
				existing.WarmingContainers = append(existing.WarmingContainers, containerID)
				existing.PreviousRevision = mergeOptionalLabel(existing.PreviousRevision, revision)
				existing.WarmingRevisions = appendUniqueRevision(existing.WarmingRevisions, revision)
			}
			continue
		}

		actual := &ActualService{
			Name:           serviceName,
			Image:          image,
			RevisionImages: mergeRevisionImage(nil, revision, image),
			Replicas:       1,
			Containers:     []string{containerID},
			ConfigHash:     configHash,
			RuntimeID:      runtimeID,
			Persistent:     persistent,
		}
		actual.DeployStrategy = strategy
		if active {
			actual.ActiveContainers = []string{containerID}
			actual.CurrentRevision = revision
			actual.Health = health
		} else {
			actual.WarmingContainers = []string{containerID}
			actual.PreviousRevision = revision
			actual.WarmingRevisions = appendUniqueRevision(actual.WarmingRevisions, revision)
		}
		response.Services[serviceName] = actual
	}

	finalizeActualServiceRevisionStates(response.Services)
	return response
}

func finalizeActualServiceRevisionStates(services map[string]*ActualService) {
	for _, service := range services {
		if service == nil || len(service.ActiveContainers) > 0 || service.CurrentRevision != "" {
			continue
		}
		if len(service.WarmingRevisions) != 1 {
			continue
		}
		service.CurrentRevision = service.WarmingRevisions[0]
		service.PreviousRevision = ""
		service.ActiveContainers = append([]string(nil), service.WarmingContainers...)
		service.WarmingContainers = nil
		service.WarmingRevisions = nil
	}
}

// ParseContainerHealth extracts the docker health-check state from a
// `docker ps` status column value such as "Up 5 minutes (healthy)".
// Containers without a health check report no parenthesized state and yield
// the empty string.
func ParseContainerHealth(dockerStatus string) string {
	status := strings.ToLower(strings.TrimSpace(dockerStatus))
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return HealthStateUnhealthy
	case strings.Contains(status, "(health: starting)"):
		return HealthStateStarting
	case strings.Contains(status, "(healthy)"):
		return HealthStateHealthy
	default:
		return ""
	}
}

// MergeHealthStates aggregates two health-check states with worst-wins
// semantics: unhealthy > starting > healthy > unknown (empty).
func MergeHealthStates(existing string, incoming string) string {
	if healthStateRank(incoming) > healthStateRank(existing) {
		return incoming
	}
	return existing
}

func healthStateRank(state string) int {
	switch state {
	case HealthStateUnhealthy:
		return 3
	case HealthStateStarting:
		return 2
	case HealthStateHealthy:
		return 1
	default:
		return 0
	}
}

func appendUniqueRevision(revisions []string, revision string) []string {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return revisions
	}
	for _, existing := range revisions {
		if existing == revision {
			return revisions
		}
	}
	return append(revisions, revision)
}

func mergeRevisionImage(images map[string]string, revision string, image string) map[string]string {
	revision = strings.TrimSpace(revision)
	image = strings.TrimSpace(image)
	if revision == "" || image == "" {
		return images
	}
	if images == nil {
		images = make(map[string]string)
	}
	if existing := images[revision]; existing != "" && existing != image {
		images[revision] = ""
		return images
	}
	images[revision] = image
	return images
}

func mergeOptionalLabel(existing string, incoming string) string {
	if incoming == "" {
		return existing
	}
	if existing == "" {
		return incoming
	}
	if existing == incoming {
		return existing
	}
	return ""
}

func mergeRuntimeID(existing string, incoming string) string {
	if existing == incoming {
		return existing
	}
	return ""
}
