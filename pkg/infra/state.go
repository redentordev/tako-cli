package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// StateBackend represents the type of state storage backend
type StateBackend string

const (
	BackendLocal   StateBackend = "local"   // Local filesystem
	BackendS3      StateBackend = "s3"      // S3-compatible storage
	BackendManager StateBackend = "manager" // Store on manager node via SSH
)

// S3EndpointMap maps providers to their S3-compatible endpoints
var S3EndpointMap = map[string]string{
	"digitalocean": "https://%s.digitaloceanspaces.com",     // %s = region
	"linode":       "https://%s.linodeobjects.com",          // %s = region (e.g., us-east-1)
	"aws":          "",                                      // AWS S3 uses default endpoint
	"hetzner":      "",                                      // Hetzner doesn't have native object storage, use external S3
}

// StateManager manages Pulumi state and Tako infrastructure state integration
type StateManager struct {
	basePath    string // .tako directory path
	projectName string
	environment string
	stateConfig *config.InfraStateConfig
	infraConfig *config.InfrastructureConfig
}

// NewStateManager creates a new state manager
func NewStateManager(basePath, projectName, environment string) *StateManager {
	return &StateManager{
		basePath:    basePath,
		projectName: projectName,
		environment: environment,
	}
}

// NewStateManagerWithConfig creates a state manager with infrastructure config
func NewStateManagerWithConfig(basePath, projectName, environment string, infraConfig *config.InfrastructureConfig) *StateManager {
	sm := &StateManager{
		basePath:    basePath,
		projectName: projectName,
		environment: environment,
		infraConfig: infraConfig,
	}

	if infraConfig != nil {
		sm.stateConfig = infraConfig.State
	}

	return sm
}

// GetBackend returns the configured state backend type
func (m *StateManager) GetBackend() StateBackend {
	if m.stateConfig == nil || m.stateConfig.Backend == "" {
		return BackendLocal
	}

	switch strings.ToLower(m.stateConfig.Backend) {
	case "s3":
		return BackendS3
	case "manager":
		return BackendManager
	default:
		return BackendLocal
	}
}

// GetPulumiDir returns the path to the Pulumi workspace directory
func (m *StateManager) GetPulumiDir() string {
	return filepath.Join(m.basePath, "pulumi")
}

// GetInfraDir returns the path to the infrastructure state directory
func (m *StateManager) GetInfraDir() string {
	return filepath.Join(m.basePath, "infra")
}

// GetBackendURL returns the backend URL for Pulumi based on configuration
func (m *StateManager) GetBackendURL() string {
	backend := m.GetBackend()

	switch backend {
	case BackendS3:
		return m.getS3BackendURL()
	case BackendManager:
		// For manager backend, we still use local storage but sync to manager
		absPath, _ := filepath.Abs(m.GetPulumiDir())
		return fmt.Sprintf("file://%s", absPath)
	default:
		// Use absolute path for local backend to avoid path confusion
		absPath, _ := filepath.Abs(m.GetPulumiDir())
		return fmt.Sprintf("file://%s", absPath)
	}
}

// getS3BackendURL constructs the S3 backend URL
func (m *StateManager) getS3BackendURL() string {
	if m.stateConfig == nil || m.stateConfig.Bucket == "" {
		// Fallback to local if bucket not configured
		return fmt.Sprintf("file://%s", m.GetPulumiDir())
	}

	bucket := m.stateConfig.Bucket
	region := m.stateConfig.Region
	if region == "" && m.infraConfig != nil {
		region = m.infraConfig.Region
	}

	// Pulumi S3 backend format: s3://bucket-name
	// Region and endpoint are set via environment variables
	return fmt.Sprintf("s3://%s", bucket)
}

