package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ActivityType represents the type of activity
type ActivityType string

const (
	ActivityServerProvisioned   ActivityType = "server.provisioned"
	ActivityServerConnected     ActivityType = "server.connected"
	ActivityDeploymentStarted   ActivityType = "deployment.started"
	ActivityDeploymentCompleted ActivityType = "deployment.completed"
	ActivityDeploymentFailed    ActivityType = "deployment.failed"
	ActivityResourceDeleted     ActivityType = "resource.deleted"
	ActivityCommandExecuted     ActivityType = "command.executed"
	ActivitySwarmInitialized    ActivityType = "swarm.initialized"
	ActivityNodeJoined          ActivityType = "node.joined"
)

// Activity represents a logged activity
type Activity struct {
	ID        string                 `json:"id"`
	Type      ActivityType           `json:"type"`
	Actor     string                 `json:"actor"`
	Resource  string                 `json:"resource"`
	Server    string                 `json:"server"`
	Command   string                 `json:"command,omitempty"`
	Output    string                 `json:"output,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	Duration  time.Duration          `json:"duration,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// Logger defines the interface for activity logging
type Logger interface {
	Log(activity *Activity) error
	Close() error
}

// FileLogger logs activities to JSON files
type FileLogger struct {
	basePath string
	mu       sync.Mutex
	enabled  bool
}

// NewFileLogger creates a new file-based activity logger
func NewFileLogger(basePath string) (*FileLogger, error) {
	if basePath == "" {
		// Use default path
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		basePath = filepath.Join(home, ".tako", "logs", "activity")
	}

	// Create directory
	if err := os.MkdirAll(basePath, 0750); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &FileLogger{
		basePath: basePath,
		enabled:  true,
	}, nil
}

// Log writes an activity to the log file
func (l *FileLogger) Log(activity *Activity) error {
	if !l.enabled {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Create daily log file path
	fileName := filepath.Join(
		l.basePath,
		activity.Timestamp.Format("2006-01-02")+".jsonl",
	)

	// Open file in append mode
	file, err := os.OpenFile(fileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer file.Close()

	// Encode and write activity
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(activity); err != nil {
		return fmt.Errorf("failed to encode activity: %w", err)
	}

	return nil
}

// Close closes the logger (noop for file logger)
func (l *FileLogger) Close() error {
	l.enabled = false
	return nil
}

// NoOpLogger is a logger that does nothing
type NoOpLogger struct{}

// NewNoOpLogger creates a no-op logger
func NewNoOpLogger() *NoOpLogger {
	return &NoOpLogger{}
}

// Log does nothing
func (n *NoOpLogger) Log(activity *Activity) error {
	return nil
}

// Close does nothing
func (n *NoOpLogger) Close() error {
	return nil
}

// GenerateID generates a simple unique ID for activities
func GenerateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
