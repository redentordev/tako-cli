package takod

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

var (
	discoveryRoundRobinMu      sync.Mutex
	discoveryRoundRobinOffsets = map[string]int{}
)

type DiscoveryRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service,omitempty"`
	Port        int    `json:"port,omitempty"`
	RoundRobin  bool   `json:"roundRobin,omitempty"`
}

type DiscoveryResponse struct {
	Project     string              `json:"project"`
	Environment string              `json:"environment"`
	Node        string              `json:"node,omitempty"`
	Services    []DiscoveryService  `json:"services"`
	Endpoints   []DiscoveryEndpoint `json:"endpoints,omitempty"`
}

type DiscoveryService struct {
	Service   string              `json:"service"`
	Endpoints []DiscoveryEndpoint `json:"endpoints"`
}

type DiscoveryEndpoint struct {
	Service     string `json:"service"`
	Container   string `json:"container"`
	ContainerID string `json:"containerId"`
	Host        string `json:"host"`
	Port        int    `json:"port,omitempty"`
	Address     string `json:"address,omitempty"`
	Scope       string `json:"scope"`
	Healthy     bool   `json:"healthy"`
}

func ResolveDiscovery(ctx context.Context, req DiscoveryRequest, nodeName string) (*DiscoveryResponse, error) {
	normalizeDiscoveryRequest(&req)
	if err := validateDiscoveryRequest(req); err != nil {
		return nil, err
	}

	containers, err := listDiscoveryContainers(ctx, req)
	if err != nil {
		return nil, err
	}

	response := &DiscoveryResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Node:        nodeName,
	}
	if len(containers) == 0 {
		return response, nil
	}

	inspected, err := inspectProxyTargetContainers(ctx, containers)
	if err != nil {
		return nil, err
	}

	networkName := runtimeid.NetworkName(req.Project, req.Environment)
	byService := make(map[string][]DiscoveryEndpoint)
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
		service := strings.TrimSpace(container.Config.Labels["tako.service"])
		if service == "" || !isSafeServiceName(service) {
			continue
		}
		if req.Service != "" && service != req.Service {
			continue
		}

		endpoint := DiscoveryEndpoint{
			Service:     service,
			Container:   strings.TrimPrefix(container.Name, "/"),
			ContainerID: container.ID,
			Host:        host,
			Port:        req.Port,
			Scope:       "local",
			Healthy:     true,
		}
		if req.Port > 0 {
			endpoint.Address = net.JoinHostPort(host, fmt.Sprintf("%d", req.Port))
			if meshHost, meshPort, ok := discoveryMeshPublishedEndpoint(container, req.Port); ok {
				endpoint.Host = meshHost
				endpoint.Address = net.JoinHostPort(meshHost, fmt.Sprintf("%d", meshPort))
				endpoint.Scope = "mesh"
			}
		}
		byService[service] = append(byService[service], endpoint)
	}

	serviceNames := make([]string, 0, len(byService))
	for service := range byService {
		serviceNames = append(serviceNames, service)
	}
	sort.Strings(serviceNames)

	for _, service := range serviceNames {
		endpoints := byService[service]
		sort.SliceStable(endpoints, func(i, j int) bool {
			if endpoints[i].Container == endpoints[j].Container {
				return endpoints[i].ContainerID < endpoints[j].ContainerID
			}
			return endpoints[i].Container < endpoints[j].Container
		})
		if req.RoundRobin {
			endpoints = rotateDiscoveryEndpoints(endpoints, nextDiscoveryRotationOffset(req.Project, req.Environment, service))
		}
		response.Services = append(response.Services, DiscoveryService{
			Service:   service,
			Endpoints: endpoints,
		})
		response.Endpoints = append(response.Endpoints, endpoints...)
	}

	return response, nil
}

func normalizeDiscoveryRequest(req *DiscoveryRequest) {
	req.Project = strings.TrimSpace(req.Project)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Service = strings.TrimSpace(req.Service)
}

func validateDiscoveryRequest(req DiscoveryRequest) error {
	if req.Project == "" {
		return fmt.Errorf("project is required")
	}
	if req.Environment == "" {
		return fmt.Errorf("environment is required")
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if req.Service != "" && !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if req.Port < 0 || req.Port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	return nil
}

func listDiscoveryContainers(ctx context.Context, req DiscoveryRequest) ([]string, error) {
	args := []string{
		"ps",
		"--filter", "label=tako.project=" + req.Project,
		"--filter", "label=tako.environment=" + req.Environment,
	}
	if req.Service != "" {
		args = append(args, "--filter", "label=tako.service="+req.Service)
	}
	args = append(args, "--format", "{{.Names}}")

	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list discovery containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(output))
	sort.Strings(containers)
	return containers, nil
}

func discoveryMeshPublishedEndpoint(container dockerProxyInspectContainer, targetPort int) (string, int, bool) {
	if targetPort <= 0 {
		return "", 0, false
	}
	bindings := container.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", targetPort)]
	for _, binding := range bindings {
		host := strings.TrimSpace(binding.HostIP)
		hostPort := strings.TrimSpace(binding.HostPort)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		port, err := strconv.Atoi(hostPort)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		return ip.String(), port, true
	}
	return "", 0, false
}

func nextDiscoveryRotationOffset(project string, environment string, service string) int {
	key := project + "/" + environment + "/" + service
	discoveryRoundRobinMu.Lock()
	defer discoveryRoundRobinMu.Unlock()
	offset := discoveryRoundRobinOffsets[key]
	discoveryRoundRobinOffsets[key] = offset + 1
	return offset
}

func rotateDiscoveryEndpoints(endpoints []DiscoveryEndpoint, offset int) []DiscoveryEndpoint {
	if len(endpoints) <= 1 {
		return endpoints
	}
	shift := offset % len(endpoints)
	if shift == 0 {
		return endpoints
	}
	out := make([]DiscoveryEndpoint, 0, len(endpoints))
	out = append(out, endpoints[shift:]...)
	out = append(out, endpoints[:shift]...)
	return out
}