// GetBackendEnvVars returns environment variables needed for the backend
func (m *StateManager) GetBackendEnvVars() map[string]string {
	envVars := map[string]string{}

	backend := m.GetBackend()

	switch backend {
	case BackendS3:
		// Set S3 credentials and endpoint
		if m.stateConfig != nil {
			// Access keys
			if m.stateConfig.AccessKey != "" {
				envVars["AWS_ACCESS_KEY_ID"] = m.stateConfig.AccessKey
			} else if m.infraConfig != nil && m.infraConfig.Credentials.AccessKey != "" {
				envVars["AWS_ACCESS_KEY_ID"] = m.infraConfig.Credentials.AccessKey
			} else if m.infraConfig != nil && m.infraConfig.Credentials.Token != "" {
				// DO Spaces uses token as access key
				envVars["AWS_ACCESS_KEY_ID"] = m.infraConfig.Credentials.Token
			}

			if m.stateConfig.SecretKey != "" {
				envVars["AWS_SECRET_ACCESS_KEY"] = m.stateConfig.SecretKey
			} else if m.infraConfig != nil && m.infraConfig.Credentials.SecretKey != "" {
				envVars["AWS_SECRET_ACCESS_KEY"] = m.infraConfig.Credentials.SecretKey
			}

			// Region
			region := m.stateConfig.Region
			if region == "" && m.infraConfig != nil {
				region = m.infraConfig.Region
			}
			if region != "" {
				envVars["AWS_REGION"] = region
			}

			// Custom endpoint for non-AWS S3-compatible services
			endpoint := m.stateConfig.Endpoint
			if endpoint == "" && m.infraConfig != nil {
				// Auto-detect endpoint based on provider
				if endpointTemplate, ok := S3EndpointMap[m.infraConfig.Provider]; ok && endpointTemplate != "" {
					endpoint = fmt.Sprintf(endpointTemplate, region)
				}
			}
			if endpoint != "" {
				envVars["AWS_ENDPOINT_URL"] = endpoint
			}

			// Encryption passphrase
			if m.stateConfig.Encrypt {
				// Use a derived passphrase if not set
				passphrase := os.Getenv("PULUMI_CONFIG_PASSPHRASE")
				if passphrase == "" {
					passphrase = os.Getenv("TAKO_STATE_PASSPHRASE")
				}
				if passphrase != "" {
					envVars["PULUMI_CONFIG_PASSPHRASE"] = passphrase
				}
			} else {
				envVars["PULUMI_CONFIG_PASSPHRASE"] = ""
			}
		}

	case BackendLocal:
		// No encryption for local state by default
		envVars["PULUMI_CONFIG_PASSPHRASE"] = ""
	}

	return envVars
}

// GetStackName returns the Pulumi stack name for this environment
func (m *StateManager) GetStackName() string {
	return m.environment
}

// ValidateS3Config validates S3 backend configuration
func (m *StateManager) ValidateS3Config() error {
	if m.GetBackend() != BackendS3 {
		return nil // No validation needed for non-S3 backends
	}

	if m.stateConfig == nil {
		return fmt.Errorf("S3 backend requires state configuration")
	}

	if m.stateConfig.Bucket == "" {
		return fmt.Errorf("S3 backend requires state.bucket to be set")
	}

	// Check for access credentials
	hasAccessKey := m.stateConfig.AccessKey != "" ||
		(m.infraConfig != nil && m.infraConfig.Credentials.AccessKey != "") ||
		(m.infraConfig != nil && m.infraConfig.Credentials.Token != "") ||
		os.Getenv("AWS_ACCESS_KEY_ID") != ""

	hasSecretKey := m.stateConfig.SecretKey != "" ||
		(m.infraConfig != nil && m.infraConfig.Credentials.SecretKey != "") ||
		os.Getenv("AWS_SECRET_ACCESS_KEY") != ""

	if !hasAccessKey {
		return fmt.Errorf("S3 backend requires access credentials (state.accessKey, credentials.accessKey, credentials.token, or AWS_ACCESS_KEY_ID env var)")
	}

	if !hasSecretKey {
		return fmt.Errorf("S3 backend requires secret key (state.secretKey, credentials.secretKey, or AWS_SECRET_ACCESS_KEY env var)")
	}

	// Check for encryption passphrase if encryption is enabled
	if m.stateConfig.Encrypt {
		hasPassphrase := os.Getenv("PULUMI_CONFIG_PASSPHRASE") != "" ||
			os.Getenv("TAKO_STATE_PASSPHRASE") != ""
		if !hasPassphrase {
			return fmt.Errorf("S3 backend with encryption requires PULUMI_CONFIG_PASSPHRASE or TAKO_STATE_PASSPHRASE env var")
		}
	}

	return nil
}

