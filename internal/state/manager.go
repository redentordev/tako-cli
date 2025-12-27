package state

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	// StateDir is the directory on the server where state is stored
	StateDir = "/var/lib/tako-cli"
	// MaxHistoryEntries is the maximum number of deployments to keep
	MaxHistoryEntries = 50
)

// StateManager manages deployment state on remote servers
type StateManager struct {
	client      *ssh.Client
	projectName string
	server      string
}

// NewStateManager creates a new state manager
func NewStateManager(client *ssh.Client, projectName, server string) *StateManager {
	return &StateManager{
		client:      client,
		projectName: projectName,
		server:      server,
	}
}

// Initialize ensures the state directory exists on the server
func (s *StateManager) Initialize() error {
	// Create state directory and project subdirectory
	statePath := s.getStatePath()
	cmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod -R 755 %s", statePath, StateDir)
	if _, err := s.client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to initialize state directory: %w", err)
	}
	return nil
}

// SaveDeployment saves a deployment state to the server
func (s *StateManager) SaveDeployment(deployment *DeploymentState) error {
	// Ensure deployment has an ID
	if deployment.ID == "" {
		deployment.ID = s.generateDeploymentID()
	}

	// Set metadata
	deployment.ProjectName = s.projectName
	deployment.Host = s.server

	// Serialize to JSON
	data, err := json.MarshalIndent(deployment, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize deployment state: %w", err)
	}

	// Write to server
	statePath := s.getStatePath()
	tmpFile := fmt.Sprintf("/tmp/tako-deploy-%s.json", deployment.ID)

	// Write to temp file first
	writeCmd := fmt.Sprintf("cat > %s <<'EOF'\n%s\nEOF", tmpFile, string(data))
	if _, err := s.client.Execute(writeCmd); err != nil {
		return fmt.Errorf("failed to write deployment state: %w", err)
	}

	// Move to state directory
	mvCmd := fmt.Sprintf("sudo mv %s %s/%s.json", tmpFile, statePath, deployment.ID)
	if _, err := s.client.Execute(mvCmd); err != nil {
		return fmt.Errorf("failed to move deployment state: %w", err)
	}

	// Update history
	return s.updateHistory(deployment)
}

// GetDeployment retrieves a specific deployment by ID
func (s *StateManager) GetDeployment(deploymentID string) (*DeploymentState, error) {
	statePath := s.getStatePath()
	filePath := fmt.Sprintf("%s/%s.json", statePath, deploymentID)

	// Read file from server
	readCmd := fmt.Sprintf("sudo cat %s 2>/dev/null", filePath)
	output, err := s.client.Execute(readCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return nil, fmt.Errorf("deployment %s not found", deploymentID)
	}

	// Parse JSON
	var deployment DeploymentState
	if err := json.Unmarshal([]byte(output), &deployment); err != nil {
		return nil, fmt.Errorf("failed to parse deployment state: %w", err)
	}

	return &deployment, nil
}

// ListDeployments lists all deployments with optional filtering
func (s *StateManager) ListDeployments(opts *HistoryOptions) ([]*DeploymentState, error) {
	if opts == nil {
		opts = &HistoryOptions{Limit: 10, IncludeFailed: true}
	}

	// Read history file
	history, err := s.loadHistory()
	if err != nil {
		return nil, err
	}

	// Filter and sort
	var result []*DeploymentState
	for _, dep := range history.Deployments {
		// Apply filters
		if opts.Status != "" && dep.Status != opts.Status {
			continue
		}
		if opts.Service != "" {
			if _, exists := dep.Services[opts.Service]; !exists {
				continue
			}
		}
		if !opts.Since.IsZero() && dep.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.IncludeFailed && dep.Status == StatusFailed {
			continue
		}

		result = append(result, dep)
	}

	// Sort by timestamp (newest first)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.After(result[j].Timestamp)
	})

	// Apply limit
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}

	return result, nil
}

// GetLatestSuccessful returns the most recent successful deployment
func (s *StateManager) GetLatestSuccessful() (*DeploymentState, error) {
	deployments, err := s.ListDeployments(&HistoryOptions{
		Status:        StatusSuccess,
		Limit:         1,
		IncludeFailed: false,
	})
	if err != nil {
		return nil, err
	}

	if len(deployments) == 0 {
		return nil, fmt.Errorf("no successful deployments found")
	}

	return deployments[0], nil
}

