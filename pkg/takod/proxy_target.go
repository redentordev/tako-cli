package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

type ProxyTargetRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Port        int    `json:"port"`
}

type ProxyTargetResponse struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Container   string `json:"container"`
	ContainerID string `json:"containerId"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Address     string `json:"address"`
}

type dockerProxyInspectContainer struct {
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	Config          dockerProxyInspectConfig
	State           dockerProxyInspectState
	NetworkSettings dockerProxyInspectNetworkSettings
}

type dockerProxyInspectConfig struct {
	Labels map[string]string `json:"Labels"`
}

type dockerProxyInspectState struct {
	Running bool                      `json:"Running"`
	Health  *dockerProxyInspectHealth `json:"Health,omitempty"`
}

type dockerProxyInspectHealth struct {
	Status string `json:"Status"`
}

type dockerProxyInspectNetworkSettings struct {
	Networks map[string]dockerProxyInspectNetwork `json:"Networks"`
	Ports    map[string][]dockerPortBinding       `json:"Ports"`
}

type dockerProxyInspectNetwork struct {
	IPAddress string `json:"IPAddress"`
}

func ResolveProxyTarget(ctx context.Context, req ProxyTargetRequest) (*ProxyTargetResponse, error) {
	if err := validateProxyTargetRequest(req); err != nil {
		return nil, err
	}

	containers, err := listProxyTargetContainers(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("no running containers found for service %s", req.Service)
	}

	inspected, err := inspectProxyTargetContainers(ctx, containers)
	if err != nil {
		return nil, err
	}

	networkName := runtimeid.NetworkName(req.Project, req.Environment)
	for _, container := range inspected {
		if !container.State.Running {
			continue
		}
		if container.State.Health != nil && container.State.Health.Status != "healthy" {
			continue
		}
		host := proxyTargetContainerIP(container, networkName)
		if host == "" {
			continue
		}
		containerName := strings.TrimPrefix(container.Name, "/")
		return &ProxyTargetResponse{
			Project:     req.Project,
			Environment: req.Environment,
			Service:     req.Service,
			Container:   containerName,
			ContainerID: container.ID,
			Host:        host,
			Port:        req.Port,
			Address:     net.JoinHostPort(host, fmt.Sprintf("%d", req.Port)),
		}, nil
	}

	return nil, fmt.Errorf("no healthy network-reachable containers found for service %s", req.Service)
}

func validateProxyTargetRequest(req ProxyTargetRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if req.Port < 1 || req.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func listProxyTargetContainers(ctx context.Context, req ProxyTargetRequest) ([]string, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--filter", "label=tako.service="+req.Service,
		"--format", "{{.Names}}",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list service containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(output))
	sort.Strings(containers)
	return containers, nil
}

func inspectProxyTargetContainers(ctx context.Context, containers []string) ([]dockerProxyInspectContainer, error) {
	args := append([]string{"inspect"}, containers...)
	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect service containers: %w", err)
	}
	var inspected []dockerProxyInspectContainer
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		return nil, fmt.Errorf("failed to parse container inspect output: %w", err)
	}
	sort.SliceStable(inspected, func(i, j int) bool {
		left := strings.TrimPrefix(inspected[i].Name, "/")
		right := strings.TrimPrefix(inspected[j].Name, "/")
		if left == right {
			return inspected[i].ID < inspected[j].ID
		}
		return left < right
	})
	return inspected, nil
}

func proxyTargetContainerIP(container dockerProxyInspectContainer, preferredNetwork string) string {
	if network, ok := container.NetworkSettings.Networks[preferredNetwork]; ok && network.IPAddress != "" {
		return network.IPAddress
	}
	return ""
}
