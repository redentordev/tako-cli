package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ResourceType represents the type of a tracked resource
type ResourceType string

const (
	ResourceService    ResourceType = "service"
	ResourceNetwork    ResourceType = "network"
	ResourceVolume     ResourceType = "volume"
	ResourceSecret     ResourceType = "secret"
	ResourceConfig     ResourceType = "config"
	ResourceImage      ResourceType = "image"
)

// ResourceStatus represents the current status of a resource
type ResourceStatus string

const (
	StatusPending   ResourceStatus = "pending"
	StatusCreating  ResourceStatus = "creating"
	StatusCreated   ResourceStatus = "created"
	StatusUpdating  ResourceStatus = "updating"
	StatusDeleting  ResourceStatus = "deleting"
	StatusDeleted   ResourceStatus = "deleted"
	StatusFailed    ResourceStatus = "failed"
)

// Resource represents a tracked infrastructure resource (Pulumi-style)
type Resource struct {
	// URN is a unique identifier: urn:tako:{project}:{env}:{type}:{name}
	URN         string                 `json:"urn"`
	Type        ResourceType           `json:"type"`
	Name        string                 `json:"name"`
	Provider    string                 `json:"provider"` // docker, swarm, traefik
	
	// State
	Status      ResourceStatus         `json:"status"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	
	// Inputs are the user-provided configuration
	Inputs      map[string]interface{} `json:"inputs"`
	
	// Outputs are provider-generated values
	Outputs     map[string]interface{} `json:"outputs"`
	
	// Dependencies are URNs of resources this depends on
	Dependencies []string              `json:"dependencies,omitempty"`
	
	// Parent is the URN of the parent resource (for hierarchical resources)
	Parent      string                 `json:"parent,omitempty"`
	
	// Checksum of inputs for change detection
	InputsHash  string                 `json:"inputs_hash"`
}

// ResourceGraph represents the complete state of all resources
type ResourceGraph struct {
	Version     int                    `json:"version"`
	Project     string                 `json:"project"`
	Environment string                 `json:"environment"`
	Resources   map[string]*Resource   `json:"resources"` // URN -> Resource
	Metadata    GraphMetadata          `json:"metadata"`
}

// GraphMetadata contains metadata about the resource graph
type GraphMetadata struct {
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	LastDeployment  string    `json:"last_deployment,omitempty"`
	DeploymentCount int       `json:"deployment_count"`
}

// ResourceManager manages the resource graph
type ResourceManager struct {
	basePath    string
	project     string
	environment string
	graph       *ResourceGraph
}

// NewResourceManager creates a new resource manager
func NewResourceManager(basePath, project, environment string) *ResourceManager {
	return &ResourceManager{
		basePath:    basePath,
		project:     project,
		environment: environment,
	}
}

// stateFilePath returns the path to the state file
func (m *ResourceManager) stateFilePath() string {
	return filepath.Join(m.basePath, "resources.json")
}

// Load loads the resource graph from disk
func (m *ResourceManager) Load() error {
	path := m.stateFilePath()
	
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Initialize empty graph
		m.graph = &ResourceGraph{
			Version:     1,
			Project:     m.project,
			Environment: m.environment,
			Resources:   make(map[string]*Resource),
			Metadata: GraphMetadata{
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	m.graph = &ResourceGraph{}
	if err := json.Unmarshal(data, m.graph); err != nil {
		return fmt.Errorf("failed to parse state: %w", err)
	}

	return nil
}

// Save saves the resource graph to disk
func (m *ResourceManager) Save() error {
	if m.graph == nil {
		return fmt.Errorf("no graph loaded")
	}

	m.graph.Metadata.UpdatedAt = time.Now()

	dir := filepath.Dir(m.stateFilePath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(m.graph, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(m.stateFilePath(), data, 0600); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	return nil
}

// GenerateURN generates a URN for a resource
func (m *ResourceManager) GenerateURN(resourceType ResourceType, name string) string {
	return fmt.Sprintf("urn:tako:%s:%s:%s:%s", m.project, m.environment, resourceType, name)
}

// Register registers a new resource or updates an existing one
func (m *ResourceManager) Register(resourceType ResourceType, name string, inputs map[string]interface{}, dependencies []string) (*Resource, error) {
	if m.graph == nil {
		if err := m.Load(); err != nil {
			return nil, err
		}
	}

	urn := m.GenerateURN(resourceType, name)
	inputsHash := hashInputs(inputs)

	existing, exists := m.graph.Resources[urn]
	if exists {
		// Update existing resource
		existing.Inputs = inputs
		existing.InputsHash = inputsHash
		existing.Dependencies = dependencies
		existing.UpdatedAt = time.Now()
		existing.Status = StatusUpdating
		return existing, nil
	}

	// Create new resource
	resource := &Resource{
		URN:          urn,
		Type:         resourceType,
		Name:         name,
		Provider:     "swarm",
		Status:       StatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Inputs:       inputs,
		Outputs:      make(map[string]interface{}),
		Dependencies: dependencies,
		InputsHash:   inputsHash,
	}

	m.graph.Resources[urn] = resource
	return resource, nil
}

// SetOutputs sets the outputs for a resource after creation
func (m *ResourceManager) SetOutputs(urn string, outputs map[string]interface{}) error {
	resource, exists := m.graph.Resources[urn]
	if !exists {
		return fmt.Errorf("resource not found: %s", urn)
	}

	resource.Outputs = outputs
	resource.UpdatedAt = time.Now()
	return nil
}

// SetStatus sets the status of a resource
func (m *ResourceManager) SetStatus(urn string, status ResourceStatus) error {
	resource, exists := m.graph.Resources[urn]
	if !exists {
		return fmt.Errorf("resource not found: %s", urn)
	}

	resource.Status = status
	resource.UpdatedAt = time.Now()
	return nil
}

// Delete marks a resource as deleted
func (m *ResourceManager) Delete(urn string) error {
	resource, exists := m.graph.Resources[urn]
	if !exists {
		return nil // Already deleted
	}

	resource.Status = StatusDeleted
	resource.UpdatedAt = time.Now()
	return nil
}

// Remove completely removes a resource from the graph
func (m *ResourceManager) Remove(urn string) {
	delete(m.graph.Resources, urn)
}

// Get retrieves a resource by URN
func (m *ResourceManager) Get(urn string) *Resource {
	if m.graph == nil {
		return nil
	}
	return m.graph.Resources[urn]
}

// GetByType returns all resources of a specific type
func (m *ResourceManager) GetByType(resourceType ResourceType) []*Resource {
	var resources []*Resource
	for _, r := range m.graph.Resources {
		if r.Type == resourceType && r.Status != StatusDeleted {
			resources = append(resources, r)
		}
	}
	return resources
}

// GetDependencyOrder returns resources in dependency order (topological sort)
func (m *ResourceManager) GetDependencyOrder() ([]*Resource, error) {
	if m.graph == nil {
		return nil, nil
	}

	// Build adjacency list
	inDegree := make(map[string]int)
	dependents := make(map[string][]string)

	for urn, r := range m.graph.Resources {
		if r.Status == StatusDeleted {
			continue
		}
		if _, exists := inDegree[urn]; !exists {
			inDegree[urn] = 0
		}
		for _, dep := range r.Dependencies {
			inDegree[urn]++
			dependents[dep] = append(dependents[dep], urn)
		}
	}

	// Kahn's algorithm
	var queue []string
	for urn, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, urn)
		}
	}

	var sorted []*Resource
	for len(queue) > 0 {
		urn := queue[0]
		queue = queue[1:]

		if r := m.graph.Resources[urn]; r != nil {
			sorted = append(sorted, r)
		}

		for _, dep := range dependents[urn] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	// Check for cycles
	if len(sorted) != len(inDegree) {
		return nil, fmt.Errorf("circular dependency detected")
	}

	return sorted, nil
}

// HasChanged checks if a resource's inputs have changed
func (m *ResourceManager) HasChanged(urn string, inputs map[string]interface{}) bool {
	resource := m.Get(urn)
	if resource == nil {
		return true // New resource
	}

	newHash := hashInputs(inputs)
	return resource.InputsHash != newHash
}

// Export exports the resource graph to a portable format
func (m *ResourceManager) Export() ([]byte, error) {
	if m.graph == nil {
		return nil, fmt.Errorf("no graph loaded")
	}
	return json.MarshalIndent(m.graph, "", "  ")
}

// Import imports a resource graph from exported data
func (m *ResourceManager) Import(data []byte) error {
	graph := &ResourceGraph{}
	if err := json.Unmarshal(data, graph); err != nil {
		return fmt.Errorf("failed to parse import data: %w", err)
	}

	// Validate project/environment match
	if graph.Project != m.project || graph.Environment != m.environment {
		return fmt.Errorf("import mismatch: expected %s/%s, got %s/%s",
			m.project, m.environment, graph.Project, graph.Environment)
	}

	m.graph = graph
	return m.Save()
}

// ListAll returns all resources sorted by type and name
func (m *ResourceManager) ListAll() []*Resource {
	var resources []*Resource
	for _, r := range m.graph.Resources {
		resources = append(resources, r)
	}

	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Type != resources[j].Type {
			return resources[i].Type < resources[j].Type
		}
		return resources[i].Name < resources[j].Name
	})

	return resources
}

// hashInputs creates a deterministic hash of resource inputs
func hashInputs(inputs map[string]interface{}) string {
	// Sort keys for deterministic output
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build canonical representation
	data, _ := json.Marshal(inputs)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes
}
