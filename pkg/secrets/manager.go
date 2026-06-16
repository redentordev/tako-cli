package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
	"github.com/redentordev/tako-cli/pkg/envexpand"
	"github.com/redentordev/tako-cli/pkg/fileutil"
)

var (
	secretCommandContext = exec.CommandContext
	secretCommandTimeout = 2 * time.Minute
)

const GitignoreContent = `# Tako secrets - DO NOT COMMIT
secrets*
encryption.key
*.key
*.env
state.json
state/
deployments/
logs/
audit.log
`

type Manager struct {
	mu          sync.RWMutex
	environment string
	basePath    string
	secrets     map[string]string
	redactor    *Redactor
}

// NewManager creates a new secrets manager for the given environment
func NewManager(environment string) (*Manager, error) {
	m := &Manager{
		environment: environment,
		basePath:    ".tako",
		secrets:     make(map[string]string),
	}

	// Create redactor
	m.redactor = NewRedactor()

	// Ensure directory exists with proper permissions
	if err := os.MkdirAll(m.basePath, 0700); err != nil {
		return nil, fmt.Errorf("failed to create secrets directory: %w", err)
	}

	// Create .gitignore if it doesn't exist
	gitignorePath := filepath.Join(m.basePath, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		if err := fileutil.WriteFileAtomic(gitignorePath, []byte(GitignoreContent), 0644); err != nil {
			return nil, fmt.Errorf("failed to create .gitignore: %w", err)
		}
	}

	// Load secrets
	if err := m.loadSecrets(); err != nil {
		return nil, fmt.Errorf("failed to load secrets: %w", err)
	}

	return m, nil
}

// loadSecrets loads secrets from files (common first, then environment-specific)
func (m *Manager) loadSecrets() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear existing secrets
	m.secrets = make(map[string]string)

	// Load common secrets first
	commonPath := filepath.Join(m.basePath, "secrets")
	if err := m.loadFile(commonPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to load common secrets: %w", err)
	}

	// Load environment-specific secrets (these override common)
	if m.environment != "" {
		envPath := filepath.Join(m.basePath, fmt.Sprintf("secrets.%s", m.environment))
		if err := m.loadFile(envPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to load %s secrets: %w", m.environment, err)
		}
	}

	// Register all secret values with the redactor
	for _, value := range m.secrets {
		m.redactor.Register(value)
	}

	return nil
}

// loadFile loads and decrypts secrets from a specific file
func (m *Manager) loadFile(path string) error {
	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // Not an error if file doesn't exist
	}

	envVars, err := readSecretsFile(path)
	if err != nil {
		return err
	}

	// Process each variable
	for key, value := range envVars {
		// Handle command substitution: $(command)
		if strings.HasPrefix(value, "$(") && strings.HasSuffix(value, ")") {
			cmd := value[2 : len(value)-1]
			output, err := m.executeCommand(cmd)
			if err != nil {
				return fmt.Errorf("command substitution failed for %s: %w", key, err)
			}
			value = strings.TrimSpace(output)
		}

		m.secrets[key] = value
	}

	return nil
}

// executeCommand executes a command for substitution (with safety checks)
func (m *Manager) executeCommand(command string) (string, error) {
	if os.Getenv("TAKO_ALLOW_SECRET_COMMANDS") != "1" {
		return "", fmt.Errorf("command substitution is disabled; set TAKO_ALLOW_SECRET_COMMANDS=1 to enable it")
	}

	// Security: Only allow specific safe commands
	allowedCommands := []string{
		"tako",
		"op",      // 1Password CLI
		"bw",      // Bitwarden CLI
		"aws",     // AWS CLI
		"gcloud",  // Google Cloud CLI
		"vault",   // HashiCorp Vault
		"doppler", // Doppler CLI
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	// Check if command is allowed
	cmdName := filepath.Base(parts[0])
	allowed := false
	for _, allowedCmd := range allowedCommands {
		if cmdName == allowedCmd {
			allowed = true
			break
		}
	}

	if !allowed {
		return "", fmt.Errorf("command '%s' not allowed in substitution (security policy)", cmdName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), secretCommandTimeout)
	defer cancel()

	cmd := secretCommandContext(ctx, parts[0], parts[1:]...)
	cmd.Env = os.Environ() // Inherit environment

	output, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("command timed out after %s", secretCommandTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("command failed: %s", string(exitErr.Stderr))
		}
		return "", err
	}

	return string(output), nil
}

