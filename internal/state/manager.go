package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/resilience"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const MaxHistoryEntries = 50

var ErrNotFound = errors.New("takod state document not found")

// StateManager manages deployment history through the node-local takod state API.
type StateManager struct {
	client         takodclient.RequestExecutor
	socket         string
	projectName    string
	environment    string
	server         string
	requestTimeout time.Duration
}

// NewStateManager creates a state manager that uses the default takod socket.
func NewStateManager(client *ssh.Client, projectName, environment, server string) *StateManager {
	return NewStateManagerWithSocket(client, projectName, environment, server, takodclient.DefaultSocket)
}

// NewStateManagerWithSocket creates a state manager using a configured takod socket.
func NewStateManagerWithSocket(client *ssh.Client, projectName, environment, server, socket string) *StateManager {
	if socket == "" {
		socket = takodclient.DefaultSocket
	}
	return &StateManager{
		client:      client,
		socket:      socket,
		projectName: projectName,
		environment: environment,
		server:      server,
	}
}

// WithRequestTimeout returns a shallow copy that uses a custom takod request
// deadline. A non-positive timeout keeps the package default.
func (s *StateManager) WithRequestTimeout(timeout time.Duration) *StateManager {
	copy := *s
	copy.requestTimeout = timeout
	return &copy
}

// Initialize is retained for callers that want to eagerly validate state access.
func (s *StateManager) Initialize() error {
	return nil
}

// SaveDeployment saves a deployment state with retry logic.
func (s *StateManager) SaveDeployment(deployment *DeploymentState) error {
	if deployment.ID == "" {
		deployment.ID = s.generateDeploymentID()
	}
	deployment.ProjectName = s.projectName
	deployment.Environment = s.environment
	deployment.Host = s.server

	ctx := context.Background()
	return resilience.RetryWithBackoff(ctx, func() error {
		return s.saveDeploymentOnce(deployment)
	},
		resilience.WithMaxElapsed(30*time.Second),
		resilience.WithMaxRetries(3),
	)
}

func (s *StateManager) saveDeploymentOnce(deployment *DeploymentState) error {
	if err := s.writeDocument("deployment", deployment.ID, deployment); err != nil {
		return fmt.Errorf("failed to write deployment state: %w", err)
	}
	return s.updateHistory(deployment)
}

// GetDeployment retrieves a specific deployment by ID.
func (s *StateManager) GetDeployment(deploymentID string) (*DeploymentState, error) {
	var deployment DeploymentState
	if err := s.readDocument("deployment", deploymentID, &deployment); err == nil {
		return &deployment, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("failed to read deployment %s: %w", deploymentID, err)
	}

	history, err := s.loadHistory()
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("deployment %s not found", deploymentID)
	} else if err != nil {
		return nil, fmt.Errorf("failed to load deployment history for %s: %w", deploymentID, err)
	}
	for _, dep := range history.Deployments {
		if dep != nil && dep.ID == deploymentID {
			return dep, nil
		}
	}
	return nil, fmt.Errorf("deployment %s not found", deploymentID)
}

// ListDeployments lists all deployments with optional filtering.
func (s *StateManager) ListDeployments(opts *HistoryOptions) ([]*DeploymentState, error) {
	if opts == nil {
		opts = &HistoryOptions{Limit: 10, IncludeFailed: true}
	}

	history, err := s.loadHistory()
	if err != nil {
		return nil, err
	}

	var result []*DeploymentState
	for _, dep := range history.Deployments {
		if dep == nil {
			continue
		}
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

	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.After(result[j].Timestamp)
	})

	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}

	return result, nil
}

// GetLatestSuccessful returns the most recent successful deployment.
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

// GetPreviousDeployment returns the deployment before the current one.
func (s *StateManager) GetPreviousDeployment(currentID string) (*DeploymentState, error) {
	deployments, err := s.ListDeployments(&HistoryOptions{
		Status:        StatusSuccess,
		Limit:         MaxHistoryEntries,
		IncludeFailed: false,
	})
	if err != nil {
		return nil, err
	}
	for i, dep := range deployments {
		if dep != nil && dep.ID == currentID && i+1 < len(deployments) {
			return deployments[i+1], nil
		}
	}
	return nil, fmt.Errorf("no previous deployment found")
}

// CleanupOldDeployments prunes old entries from the history document.
func (s *StateManager) CleanupOldDeployments() error {
	history, err := s.loadHistory()
	if err != nil {
		return err
	}
	pruned := pruneAndSortDeployments(history.Deployments, MaxHistoryEntries)
	if len(history.Deployments) <= MaxHistoryEntries && len(pruned) == len(history.Deployments) {
		return nil
	}
	history.Deployments = pruned
	history.LastUpdated = time.Now().UTC()
	return s.SaveHistory(history)
}

