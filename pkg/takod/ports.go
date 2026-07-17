package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	PortAllocationKindMeshUpstream = "mesh-upstream"
	portAllocationSchemaVersion    = 2
)

var portAllocationMu sync.Mutex

type PortAllocationRequest struct {
	Kind          string `json:"kind"`
	Project       string `json:"project"`
	Environment   string `json:"environment"`
	Service       string `json:"service"`
	Revision      string `json:"revision,omitempty"`
	Slot          int    `json:"slot"`
	HostIP        string `json:"hostIp"`
	ContainerPort int    `json:"containerPort"`
	PreferredPort int    `json:"preferredPort"`
	MinPort       int    `json:"minPort"`
	MaxPort       int    `json:"maxPort"`
}

type PortAllocationResponse struct {
	Kind          string    `json:"kind"`
	Project       string    `json:"project"`
	Environment   string    `json:"environment"`
	Service       string    `json:"service"`
	Revision      string    `json:"revision,omitempty"`
	Slot          int       `json:"slot"`
	HostIP        string    `json:"hostIp"`
	HostPort      int       `json:"hostPort"`
	ContainerPort int       `json:"containerPort"`
	Key           string    `json:"key"`
	ClusterID     string    `json:"clusterId,omitempty"`
	NodeID        string    `json:"nodeId,omitempty"`
	Generation    uint64    `json:"generation,omitempty"`
	IssuedAt      time.Time `json:"issuedAt,omitempty"`
	Signature     string    `json:"signature,omitempty"`
}

func portAllocationEvidence(response PortAllocationResponse) ([]byte, error) {
	response.Signature = ""
	return json.Marshal(response)
}

func SignPortAllocation(response *PortAllocationResponse, installation *nodeidentity.Installation) error {
	if response == nil || installation == nil || response.ClusterID != installation.ClusterID || response.NodeID != installation.NodeID {
		return fmt.Errorf("allocation response does not match the signing node identity")
	}
	if response.Generation == 0 || response.IssuedAt.IsZero() {
		return fmt.Errorf("allocation response is missing its durable generation")
	}
	message, err := portAllocationEvidence(*response)
	if err != nil {
		return err
	}
	response.Signature, err = installation.SignAllocation(message)
	return err
}

func VerifyPortAllocation(response PortAllocationResponse, publicKey string) error {
	if response.Generation == 0 || response.IssuedAt.IsZero() {
		return fmt.Errorf("allocation proof is missing its durable generation")
	}
	message, err := portAllocationEvidence(response)
	if err != nil {
		return err
	}
	return nodeidentity.VerifyAllocationSignature(publicKey, message, response.Signature)
}

type portAllocationRegistry struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	Allocations    map[string]portAllocationEntry `json:"allocations"`
	NextGeneration uint64                         `json:"nextGeneration"`
	UpdatedAt      time.Time                      `json:"updatedAt"`
}

