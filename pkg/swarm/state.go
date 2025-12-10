package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/redentordev/tako-cli/pkg/crypto"
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

// LoadSwarmState loads and decrypts the swarm state from disk
func (m *Manager) LoadSwarmState() (*SwarmState, error) {
	stateFile := m.GetSwarmStateFile()

	// Check if file exists
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		return &SwarmState{
			Nodes: make(map[string]string),
		}, nil
	}

	// Load encryption key and decrypt
	encryptor, err := crypto.NewEncryptorFromKeyFile(crypto.GetProjectKeyPath("."))
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}

	data, err := encryptor.ReadEncryptedFile(stateFile)
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

// SaveSwarmState saves the swarm state to disk with encryption
// Swarm tokens are sensitive and should always be encrypted
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

	// Encrypt the state data (contains sensitive swarm tokens)
	encryptor, err := crypto.NewEncryptorFromKeyFile(crypto.GetProjectKeyPath("."))
	if err != nil {
		return fmt.Errorf("failed to initialize encryption: %w", err)
	}

	encryptedData, err := encryptor.Encrypt(data)
	if err != nil {
		return fmt.Errorf("failed to encrypt swarm state: %w", err)
	}

	// Write encrypted file
	if err := os.WriteFile(stateFile, encryptedData, 0600); err != nil {
		return fmt.Errorf("failed to write swarm state: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Swarm state saved (encrypted) to %s\n", stateFile)
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