func (s *StateManager) generateDeploymentID() string {
	return fmt.Sprintf("%d_%d", time.Now().UnixNano(), os.Getpid())
}

func (s *StateManager) updateHistory(deployment *DeploymentState) error {
	history, err := s.loadHistory()
	if errors.Is(err, ErrNotFound) {
		history = &DeploymentHistory{
			ProjectName: s.projectName,
			Environment: s.environment,
			Server:      s.server,
			Deployments: []*DeploymentState{},
		}
	} else if err != nil {
		return fmt.Errorf("failed to load existing deployment history before update: %w", err)
	}

	found := false
	for i, existing := range history.Deployments {
		if existing != nil && existing.ID == deployment.ID {
			history.Deployments[i] = deployment
			found = true
			break
		}
	}
	if !found {
		history.Deployments = append(history.Deployments, deployment)
	}

	history.Deployments = pruneAndSortDeployments(history.Deployments, MaxHistoryEntries)
	history.ProjectName = s.projectName
	history.Environment = s.environment
	history.Server = s.server
	history.LastUpdated = time.Now().UTC()
	return s.SaveHistory(history)
}

func pruneAndSortDeployments(deployments []*DeploymentState, limit int) []*DeploymentState {
	pruned := make([]*DeploymentState, 0, len(deployments))
	for _, deployment := range deployments {
		if deployment != nil {
			pruned = append(pruned, deployment)
		}
	}
	sort.Slice(pruned, func(i, j int) bool {
		return pruned[i].Timestamp.After(pruned[j].Timestamp)
	})
	if limit > 0 && len(pruned) > limit {
		return pruned[:limit]
	}
	return pruned
}

// LoadHistory returns the deployment history from takod state.
func (s *StateManager) LoadHistory() (*DeploymentHistory, error) {
	return s.loadHistory()
}

func (s *StateManager) loadHistory() (*DeploymentHistory, error) {
	var history DeploymentHistory
	if err := s.readDocument("history", "", &history); err != nil {
		return nil, err
	}
	return &history, nil
}

// SaveHistory writes a full deployment history document through takod.
func (s *StateManager) SaveHistory(history *DeploymentHistory) error {
	if history == nil {
		return fmt.Errorf("history is nil")
	}
	history.ProjectName = s.projectName
	history.Environment = s.environment
	history.Server = s.server
	history.LastUpdated = time.Now().UTC()
	return s.writeDocument("history", "", history)
}

func (s *StateManager) writeDocument(document string, revisionID string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	request := takod.StateDocumentRequest{
		Project:     s.projectName,
		Environment: s.environment,
		Document:    document,
		RevisionID:  revisionID,
		Content:     string(data),
	}
	_, err = s.requestJSON("PUT", "/v1/state", request)
	return err
}

func (s *StateManager) readDocument(document string, revisionID string, value any) error {
	content, err := s.readRawDocument(document, revisionID)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(content), value); err != nil {
		return fmt.Errorf("failed to parse %s state: %w", document, err)
	}
	return nil
}

func (s *StateManager) readRawDocument(document string, revisionID string) (string, error) {
	endpoint := takodclient.StateRevisionEndpoint(s.projectName, s.environment, document, revisionID)
	output, err := s.requestJSON("GET", endpoint, nil)
	if err != nil {
		return "", err
	}
	return decodeStateDocumentContent(output, document)
}

func (s *StateManager) requestJSON(method string, endpoint string, value any) (string, error) {
	if s.requestTimeout > 0 {
		return takodclient.RequestJSONWithTimeout(s.client, s.socket, method, endpoint, value, s.requestTimeout)
	}
	return takodclient.RequestJSON(s.client, s.socket, method, endpoint, value)
}

func (s *StateManager) requestJSONWithTimeout(method string, endpoint string, value any, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return s.requestJSON(method, endpoint, value)
	}
	return takodclient.RequestJSONWithTimeout(s.client, s.socket, method, endpoint, value, timeout)
}

func decodeStateDocumentContent(output string, document string) (string, error) {
	var response takod.StateDocumentResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return "", fmt.Errorf("failed to parse takod state response: %w", err)
	}
	if !response.Found {
		return "", ErrNotFound
	}
	if response.Content == "" {
		return "", fmt.Errorf("empty takod state document %s", document)
	}
	return response.Content, nil
}

// GetCurrentUser returns the current system user for deployment tracking.
func GetCurrentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	if hostname, err := os.Hostname(); err == nil {
		return fmt.Sprintf("user@%s", hostname)
	}
	return "unknown"
}

// FormatDeploymentID formats a deployment ID for display.
func FormatDeploymentID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// FormatDuration formats a duration for display.
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}
