package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type InspectRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service,omitempty"`
}

type InspectResponse struct {
	Project     string             `json:"project"`
	Environment string             `json:"environment"`
	Node        string             `json:"node,omitempty"`
	Services    []InspectService   `json:"services"`
	Containers  []InspectContainer `json:"containers,omitempty"`
}

type InspectService struct {
	Service    string             `json:"service"`
	Containers []InspectContainer `json:"containers"`
}

type InspectContainer struct {
	ID         string           `json:"id"`
	ShortID    string           `json:"shortId"`
	Name       string           `json:"name"`
	Service    string           `json:"service"`
	Slot       int              `json:"slot,omitempty"`
	Image      string           `json:"image,omitempty"`
	ImageID    string           `json:"imageId,omitempty"`
	State      string           `json:"state"`
	Running    bool             `json:"running"`
	Health     string           `json:"health,omitempty"`
	ExitCode   int              `json:"exitCode,omitempty"`
	StartedAt  string           `json:"startedAt,omitempty"`
	FinishedAt string           `json:"finishedAt,omitempty"`
	ConfigHash string           `json:"configHash,omitempty"`
	RuntimeID  string           `json:"runtimeId,omitempty"`
	Ports      []InspectPort    `json:"ports,omitempty"`
	Mounts     []InspectMount   `json:"mounts,omitempty"`
	Networks   []InspectNetwork `json:"networks,omitempty"`
}

type InspectPort struct {
	PrivatePort int    `json:"privatePort"`
	Protocol    string `json:"protocol"`
	HostIP      string `json:"hostIp,omitempty"`
	HostPort    string `json:"hostPort,omitempty"`
}

type InspectMount struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

type InspectNetwork struct {
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress,omitempty"`
}

type dockerInspectContainer struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health,omitempty"`
	} `json:"State"`
	NetworkSettings struct {
		Ports    map[string][]dockerInspectPortBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

type dockerInspectPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func InspectProject(ctx context.Context, req InspectRequest, nodeName string) (*InspectResponse, error) {
	normalizeInspectRequest(&req)
	if err := validateInspectRequest(req); err != nil {
		return nil, err
	}

	containers, err := listInspectContainers(ctx, req)
	if err != nil {
		return nil, err
	}

	response := &InspectResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Node:        nodeName,
	}
	if len(containers) == 0 {
		return response, nil
	}

	inspected, err := inspectDockerContainers(ctx, containers)
	if err != nil {
		return nil, err
	}
	return inspectResponseFromDocker(req, nodeName, inspected), nil
}

func normalizeInspectRequest(req *InspectRequest) {
	req.Project = strings.TrimSpace(req.Project)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Service = strings.TrimSpace(req.Service)
}

func validateInspectRequest(req InspectRequest) error {
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
	return nil
}

func listInspectContainers(ctx context.Context, req InspectRequest) ([]string, error) {
	args := []string{
		"ps", "-a",
		"--filter", "label=tako.project=" + req.Project,
		"--filter", "label=tako.environment=" + req.Environment,
	}
	if req.Service != "" {
		args = append(args, "--filter", "label=tako.service="+req.Service)
	}
	args = append(args, "--format", "{{.Names}}")

	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list inspect containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(output))
	sort.Strings(containers)
	return containers, nil
}

func inspectDockerContainers(ctx context.Context, containers []string) ([]dockerInspectContainer, error) {
	if len(containers) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect"}, containers...)
	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect containers: %w", err)
	}
	var inspected []dockerInspectContainer
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