// Get retrieves a secret value by key
func (m *Manager) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if value, exists := m.secrets[key]; exists {
		return value, nil
	}

	// Provide helpful error message
	availableKeys := make([]string, 0, len(m.secrets))
	for k := range m.secrets {
		availableKeys = append(availableKeys, k)
	}

	return "", fmt.Errorf("secret '%s' not found. Available secrets: %s",
		key, strings.Join(availableKeys, ", "))
}

func (m *Manager) Has(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.secrets[key]
	return exists
}

// Set sets a secret value
func (m *Manager) Set(key, value string, environment string) error {
	if environment == "" {
		environment = "common"
	}

	// Determine file path
	var path string
	if environment == "common" {
		path = filepath.Join(m.basePath, "secrets")
	} else {
		path = filepath.Join(m.basePath, fmt.Sprintf("secrets.%s", environment))
	}

	// Load existing secrets from the target file. Files may be encrypted from
	// previous writes or plaintext placeholders from `tako secrets init`.
	existing, err := readSecretsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			existing = make(map[string]string)
		} else {
			return fmt.Errorf("failed to read existing secrets: %w", err)
		}
	}

	// Update the value
	existing[key] = value

	// Write back to file
	if err := m.writeSecrets(path, existing); err != nil {
		return fmt.Errorf("failed to save secret: %w", err)
	}

	// Reload secrets
	return m.loadSecrets()
}

// writeSecrets encrypts and writes secrets to a file
func (m *Manager) writeSecrets(path string, secrets map[string]string) error {
	// Build content in memory
	var buf strings.Builder
	buf.WriteString("# Tako secrets file - ENCRYPTED\n")
	buf.WriteString(fmt.Sprintf("# Generated/Updated: %s\n\n", time.Now().Format(time.RFC3339)))

	// Write secrets (sorted for consistency)
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := secrets[key]

		// Escape value if it contains special characters
		if strings.ContainsAny(value, " \t\n\"'$") || strings.Contains(value, "=") {
			value = fmt.Sprintf("\"%s\"", strings.ReplaceAll(value, "\"", "\\\""))
		}

		buf.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}

	// Load encryption key and encrypt
	encryptor, err := crypto.NewEncryptorFromKeyFile(crypto.GetProjectKeyPath("."))
	if err != nil {
		return fmt.Errorf("failed to load encryption key: %w", err)
	}

	return encryptor.WriteEncryptedFile(path, []byte(buf.String()), 0600)
}

// List returns all secret keys (values are not returned for security)
func (m *Manager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.secrets))
	for key := range m.secrets {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	return keys
}

// Delete removes a secret
func (m *Manager) Delete(key string, environment string) error {
	if environment == "" {
		environment = "common"
	}

	// Determine file path
	var path string
	if environment == "common" {
		path = filepath.Join(m.basePath, "secrets")
	} else {
		path = filepath.Join(m.basePath, fmt.Sprintf("secrets.%s", environment))
	}

	// Load existing secrets from the target file. Files may be encrypted from
	// previous writes or plaintext placeholders from `tako secrets init`.
	existing, err := readSecretsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			existing = make(map[string]string)
		} else {
			return fmt.Errorf("failed to read existing secrets: %w", err)
		}
	}

	// Delete the key
	delete(existing, key)

	// Write back
	if err := m.writeSecrets(path, existing); err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}

	// Reload secrets
	return m.loadSecrets()
}

