package state

import (
	"time"
)

// DeploymentState represents a single deployment's state
type DeploymentState struct {
	ID          string                  `json:"id"`              // Unique deployment ID (timestamp-based)
	Timestamp   time.Time               `json:"timestamp"`       // When deployment occurred
	ProjectName string                  `json:"projectName"`     // Project name
	Version     string                  `json:"version"`         // Project version
	Status      DeploymentStatus        `json:"status"`          // success, failed, rolled_back
	Services    map[string]ServiceState `json:"services"`        // Deployed services
	User        string                  `json:"user"`            // Who deployed
	Host        string                  `json:"host"`            // Which server
	Duration    time.Duration           `json:"duration"`        // How long deployment took
	Message     string                  `json:"message"`         // Deployment message/notes
	Error       string                  `json:"error,omitempty"` // Error if failed
	// Git information
	GitCommit      string `json:"gitCommit,omitempty"`      // Full commit hash
	GitCommitShort string `json:"gitCommitShort,omitempty"` // Short commit hash (7 chars)
	GitBranch      string `json:"gitBranch,omitempty"`      // Branch name
	GitCommitMsg   string `json:"gitCommitMsg,omitempty"`   // Commit message
	GitAuthor      string `json:"gitAuthor,omitempty"`      // Commit author
}

// ServiceState represents a deployed service's state
type ServiceState struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`       // Image name with tag
	ImageID     string            `json:"imageId"`     // Docker image ID
	ContainerID string            `json:"containerId"` // Running container ID
	Port        int               `json:"port"`
	Replicas    int               `json:"replicas"`
	Env         map[string]string `json:"env"`
	HealthCheck HealthCheckState  `json:"healthCheck"`
}

// HealthCheckState represents health check status
type HealthCheckState struct {
	Enabled   bool      `json:"enabled"`
	Path      string    `json:"path"`
	Healthy   bool      `json:"healthy"`
	LastCheck time.Time `json:"lastCheck"`
}

// DeploymentStatus represents deployment outcome
type DeploymentStatus string

const (
	StatusInProgress DeploymentStatus = "in_progress"
	StatusSuccess    DeploymentStatus = "success"
	StatusFailed     DeploymentStatus = "failed"
	StatusRolledBack DeploymentStatus = "rolled_back"
)

// DeploymentHistory contains all deployments for a project
type DeploymentHistory struct {
	ProjectName string             `json:"projectName"`
	Server      string             `json:"server"`
	Deployments []*DeploymentState `json:"deployments"`
	LastUpdated time.Time          `json:"lastUpdated"`
}

// HistoryOptions for filtering deployment history
type HistoryOptions struct {
	Limit         int              // Max number of deployments to return
	Status        DeploymentStatus // Filter by status
	Service       string           // Filter by service name
	Since         time.Time        // Only deployments after this time
	IncludeFailed bool             // Include failed deployments
}
