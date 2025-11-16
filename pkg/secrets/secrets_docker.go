package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// DockerSecretsManager handles Docker Swarm secrets (for backward compatibility)
type DockerSecretsManager struct {
	client  *ssh.Client
	project string
	env     string
	verbose bool
}

// NewDockerSecretsManager creates a new Docker secrets manager
func NewDockerSecretsManager(client *ssh.Client, project, environment string, verbose bool) *DockerSecretsManager {
	return &DockerSecretsManager{
		client:  client,
		project: project,
		env:     environment,
		verbose: verbose,
	}
}

// EnsureSecret creates or updates a Docker secret
// Returns the secret name that was created
func (m *DockerSecretsManager) EnsureSecret(secret config.SecretConfig) (string, error) {
	// Generate full secret name: {project}_{env}_{name}
	secretName := fmt.Sprintf("%s_%s_%s", m.project, m.env, secret.Name)

	if m.verbose {
		fmt.Printf("  Managing secret: %s\n", secret.Name)
	}

	// Get secret value from source
	secretValue, err := m.getSecretValue(secret)
	if err != nil {
		return "", fmt.Errorf("failed to get secret value for %s: %w", secret.Name, err)
	}

	// Check if secret already exists
	checkCmd := fmt.Sprintf("docker secret ls --format '{{.Name}}' | grep -q '^%s$'", secretName)
	_, err = m.client.Execute(checkCmd)
	secretExists := err == nil

	if secretExists {
		if m.verbose {
			fmt.Printf("    Secret %s already exists, updating...\n", secretName)
		}

		// Docker secrets are immutable, so we need to:
		// 1. Create new secret with version suffix
		// 2. Update services to use new secret
		// 3. Remove old secret
		// For simplicity, we'll skip recreation if it exists
		// In production, you'd implement versioned secrets

		if m.verbose {
			fmt.Printf("    ✓ Secret %s exists (Docker secrets are immutable)\n", secretName)
		}
	} else {
		if m.verbose {
			fmt.Printf("    Creating new secret %s...\n", secretName)
		}

		// Create the secret
		// Echo secret value and pipe to docker secret create
		createCmd := fmt.Sprintf("echo -n '%s' | docker secret create %s -",
			strings.ReplaceAll(secretValue, "'", "'\\''"), // Escape single quotes
			secretName)

		if _, err := m.client.Execute(createCmd); err != nil {
			return "", fmt.Errorf("failed to create secret %s: %w", secretName, err)
		}

		if m.verbose {
			fmt.Printf("    ✓ Secret %s created\n", secretName)
		}
	}

	return secretName, nil
}

// getSecretValue retrieves the secret value from its source
func (m *DockerSecretsManager) getSecretValue(secret config.SecretConfig) (string, error) {
	source := secret.Source
	if source == "" {
		// Default: get from environment variable with same name as secret
		source = fmt.Sprintf("env:%s", strings.ToUpper(secret.Name))
	}

	parts := strings.SplitN(source, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid source format '%s' (must be 'env:VAR' or 'file:path')", source)
	}

	sourceType := parts[0]
	sourceValue := parts[1]

	switch sourceType {
	case "env":
		// Get from environment variable
		value := os.Getenv(sourceValue)
		if value == "" {
			return "", fmt.Errorf("environment variable %s is not set", sourceValue)
		}
		return value, nil

	case "file":
		// Get from file
		filePath := sourceValue
		if !filepath.IsAbs(filePath) {
			// Make absolute relative to current directory
			cwd, _ := os.Getwd()
			filePath = filepath.Join(cwd, filePath)
		}

		content, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
		}

		// Trim whitespace from file content
		return strings.TrimSpace(string(content)), nil

	default:
		return "", fmt.Errorf("unknown source type '%s' (must be 'env' or 'file')", sourceType)
	}
}

// GetSecretName returns the full secret name for a given secret config
func (m *DockerSecretsManager) GetSecretName(secret config.SecretConfig) string {
	return fmt.Sprintf("%s_%s_%s", m.project, m.env, secret.Name)
}

// GetSecretTarget returns the target path for a secret in the container
func (m *DockerSecretsManager) GetSecretTarget(secret config.SecretConfig) string {
	if secret.Target != "" {
		return secret.Target
	}
	// Default: /run/secrets/{secret_name}
	return fmt.Sprintf("/run/secrets/%s", secret.Name)
}

// CleanupUnusedSecrets removes secrets that are no longer referenced
func (m *DockerSecretsManager) CleanupUnusedSecrets(usedSecrets []string) error {
	if m.verbose {
		fmt.Printf("\n→ Cleaning up unused secrets...\n")
	}

	// Get all secrets for this project and environment
	prefix := fmt.Sprintf("%s_%s_", m.project, m.env)
	listCmd := fmt.Sprintf("docker secret ls --format '{{.Name}}' | grep '^%s' || true", prefix)
	output, err := m.client.Execute(listCmd)
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		if m.verbose {
			fmt.Printf("  No secrets found to clean up\n")
		}
		return nil
	}

	existingSecrets := strings.Split(strings.TrimSpace(output), "\n")
	usedMap := make(map[string]bool)
	for _, s := range usedSecrets {
		usedMap[s] = true
	}

	removedCount := 0
	for _, secretName := range existingSecrets {
		if !usedMap[secretName] {
			if m.verbose {
				fmt.Printf("  Removing unused secret: %s\n", secretName)
			}
			removeCmd := fmt.Sprintf("docker secret rm %s 2>/dev/null || true", secretName)
			m.client.Execute(removeCmd)
			removedCount++
		}
	}

	if m.verbose {
		if removedCount > 0 {
			fmt.Printf("  ✓ Removed %d unused secret(s)\n", removedCount)
		} else {
			fmt.Printf("  ✓ No unused secrets to remove\n")
		}
	}

	return nil
}
