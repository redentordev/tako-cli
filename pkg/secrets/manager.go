package secrets

import (
	"bufio"
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
)

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
		gitignoreContent := `# Tako secrets - DO NOT COMMIT
secrets*
*.env
state/
audit.log
`
		if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); err != nil {
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

// loadFile loads secrets from a specific file
func (m *Manager) loadFile(path string) error {
	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // Not an error if file doesn't exist
	}

	// Parse the file
	envVars, err := godotenv.Read(path)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
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

	// Execute the command
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Env = os.Environ() // Inherit environment

	output, err := cmd.Output()
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

	// Load existing secrets from file
	existing := make(map[string]string)
	if fileData, err := godotenv.Read(path); err == nil {
		existing = fileData
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

// writeSecrets writes secrets to a file
func (m *Manager) writeSecrets(path string, secrets map[string]string) error {
	// Create or truncate file with secure permissions
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()

	// Write header
	writer := bufio.NewWriter(file)
	fmt.Fprintf(writer, "# Tako secrets file - DO NOT COMMIT\n")
	fmt.Fprintf(writer, "# Generated/Updated: %s\n\n", time.Now().Format(time.RFC3339))

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

		fmt.Fprintf(writer, "%s=%s\n", key, value)
	}

	return writer.Flush()
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

	// Load existing secrets
	existing := make(map[string]string)
	if fileData, err := godotenv.Read(path); err == nil {
		existing = fileData
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

	// First, add clear (non-secret) environment variables with expansion
	for key, value := range service.Env {
		// Expand ${VAR} and $VAR syntax
		expandedValue := os.Expand(value, func(varName string) string {
			// First check project .env
			if val, ok := projectEnv[varName]; ok {
				return strings.TrimSpace(val)
			}
			// Then check loaded secrets
			if val, ok := m.secrets[varName]; ok {
				return val
			}
			// Return empty string if not found (don't leave ${VAR} in output)
			return ""
		})

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

	// Also handle DockerSecrets (for backward compatibility with Docker Swarm)
	for _, dockerSecret := range service.DockerSecrets {
		// For DockerSecrets, we'll read them similar to before
		// This maintains backward compatibility
		if m.IsSensitive(dockerSecret.Name) {
			value, err := m.Get(dockerSecret.Name)
			if err != nil {
				// Try to get from environment or file as before
				continue
			}
			envFile.Set(dockerSecret.Name, value)
		}
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
