package infra

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// SSHKeyPair represents a generated SSH key pair
type SSHKeyPair struct {
	PrivateKeyPath string `json:"private_key_path"`
	PublicKeyPath  string `json:"public_key_path"`
	PublicKey      string `json:"public_key"` // The actual public key content
	Fingerprint    string `json:"fingerprint"`
}

// SSHKeyState stores the generated key and uploaded key IDs per provider
type SSHKeyState struct {
	KeyPair      SSHKeyPair        `json:"key_pair"`
	ProviderKeys map[string]string `json:"provider_keys"` // provider -> key ID on that provider
}

// SSHKeyManager handles SSH key generation and state management
type SSHKeyManager struct {
	takoDir string
	state   *SSHKeyState
}

// NewSSHKeyManager creates a new SSH key manager
func NewSSHKeyManager(takoDir string) *SSHKeyManager {
	return &SSHKeyManager{
		takoDir: takoDir,
		state:   nil,
	}
}

// GetKeyStatePath returns the path to the SSH key state file
func (m *SSHKeyManager) GetKeyStatePath() string {
	return filepath.Join(m.takoDir, "infra", "ssh_keys.json")
}

// GetKeyDir returns the directory where SSH keys are stored
func (m *SSHKeyManager) GetKeyDir() string {
	return filepath.Join(m.takoDir, "infra", "keys")
}

// LoadState loads the SSH key state from disk
func (m *SSHKeyManager) LoadState() error {
	statePath := m.GetKeyStatePath()
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		m.state = &SSHKeyState{
			ProviderKeys: make(map[string]string),
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read SSH key state: %w", err)
	}

	m.state = &SSHKeyState{}
	if err := json.Unmarshal(data, m.state); err != nil {
		return fmt.Errorf("failed to parse SSH key state: %w", err)
	}

	if m.state.ProviderKeys == nil {
		m.state.ProviderKeys = make(map[string]string)
	}

	return nil
}

// SaveState saves the SSH key state to disk
func (m *SSHKeyManager) SaveState() error {
	if m.state == nil {
		return nil
	}

	statePath := m.GetKeyStatePath()
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal SSH key state: %w", err)
	}

	if err := os.WriteFile(statePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write SSH key state: %w", err)
	}

	return nil
}

// EnsureKeyPair ensures an SSH key pair exists, generating one if needed
func (m *SSHKeyManager) EnsureKeyPair(projectName string) (*SSHKeyPair, error) {
	if err := m.LoadState(); err != nil {
		return nil, err
	}

	// Check if key already exists and is valid
	if m.state.KeyPair.PrivateKeyPath != "" {
		if _, err := os.Stat(m.state.KeyPair.PrivateKeyPath); err == nil {
			// Key exists
			return &m.state.KeyPair, nil
		}
	}

	// Generate new key pair
	keyPair, err := m.generateKeyPair(projectName)
	if err != nil {
		return nil, err
	}

	m.state.KeyPair = *keyPair
	if err := m.SaveState(); err != nil {
		return nil, err
	}

	return keyPair, nil
}

// generateKeyPair generates a new ED25519 SSH key pair
func (m *SSHKeyManager) generateKeyPair(projectName string) (*SSHKeyPair, error) {
	keyDir := m.GetKeyDir()
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	// Generate ED25519 key pair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}

	// Convert to SSH format
	sshPubKey, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert public key: %w", err)
	}

	// Marshal public key
	authorizedKey := ssh.MarshalAuthorizedKey(sshPubKey)
	// Add comment to the key
	publicKeyStr := string(authorizedKey[:len(authorizedKey)-1]) + fmt.Sprintf(" tako-%s\n", projectName)

	// Calculate fingerprint
	fingerprint := ssh.FingerprintSHA256(sshPubKey)

	// Marshal private key to OpenSSH format
	privKeyBytes, err := ssh.MarshalPrivateKey(privKey, fmt.Sprintf("tako-%s", projectName))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	privKeyPEM := pem.EncodeToMemory(privKeyBytes)

	// Write files
	keyName := fmt.Sprintf("tako_%s", projectName)
	privateKeyPath := filepath.Join(keyDir, keyName)
	publicKeyPath := filepath.Join(keyDir, keyName+".pub")

	if err := os.WriteFile(privateKeyPath, privKeyPEM, 0600); err != nil {
		return nil, fmt.Errorf("failed to write private key: %w", err)
	}

	if err := os.WriteFile(publicKeyPath, []byte(publicKeyStr), 0644); err != nil {
		return nil, fmt.Errorf("failed to write public key: %w", err)
	}

	return &SSHKeyPair{
		PrivateKeyPath: privateKeyPath,
		PublicKeyPath:  publicKeyPath,
		PublicKey:      publicKeyStr,
		Fingerprint:    fingerprint,
	}, nil
}

// GetProviderKeyID returns the key ID for a specific provider
func (m *SSHKeyManager) GetProviderKeyID(provider string) string {
	if m.state == nil {
		return ""
	}
	return m.state.ProviderKeys[provider]
}

// SetProviderKeyID sets the key ID for a specific provider
func (m *SSHKeyManager) SetProviderKeyID(provider, keyID string) error {
	if m.state == nil {
		if err := m.LoadState(); err != nil {
			return err
		}
	}
	m.state.ProviderKeys[provider] = keyID
	return m.SaveState()
}

// GetPrivateKeyPath returns the private key path
func (m *SSHKeyManager) GetPrivateKeyPath() string {
	if m.state == nil {
		return ""
	}
	return m.state.KeyPair.PrivateKeyPath
}

// GetPublicKey returns the public key content
func (m *SSHKeyManager) GetPublicKey() string {
	if m.state == nil {
		return ""
	}
	return m.state.KeyPair.PublicKey
}

// HasKeyPair returns true if a key pair exists
func (m *SSHKeyManager) HasKeyPair() bool {
	if m.state == nil {
		if err := m.LoadState(); err != nil {
			return false
		}
	}
	return m.state.KeyPair.PrivateKeyPath != ""
}

// GetKeyPair returns the current key pair, loading state if needed
func (m *SSHKeyManager) GetKeyPair() *SSHKeyPair {
	if m.state == nil {
		if err := m.LoadState(); err != nil {
			return nil
		}
	}
	if m.state.KeyPair.PrivateKeyPath == "" {
		return nil
	}
	return &m.state.KeyPair
}

// CleanupKeys removes generated keys and state
func (m *SSHKeyManager) CleanupKeys() error {
	keyDir := m.GetKeyDir()
	statePath := m.GetKeyStatePath()

	// Remove key directory
	if err := os.RemoveAll(keyDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove key directory: %w", err)
	}

	// Remove state file
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove state file: %w", err)
	}

	m.state = nil
	return nil
}
