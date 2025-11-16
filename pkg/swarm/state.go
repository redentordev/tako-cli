package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SwarmState holds the state of the Docker Swarm
type SwarmState struct {
	Initialized  bool              `json:"initialized"`
	ManagerHost  string            `json:"manager_host"`
	WorkerToken  string            `json:"worker_token"`
	ManagerToken string            `json:"manager_token"`
	Nodes        map[string]string `json:"nodes"` // hostname -> node_id
	RegistryHost string            `json:"registry_host"`
	RegistryPort int               `json:"registry_port"`
	LastUpdated  string            `json:"last_updated"`
}

// GetSwarmStateFile returns the path to the swarm state file
func (m *Manager) GetSwarmStateFile() string {
	// Store in .tako directory in project root
	return filepath.Join(".tako", fmt.Sprintf("swarm_%s_%s.json", m.config.Project.Name, m.environment))
}

// LoadSwarmState loads the swarm state from disk
func (m *Manager) LoadSwarmState() (*SwarmState, error) {
	stateFile := m.GetSwarmStateFile()

	// Check if file exists
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		// Return empty state if file doesn't exist
		return &SwarmState{
			Nodes: make(map[string]string),
		}, nil
	}

	// Read file
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read swarm state: %w", err)
	}

	// Parse JSON
	var state SwarmState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse swarm state: %w", err)
	}

	if state.Nodes == nil {
		state.Nodes = make(map[string]string)
	}

	return &state, nil
}

// SaveSwarmState saves the swarm state to disk
func (m *Manager) SaveSwarmState(state *SwarmState) error {
	stateFile := m.GetSwarmStateFile()

	// Create .tako directory if it doesn't exist
	dir := filepath.Dir(stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal swarm state: %w", err)
	}

	// Write file
	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write swarm state: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Swarm state saved to %s\n", stateFile)
	}

	return nil
}

// ClearSwarmState removes the swarm state file
func (m *Manager) ClearSwarmState() error {
	stateFile := m.GetSwarmStateFile()

	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove swarm state: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Swarm state cleared\n")
	}

	return nil
}