// GetPreviousDeployment returns the deployment before the current one
func (s *StateManager) GetPreviousDeployment(currentID string) (*DeploymentState, error) {
	deployments, err := s.ListDeployments(&HistoryOptions{
		Status:        StatusSuccess,
		Limit:         MaxHistoryEntries,
		IncludeFailed: false,
	})
	if err != nil {
		return nil, err
	}

	// Find current deployment and return the one before it
	for i, dep := range deployments {
		if dep.ID == currentID && i+1 < len(deployments) {
			return deployments[i+1], nil
		}
	}

	return nil, fmt.Errorf("no previous deployment found")
}

// CleanupOldDeployments removes old deployment records
func (s *StateManager) CleanupOldDeployments() error {
	deployments, err := s.ListDeployments(&HistoryOptions{
		Limit:         MaxHistoryEntries * 2,
		IncludeFailed: true,
	})
	if err != nil {
		return err
	}

	// Keep only MaxHistoryEntries
	if len(deployments) > MaxHistoryEntries {
		toDelete := deployments[MaxHistoryEntries:]
		statePath := s.getStatePath()

		for _, dep := range toDelete {
			deleteCmd := fmt.Sprintf("sudo rm -f %s/%s.json", statePath, dep.ID)
			s.client.Execute(deleteCmd) // Ignore errors
		}
	}

	return nil
}

// Helper functions

func (s *StateManager) getStatePath() string {
	return fmt.Sprintf("%s/%s", StateDir, s.projectName)
}

func (s *StateManager) generateDeploymentID() string {
	// Use nanosecond precision + process ID to avoid collisions
	// Format: timestamp_nanoseconds_pid for uniqueness
	return fmt.Sprintf("%d_%d", time.Now().UnixNano(), os.Getpid())
}

func (s *StateManager) updateHistory(deployment *DeploymentState) error {
	history, _ := s.loadHistory() // Ignore error, create new if needed

	if history == nil {
		history = &DeploymentHistory{
			ProjectName: s.projectName,
			Server:      s.server,
			Deployments: []*DeploymentState{},
		}
	}

	// Add or update deployment in history
	found := false
	for i, d := range history.Deployments {
		if d.ID == deployment.ID {
			history.Deployments[i] = deployment
			found = true
			break
		}
	}

	if !found {
		history.Deployments = append(history.Deployments, deployment)
	}

	history.LastUpdated = time.Now()

	// Save history
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}

	historyPath := fmt.Sprintf("%s/history.json", s.getStatePath())
	// Use unique temp file to avoid collisions from concurrent operations
	tmpFile := fmt.Sprintf("/tmp/tako-history-%d-%d.json", time.Now().UnixNano(), os.Getpid())

	writeCmd := fmt.Sprintf("cat > %s <<'EOF'\n%s\nEOF", tmpFile, string(data))
	if _, err := s.client.Execute(writeCmd); err != nil {
		return err
	}

	// Atomic move to final location
	mvCmd := fmt.Sprintf("sudo mv %s %s", tmpFile, historyPath)
	if _, err = s.client.Execute(mvCmd); err != nil {
		// Clean up temp file on failure
		s.client.Execute(fmt.Sprintf("rm -f %s", tmpFile))
		return err
	}
	return nil
}

func (s *StateManager) loadHistory() (*DeploymentHistory, error) {
	historyPath := fmt.Sprintf("%s/history.json", s.getStatePath())
	readCmd := fmt.Sprintf("sudo cat %s 2>/dev/null", historyPath)

	output, err := s.client.Execute(readCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return nil, fmt.Errorf("no history found")
	}

	var history DeploymentHistory
	if err := json.Unmarshal([]byte(output), &history); err != nil {
		return nil, fmt.Errorf("failed to parse history: %w", err)
	}

	return &history, nil
}

// GetCurrentUser returns the current system user for deployment tracking
func GetCurrentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	if hostname, err := os.Hostname(); err == nil {
		return fmt.Sprintf("user@%s", hostname)
	}
	return "unknown"
}

// FormatDeploymentID formats a deployment ID for display
func FormatDeploymentID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// FormatDuration formats a duration for display
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}