// EnsureDirectories creates required directories for Pulumi state
func (m *StateManager) EnsureDirectories() error {
	dirs := []string{
		m.GetPulumiDir(),
		m.GetInfraDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}

// CreateStack creates or selects a Pulumi stack with configured backend
func (m *StateManager) CreateStack(ctx context.Context, program pulumi.RunFunc) (auto.Stack, error) {
	if err := m.EnsureDirectories(); err != nil {
		return auto.Stack{}, err
	}

	// Get environment variables for the backend
	envVars := m.GetBackendEnvVars()

	// Use absolute path for backend URL to avoid path confusion
	backendURL := m.GetBackendURL()

	// Create workspace with inline program using local backend
	stackName := auto.FullyQualifiedStackName("organization", m.projectName, m.environment)

	// For inline programs with local backend, use Project to specify backend
	project := auto.Project(workspace.Project{
		Name:    tokens.PackageName(m.projectName),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
		Backend: &workspace.ProjectBackend{
			URL: backendURL,
		},
	})

	stack, err := auto.UpsertStackInlineSource(ctx, stackName, m.projectName, program,
		project,
		auto.EnvVars(envVars),
	)
	if err != nil {
		return auto.Stack{}, fmt.Errorf("failed to create/select stack: %w", err)
	}

	return stack, nil
}

// Preview runs a preview of infrastructure changes
func (m *StateManager) Preview(ctx context.Context, stack auto.Stack, verbose bool) (*auto.PreviewResult, error) {
	opts := []optpreview.Option{}
	if verbose {
		opts = append(opts, optpreview.ProgressStreams(os.Stdout))
	}

	result, err := stack.Preview(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("preview failed: %w", err)
	}

	return &result, nil
}

// Up applies infrastructure changes
func (m *StateManager) Up(ctx context.Context, stack auto.Stack, verbose bool) (*auto.UpResult, error) {
	opts := []optup.Option{}
	if verbose {
		opts = append(opts, optup.ProgressStreams(os.Stdout))
	}

	result, err := stack.Up(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("up failed: %w", err)
	}

	return &result, nil
}

// Destroy tears down infrastructure
func (m *StateManager) Destroy(ctx context.Context, stack auto.Stack, verbose bool) (*auto.DestroyResult, error) {
	opts := []optdestroy.Option{}
	if verbose {
		opts = append(opts, optdestroy.ProgressStreams(os.Stdout))
	}

	result, err := stack.Destroy(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("destroy failed: %w", err)
	}

	return &result, nil
}

// SaveOutputs persists Pulumi stack outputs for Tako to use
func (m *StateManager) SaveOutputs(outputs auto.OutputMap) error {
	if err := m.EnsureDirectories(); err != nil {
		return err
	}

	// Convert OutputMap to serializable format
	serialized := make(map[string]interface{})
	for k, v := range outputs {
		serialized[k] = v.Value
	}

	data, err := json.MarshalIndent(serialized, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal outputs: %w", err)
	}

	outputsPath := filepath.Join(m.GetInfraDir(), "outputs.json")
	if err := os.WriteFile(outputsPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write outputs: %w", err)
	}

	return nil
}

// LoadOutputs reads cached infrastructure outputs
func (m *StateManager) LoadOutputs() (map[string]interface{}, error) {
	outputsPath := filepath.Join(m.GetInfraDir(), "outputs.json")

	data, err := os.ReadFile(outputsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No outputs yet
		}
		return nil, fmt.Errorf("failed to read outputs: %w", err)
	}

	var outputs map[string]interface{}
	if err := json.Unmarshal(data, &outputs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal outputs: %w", err)
	}

	return outputs, nil
}