func readSecretsFile(path string) (map[string]string, error) {
	encryptor, err := crypto.NewEncryptorFromKeyFile(crypto.GetProjectKeyPath("."))
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}

	data, err := encryptor.ReadEncryptedFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}

	envVars, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return envVars, nil
}

// IsSensitive checks if a key name indicates sensitive data
func (m *Manager) IsSensitive(key string) bool {
	sensitivePatterns := []string{
		"PASSWORD", "PASSWD", "PWD",
		"SECRET", "TOKEN", "KEY",
		"API", "APIKEY", "API_KEY",
		"AUTH", "AUTHORIZATION",
		"CREDENTIAL", "CRED",
		"PRIVATE", "CERT", "CERTIFICATE",
		"ENCRYPTION", "DECRYPT",
	}

	keyUpper := strings.ToUpper(key)
	for _, pattern := range sensitivePatterns {
		if strings.Contains(keyUpper, pattern) {
			return true
		}
	}

	return false
}

// GetRedactor returns the redactor for output sanitization
func (m *Manager) GetRedactor() *Redactor {
	return m.redactor
}

// CreateEnvFile creates a Docker env file for a service
func (m *Manager) CreateEnvFile(service *config.ServiceConfig) (*EnvFile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	envFile := NewEnvFile()

	serviceEnv := make(map[string]string)
	if service.EnvFile != "" {
		loaded, err := config.LoadEnvFile(service.EnvFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load service envFile %s: %w", service.EnvFile, err)
		}
		serviceEnv = loaded
		for key, value := range serviceEnv {
			envFile.Set(key, value)
		}
	}

	// Load .env file from current directory for variable expansion
	projectEnv := make(map[string]string)
	if envData, err := godotenv.Read(".env"); err == nil {
		projectEnv = envData
	}

	// Also load environment variables from OS
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			projectEnv[parts[0]] = parts[1]
		}
	}

	// First, add clear (non-secret) environment variables with explicit
	// ${VAR} expansion. Bare dollar values are preserved for secrets such as
	// bcrypt hashes.
	for key, value := range service.Env {
		expandedValue, missing := envexpand.Braced(value, func(varName string) (string, bool) {
			if val, ok := serviceEnv[varName]; ok {
				return strings.TrimSpace(val), true
			}
			// Then check project .env and OS environment.
			if val, ok := projectEnv[varName]; ok {
				return strings.TrimSpace(val), true
			}
			// Finally check loaded Tako secrets.
			if val, ok := m.secrets[varName]; ok {
				return val, true
			}
			return "", false
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("service env %s references missing variable(s): %s", key, strings.Join(missing, ", "))
		}

		// Trim any trailing whitespace from the expanded value
		expandedValue = strings.TrimSpace(expandedValue)

		// Only add non-empty values
		if expandedValue != "" {
			envFile.Set(key, expandedValue)
		}
	}

	// Then add secrets (string array format)
	for _, secretRef := range service.Secrets {
		// Support aliasing format: CONTAINER_VAR:SECRET_KEY
		parts := strings.SplitN(secretRef, ":", 2)

		containerVar := parts[0]
		secretKey := containerVar // Default: same as container var

		if len(parts) == 2 {
			secretKey = parts[1]
		}

		// Get the secret value
		value, err := m.Get(secretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve secret '%s': %w", secretKey, err)
		}

		envFile.Set(containerVar, value)
	}

	// Validate the env file
	if err := envFile.Validate(); err != nil {
		return nil, fmt.Errorf("env file validation failed: %w", err)
	}

	return envFile, nil
}

// ValidateRequired checks if all required secrets are present
func (m *Manager) ValidateRequired(required []string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	missing := []string{}

	for _, key := range required {
		// Handle aliasing
		actualKey := key
		if idx := strings.Index(key, ":"); idx > 0 {
			actualKey = key[idx+1:]
		}

		if _, exists := m.secrets[actualKey]; !exists {
			missing = append(missing, actualKey)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required secrets: %s", strings.Join(missing, ", "))
	}

	return nil
}