func inspectResponseFromDocker(req InspectRequest, nodeName string, inspected []dockerInspectContainer) *InspectResponse {
	response := &InspectResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Node:        nodeName,
	}

	byService := make(map[string][]InspectContainer)
	for _, container := range inspected {
		labels := container.Config.Labels
		if labels["tako.project"] != req.Project || labels["tako.environment"] != req.Environment {
			continue
		}
		service := strings.TrimSpace(labels["tako.service"])
		if service == "" || !isSafeServiceName(service) {
			continue
		}
		if req.Service != "" && service != req.Service {
			continue
		}
		item := inspectContainerFromDocker(container, service)
		byService[service] = append(byService[service], item)
		response.Containers = append(response.Containers, item)
	}

	sort.SliceStable(response.Containers, func(i, j int) bool {
		if response.Containers[i].Service == response.Containers[j].Service {
			return response.Containers[i].Name < response.Containers[j].Name
		}
		return response.Containers[i].Service < response.Containers[j].Service
	})

	serviceNames := make([]string, 0, len(byService))
	for service := range byService {
		serviceNames = append(serviceNames, service)
	}
	sort.Strings(serviceNames)
	for _, service := range serviceNames {
		containers := byService[service]
		sort.SliceStable(containers, func(i, j int) bool {
			if containers[i].Slot == containers[j].Slot {
				return containers[i].Name < containers[j].Name
			}
			return containers[i].Slot < containers[j].Slot
		})
		response.Services = append(response.Services, InspectService{
			Service:    service,
			Containers: containers,
		})
	}
	return response
}

func inspectContainerFromDocker(container dockerInspectContainer, service string) InspectContainer {
	labels := container.Config.Labels
	item := InspectContainer{
		ID:         container.ID,
		ShortID:    shortContainerID(container.ID),
		Name:       strings.TrimPrefix(container.Name, "/"),
		Service:    service,
		Slot:       parseInspectSlot(labels["tako.slot"]),
		Image:      container.Config.Image,
		ImageID:    container.Image,
		State:      container.State.Status,
		Running:    container.State.Running,
		ExitCode:   container.State.ExitCode,
		StartedAt:  container.State.StartedAt,
		FinishedAt: container.State.FinishedAt,
		ConfigHash: labels["tako.configHash"],
		RuntimeID:  labels["tako.runtimeId"],
		Ports:      inspectPortsFromDocker(container.NetworkSettings.Ports),
		Mounts:     inspectMountsFromDocker(container.Mounts),
		Networks:   inspectNetworksFromDocker(container.NetworkSettings.Networks),
	}
	if item.Image == "" {
		item.Image = container.Image
	}
	if container.State.Health != nil {
		item.Health = container.State.Health.Status
	}
	return item
}

func parseInspectSlot(value string) int {
	slot, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || slot < 0 {
		return 0
	}
	return slot
}

func shortContainerID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func inspectPortsFromDocker(ports map[string][]dockerInspectPortBinding) []InspectPort {
	if len(ports) == 0 {
		return nil
	}
	keys := make([]string, 0, len(ports))
	for key := range ports {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out []InspectPort
	for _, key := range keys {
		private, protocol := parseDockerPortKey(key)
		bindings := ports[key]
		if len(bindings) == 0 {
			out = append(out, InspectPort{PrivatePort: private, Protocol: protocol})
			continue
		}
		for _, binding := range bindings {
			out = append(out, InspectPort{
				PrivatePort: private,
				Protocol:    protocol,
				HostIP:      binding.HostIP,
				HostPort:    binding.HostPort,
			})
		}
	}
	return out
}

func parseDockerPortKey(key string) (int, string) {
	portText, protocol, ok := strings.Cut(key, "/")
	if !ok {
		protocol = "tcp"
	}
	port, _ := strconv.Atoi(portText)
	return port, protocol
}

func inspectMountsFromDocker(mounts []struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}) []InspectMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]InspectMount, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, InspectMount{
			Type:        mount.Type,
			Name:        mount.Name,
			Source:      mount.Source,
			Destination: mount.Destination,
			RW:          mount.RW,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Destination == out[j].Destination {
			return out[i].Name < out[j].Name
		}
		return out[i].Destination < out[j].Destination
	})
	return out
}

func inspectNetworksFromDocker(networks map[string]struct {
	IPAddress string `json:"IPAddress"`
}) []InspectNetwork {
	if len(networks) == 0 {
		return nil
	}
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]InspectNetwork, 0, len(names))
	for _, name := range names {
		out = append(out, InspectNetwork{Name: name, IPAddress: networks[name].IPAddress})
	}
	return out
}