// SaveInfraState saves the complete infrastructure state
func (m *StateManager) SaveInfraState(state *InfraState) error {
	if err := m.EnsureDirectories(); err != nil {
		return err
	}

	state.LastProvisioned = time.Now()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	statePath := filepath.Join(m.GetInfraDir(), "state.json")
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	return nil
}

// LoadInfraState loads the infrastructure state
func (m *StateManager) LoadInfraState() (*InfraState, error) {
	statePath := filepath.Join(m.GetInfraDir(), "state.json")

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No state yet
		}
		return nil, fmt.Errorf("failed to read state: %w", err)
	}

	var state InfraState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

// IsProvisioned checks if infrastructure has been provisioned
func (m *StateManager) IsProvisioned() bool {
	state, err := m.LoadInfraState()
	if err != nil || state == nil {
		return false
	}
	return len(state.Servers) > 0
}

// GetServerIP retrieves a server's IP from cached outputs
func (m *StateManager) GetServerIP(serverName string, index int) (string, error) {
	outputs, err := m.LoadOutputs()
	if err != nil {
		return "", err
	}
	if outputs == nil {
		return "", fmt.Errorf("no infrastructure outputs found - run 'tako provision' first")
	}

	// Try indexed name first (for count > 1)
	key := fmt.Sprintf("%s_%d_ip", serverName, index)
	if ip, ok := outputs[key].(string); ok {
		return ip, nil
	}

	// Try simple name (for count = 1)
	key = fmt.Sprintf("%s_ip", serverName)
	if ip, ok := outputs[key].(string); ok {
		return ip, nil
	}

	return "", fmt.Errorf("no IP found for server %s", serverName)
}

// ClearState removes all infrastructure state (used after destroy)
func (m *StateManager) ClearState() error {
	infraDir := m.GetInfraDir()

	// Remove outputs and state files
	files := []string{
		filepath.Join(infraDir, "outputs.json"),
		filepath.Join(infraDir, "state.json"),
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", file, err)
		}
	}

	return nil
}

// GetStateBackendDescription returns a human-readable description of the state backend
func (m *StateManager) GetStateBackendDescription() string {
	backend := m.GetBackend()

	switch backend {
	case BackendS3:
		if m.stateConfig != nil && m.stateConfig.Bucket != "" {
			endpoint := "S3"
			if m.stateConfig.Endpoint != "" {
				endpoint = m.stateConfig.Endpoint
			} else if m.infraConfig != nil {
				switch m.infraConfig.Provider {
				case "digitalocean":
					endpoint = "DigitalOcean Spaces"
				case "linode":
					endpoint = "Linode Object Storage"
				case "aws":
					endpoint = "AWS S3"
				}
			}
			return fmt.Sprintf("%s (%s)", endpoint, m.stateConfig.Bucket)
		}
		return "S3 (not configured)"
	case BackendManager:
		return "Manager node (via SSH)"
	default:
		return fmt.Sprintf("Local (%s)", m.GetPulumiDir())
	}
}

// RequiresRemoteSync returns true if the backend requires syncing state to remote
func (m *StateManager) RequiresRemoteSync() bool {
	return m.GetBackend() == BackendManager
}

// GetS3BucketSuggestion returns a suggested bucket name for the project
func (m *StateManager) GetS3BucketSuggestion() string {
	return fmt.Sprintf("tako-state-%s", strings.ToLower(m.projectName))
}