type portAllocationEntry struct {
	Kind          string    `json:"kind"`
	Project       string    `json:"project"`
	Environment   string    `json:"environment"`
	Service       string    `json:"service"`
	Revision      string    `json:"revision,omitempty"`
	Slot          int       `json:"slot"`
	HostIP        string    `json:"hostIp"`
	HostPort      int       `json:"hostPort"`
	ContainerPort int       `json:"containerPort"`
	Generation    uint64    `json:"generation"`
	IssuedAt      time.Time `json:"issuedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerHostPortUse struct {
	Project     string
	Environment string
	Service     string
	Revision    string
}

func AllocatePort(ctx context.Context, dataDir string, req PortAllocationRequest) (*PortAllocationResponse, error) {
	if req.Kind == "" {
		req.Kind = PortAllocationKindMeshUpstream
	}
	if err := validatePortAllocationRequest(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	portAllocationMu.Lock()
	defer portAllocationMu.Unlock()

	path := portAllocationRegistryPath(dataDir)
	registry, err := readPortAllocationRegistry(path)
	if err != nil {
		return nil, err
	}

	key := portAllocationKey(req.Kind, req.Project, req.Environment, req.Service, req.Revision, req.Slot)
	usedPorts, err := usedDockerHostPorts(ctx)
	if err != nil {
		return nil, err
	}
	if existing, ok := registry.Allocations[key]; ok && existing.ContainerPort == req.ContainerPort && existing.HostIP == req.HostIP {
		if !hostPortUsedByOtherService(usedPorts[existing.HostPort], req) {
			return portAllocationResponse(key, existing), nil
		}
	}

	reserved := make(map[int]bool)
	for port, uses := range usedPorts {
		if hostPortUsedByOtherService(uses, req) {
			reserved[port] = true
		}
	}
	for allocationKey, allocation := range registry.Allocations {
		if allocationKey == key {
			continue
		}
		if allocation.HostPort > 0 {
			reserved[allocation.HostPort] = true
		}
	}
	hostPort, err := chooseAllocatedPort(req, reserved)
	if err != nil {
		return nil, err
	}
	registry.NextGeneration++
	issuedAt := time.Now().UTC()
	allocation := portAllocationEntry{
		Kind:          req.Kind,
		Project:       req.Project,
		Environment:   req.Environment,
		Service:       req.Service,
		Revision:      req.Revision,
		Slot:          req.Slot,
		HostIP:        req.HostIP,
		HostPort:      hostPort,
		ContainerPort: req.ContainerPort,
		Generation:    registry.NextGeneration,
		IssuedAt:      issuedAt,
		UpdatedAt:     issuedAt,
	}
	registry.SchemaVersion = portAllocationSchemaVersion
	registry.Allocations[key] = allocation
	registry.UpdatedAt = allocation.UpdatedAt

	if err := writePortAllocationRegistry(path, registry); err != nil {
		return nil, err
	}
	return portAllocationResponse(key, allocation), nil
}

func ReleaseServicePortAllocations(ctx context.Context, dataDir string, project string, environment string, service string) error {
	return releaseServicePortAllocations(ctx, dataDir, project, environment, service, "")
}

func ReleaseServicePortAllocationsExceptRevision(ctx context.Context, dataDir string, project string, environment string, service string, keepRevision string) error {
	return releaseServicePortAllocations(ctx, dataDir, project, environment, service, keepRevision)
}

func releaseServicePortAllocations(ctx context.Context, dataDir string, project string, environment string, service string, keepRevision string) error {
	if !isSafeProjectName(project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(service) {
		return fmt.Errorf("invalid service name")
	}
	if keepRevision != "" && !isSafeRuntimeName(keepRevision) {
		return fmt.Errorf("invalid keep revision")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	portAllocationMu.Lock()
	defer portAllocationMu.Unlock()

	path := portAllocationRegistryPath(dataDir)
	registry, err := readPortAllocationRegistry(path)
	if err != nil {
		return err
	}
	changed := false
	for key, allocation := range registry.Allocations {
		if allocation.Project == project && allocation.Environment == environment && allocation.Service == service {
			if keepRevision != "" && allocation.Revision == keepRevision {
				continue
			}
			delete(registry.Allocations, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	registry.UpdatedAt = time.Now().UTC()
	return writePortAllocationRegistry(path, registry)
}

func ReleaseProjectPortAllocations(ctx context.Context, dataDir string, project string, environment string) error {
	if !isSafeProjectName(project) {
		return fmt.Errorf("invalid project name")
	}
	if environment != "" && !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid environment name")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	portAllocationMu.Lock()
	defer portAllocationMu.Unlock()

	path := portAllocationRegistryPath(dataDir)
	registry, err := readPortAllocationRegistry(path)
	if err != nil {
		return err
	}
	changed := false
	for key, allocation := range registry.Allocations {
		if allocation.Project == project && (environment == "" || allocation.Environment == environment) {
			delete(registry.Allocations, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	registry.UpdatedAt = time.Now().UTC()
	return writePortAllocationRegistry(path, registry)
}

func validatePortAllocationRequest(req PortAllocationRequest) error {
	if req.Kind != PortAllocationKindMeshUpstream {
		return fmt.Errorf("invalid port allocation kind")
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
	if req.Revision != "" && !isSafeRuntimeName(req.Revision) {
		return fmt.Errorf("invalid revision")
	}
	if req.Slot <= 0 || req.Slot > 10000 {
		return fmt.Errorf("slot must be between 1 and 10000")
	}
	if req.HostIP == "" || net.ParseIP(req.HostIP) == nil {
		return fmt.Errorf("invalid host IP")
	}
	for label, port := range map[string]int{
		"containerPort": req.ContainerPort,
		"preferredPort": req.PreferredPort,
		"minPort":       req.MinPort,
		"maxPort":       req.MaxPort,
	} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s must be between 1 and 65535", label)
		}
	}
	if req.MinPort > req.MaxPort {
		return fmt.Errorf("minPort must be <= maxPort")
	}
	if req.PreferredPort < req.MinPort || req.PreferredPort > req.MaxPort {
		return fmt.Errorf("preferredPort must be within minPort and maxPort")
	}
	return nil
}

func chooseAllocatedPort(req PortAllocationRequest, reserved map[int]bool) (int, error) {
	for _, port := range candidatePorts(req.PreferredPort, req.MinPort, req.MaxPort) {
		if !reserved[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free host ports available in range %d-%d", req.MinPort, req.MaxPort)
}

func candidatePorts(preferred int, minPort int, maxPort int) []int {
	ports := make([]int, 0, maxPort-minPort+1)
	for port := preferred; port <= maxPort; port++ {
		ports = append(ports, port)
	}
	for port := minPort; port < preferred; port++ {
		ports = append(ports, port)
	}
	return ports
}

func usedDockerHostPorts(ctx context.Context) (map[int][]dockerHostPortUse, error) {
	used := make(map[int][]dockerHostPortUse)
	output, err := runDocker(ctx, "ps", "-q")
	if err != nil {
		return nil, fmt.Errorf("failed to list containers for port allocation: %w", err)
	}
	ids := strings.Fields(output)
	sort.Strings(ids)
	for _, id := range ids {
		inspect, err := runDocker(ctx, "inspect", id, "--format", "{{json .HostConfig.PortBindings}}")
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s port bindings: %w", id, err)
		}
		labelsRaw, err := runDocker(ctx, "inspect", id, "--format", "{{json .Config.Labels}}")
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s labels: %w", id, err)
		}
		owner := dockerHostPortUseFromLabels(parseDockerLabels(labelsRaw))
		for port := range parseDockerHostPorts(inspect) {
			used[port] = append(used[port], owner)
		}
	}
	return used, nil
}

func dockerHostPortUseFromLabels(labels map[string]string) dockerHostPortUse {
	return dockerHostPortUse{
		Project:     labels["tako.project"],
		Environment: labels["tako.environment"],
		Service:     labels["tako.service"],
		Revision:    labels["tako.revision"],
	}
}

func hostPortUsedByOtherService(uses []dockerHostPortUse, req PortAllocationRequest) bool {
	for _, use := range uses {
		if use.Project == req.Project && use.Environment == req.Environment && use.Service == req.Service && (req.Revision == "" || use.Revision == req.Revision) {
			continue
		}
		return true
	}
	return false
}

func parseDockerHostPorts(raw string) map[int]bool {
	used := make(map[int]bool)
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return used
	}
	var bindings map[string][]dockerPortBinding
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return used
	}
	for _, entries := range bindings {
		for _, entry := range entries {
			port, err := strconv.Atoi(strings.TrimSpace(entry.HostPort))
			if err == nil && port > 0 && port <= 65535 {
				used[port] = true
			}
		}
	}
	return used
}

func parseDockerLabels(raw string) map[string]string {
	labels := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return labels
	}
	_ = json.Unmarshal([]byte(raw), &labels)
	return labels
}

func readPortAllocationRegistry(path string) (portAllocationRegistry, error) {
	registry := portAllocationRegistry{
		SchemaVersion: portAllocationSchemaVersion,
		Allocations:   make(map[string]portAllocationEntry),
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return registry, nil
	}
	if err != nil {
		return registry, fmt.Errorf("failed to read port allocation registry: %w", err)
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return registry, fmt.Errorf("failed to parse port allocation registry: %w", err)
	}
	if registry.Allocations == nil {
		registry.Allocations = make(map[string]portAllocationEntry)
	}
	// Upgrade pre-generation registries deterministically before they can be
	// signed. New generations remain monotonic for the lifetime of the node.
	keys := make([]string, 0, len(registry.Allocations))
	for key := range registry.Allocations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := registry.Allocations[key]
		if entry.Generation == 0 {
			registry.NextGeneration++
			entry.Generation = registry.NextGeneration
			entry.IssuedAt = entry.UpdatedAt
			if entry.IssuedAt.IsZero() {
				entry.IssuedAt = time.Now().UTC()
			}
			registry.Allocations[key] = entry
		} else if entry.Generation > registry.NextGeneration {
			registry.NextGeneration = entry.Generation
		}
	}
	return registry, nil
}

func writePortAllocationRegistry(path string, registry portAllocationRegistry) error {
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create port allocation directory: %w", err)
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write port allocation registry: %w", err)
	}
	return nil
}

func portAllocationRegistryPath(dataDir string) string {
	return filepath.Join(dataDir, "ports", "allocations.json")
}

func portAllocationKey(kind string, project string, environment string, service string, revision string, slot int) string {
	if revision != "" {
		return fmt.Sprintf("%s/%s/%s/%s/%s/%d", kind, project, environment, service, revision, slot)
	}
	return fmt.Sprintf("%s/%s/%s/%s/%d", kind, project, environment, service, slot)
}

func portAllocationResponse(key string, allocation portAllocationEntry) *PortAllocationResponse {
	return &PortAllocationResponse{
		Kind:          allocation.Kind,
		Project:       allocation.Project,
		Environment:   allocation.Environment,
		Service:       allocation.Service,
		Revision:      allocation.Revision,
		Slot:          allocation.Slot,
		HostIP:        allocation.HostIP,
		HostPort:      allocation.HostPort,
		ContainerPort: allocation.ContainerPort,
		Generation:    allocation.Generation,
		IssuedAt:      allocation.IssuedAt,
		Key:           key,
	}
}
