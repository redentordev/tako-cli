package takoapi

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	// StateSchemaVersionCurrent is the current takod /v1/state document schema version.
	StateSchemaVersionCurrent = 1

	// StateDocumentDesired is the takod /v1/state document name for desired state.
	StateDocumentDesired = "desired"
	// StateDocumentActual is the takod /v1/state document name for aggregate actual state.
	StateDocumentActual = "actual"
	// StateDocumentActualNode is the takod /v1/state document name for per-node actual state.
	StateDocumentActualNode = "actual-node"
	// StateDocumentEvent is the takod /v1/state document name for append-only events.
	StateDocumentEvent = "event"
	// StateDocumentHistory is the takod /v1/state document name for deployment history.
	StateDocumentHistory = "history"
	// StateDocumentDeployment is the takod /v1/state document name for a deployment record.
	StateDocumentDeployment = "deployment"

	// KindDesiredStateDocument identifies a canonical desired state document.
	KindDesiredStateDocument = "DesiredState"
	// KindDesiredServiceDocument identifies a service entry in desired state.
	KindDesiredServiceDocument = "DesiredService"
	// KindActualStateDocument identifies a canonical aggregate actual state document.
	KindActualStateDocument = "ActualState"
	// KindActualNodeStateDocument identifies a canonical per-node actual state document.
	KindActualNodeStateDocument = "ActualNodeState"
	// KindActualServiceDocument identifies a service entry in actual state.
	KindActualServiceDocument = "ActualService"
	// KindStateEventDocument identifies a canonical state event document.
	KindStateEventDocument = "StateEvent"
)

// DeploymentStatus represents a deployment outcome.
type DeploymentStatus string

const (
	// StatusInProgress means a deployment is still running.
	StatusInProgress DeploymentStatus = "in_progress"
	// StatusSuccess means a deployment completed successfully.
	StatusSuccess DeploymentStatus = "success"
	// StatusWarmed means a deployment warmed a new revision without switching all traffic.
	StatusWarmed DeploymentStatus = "warmed"
	// StatusFailed means a deployment failed.
	StatusFailed DeploymentStatus = "failed"
	// StatusRolledBack means a deployment was rolled back.
	StatusRolledBack DeploymentStatus = "rolled_back"
)

// DesiredStateDocument is Tako's canonical desired state schema for takod
// /v1/state. Project, Environment, and RevisionID remain top-level so existing
// takod state validation can accept the document without transport coupling.
type DesiredStateDocument struct {
	APIVersion    string                            `json:"apiVersion"`
	Kind          string                            `json:"kind"`
	SchemaVersion int                               `json:"schemaVersion"`
	RevisionID    string                            `json:"revisionId"`
	Project       string                            `json:"project"`
	Environment   string                            `json:"environment"`
	Source        string                            `json:"source"`
	TargetNodes   []string                          `json:"targetNodes"`
	Services      map[string]DesiredServiceDocument `json:"services"`
	Git           *GitMetadata                      `json:"git,omitempty"`
	CreatedAt     time.Time                         `json:"createdAt"`
}

// DesiredServiceDocument is a JSON-friendly desired service schema. Config-like
// nested fields that belong to deployment configuration, not state identity, are
// represented without importing pkg/config.
type DesiredServiceDocument struct {
	APIVersion     string            `json:"apiVersion,omitempty"`
	Kind           string            `json:"kind,omitempty"`
	Name           string            `json:"name"`
	Type           string            `json:"type,omitempty"`
	Image          string            `json:"image,omitempty"`
	Build          string            `json:"build,omitempty"`
	Command        string            `json:"command,omitempty"`
	CommandArgs    []string          `json:"commandArgs,omitempty"`
	Entrypoint     string            `json:"entrypoint,omitempty"`
	EntrypointArgs []string          `json:"entrypointArgs,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Port           int               `json:"port,omitempty"`
	Replicas       int               `json:"replicas"`
	Restart        string            `json:"restart,omitempty"`
	Persistent     bool              `json:"persistent,omitempty"`
	Placement      json.RawMessage   `json:"placement,omitempty"`
	Domains        []string          `json:"domains,omitempty"`
	Volumes        []string          `json:"volumes,omitempty"`
	EnvKeys        []string          `json:"envKeys,omitempty"`
	EnvFile        bool              `json:"envFile,omitempty"`
	SecretRefs     []string          `json:"secretRefs,omitempty"`
	DependsOn      []string          `json:"dependsOn,omitempty"`
	HealthCheck    json.RawMessage   `json:"healthCheck,omitempty"`
	DeployStrategy string            `json:"deployStrategy,omitempty"`
}

// ActualStateDocument is Tako's canonical aggregate runtime state schema for
// takod /v1/state. Project and Environment remain top-level for takod identity
// validation.
type ActualStateDocument struct {
	APIVersion    string                             `json:"apiVersion"`
	Kind          string                             `json:"kind"`
	SchemaVersion int                                `json:"schemaVersion"`
	Project       string                             `json:"project"`
	Environment   string                             `json:"environment"`
	Node          string                             `json:"node,omitempty"`
	TargetNodes   []string                           `json:"targetNodes,omitempty"`
	Services      map[string]ActualServiceDocument   `json:"services"`
	Nodes         map[string]ActualNodeStateDocument `json:"nodes,omitempty"`
	CapturedAt    time.Time                          `json:"capturedAt"`
}

// ActualNodeStateDocument is Tako's canonical per-node runtime state schema for
// takod /v1/state actual-node documents. Node remains top-level for takod
// identity validation.
type ActualNodeStateDocument struct {
	APIVersion    string                           `json:"apiVersion"`
	Kind          string                           `json:"kind"`
	SchemaVersion int                              `json:"schemaVersion"`
	Project       string                           `json:"project"`
	Environment   string                           `json:"environment"`
	Node          string                           `json:"node"`
	Services      map[string]ActualServiceDocument `json:"services"`
	CapturedAt    time.Time                        `json:"capturedAt"`
}

// ActualServiceDocument is a JSON-friendly runtime service state schema.
type ActualServiceDocument struct {
	APIVersion        string            `json:"apiVersion,omitempty"`
	Kind              string            `json:"kind,omitempty"`
	Name              string            `json:"name"`
	Image             string            `json:"image,omitempty"`
	RevisionImages    map[string]string `json:"revisionImages,omitempty"`
	Replicas          int               `json:"replicas"`
	Containers        []string          `json:"containers,omitempty"`
	ConfigHash        string            `json:"configHash,omitempty"`
	RuntimeID         string            `json:"runtimeId,omitempty"`
	Persistent        bool              `json:"persistent,omitempty"`
	CurrentRevision   string            `json:"currentRevision,omitempty"`
	PreviousRevision  string            `json:"previousRevision,omitempty"`
	WarmingRevisions  []string          `json:"warmingRevisions,omitempty"`
	DeployStrategy    string            `json:"deployStrategy,omitempty"`
	ActiveContainers  []string          `json:"activeContainers,omitempty"`
	WarmingContainers []string          `json:"warmingContainers,omitempty"`
}

// StateEventDocument is Tako's canonical append-only event schema for takod
// /v1/state. Project and Environment remain top-level for takod identity
// validation.
type StateEventDocument struct {
	APIVersion    string            `json:"apiVersion"`
	Kind          string            `json:"kind"`
	SchemaVersion int               `json:"schemaVersion"`
	Type          string            `json:"type"`
	Project       string            `json:"project"`
	Environment   string            `json:"environment"`
	RevisionID    string            `json:"revisionId,omitempty"`
	Service       string            `json:"service,omitempty"`
	Message       string            `json:"message,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
	Time          time.Time         `json:"time"`
}

// DeploymentStateDocument represents one historical deployment record. Its
// JSON shape intentionally matches internal/state.DeploymentState so existing
// replicated deployment documents can be decoded into this public schema.
type DeploymentStateDocument struct {
	ID             string                          `json:"id"`
	Timestamp      time.Time                       `json:"timestamp"`
	ProjectName    string                          `json:"projectName"`
	Environment    string                          `json:"environment,omitempty"`
	Version        string                          `json:"version"`
	Status         DeploymentStatus                `json:"status"`
	Services       map[string]ServiceStateDocument `json:"services"`
	User           string                          `json:"user"`
	Host           string                          `json:"host"`
	Duration       time.Duration                   `json:"duration"`
	Message        string                          `json:"message"`
	Error          string                          `json:"error,omitempty"`
	GitCommit      string                          `json:"gitCommit,omitempty"`
	GitCommitShort string                          `json:"gitCommitShort,omitempty"`
	GitBranch      string                          `json:"gitBranch,omitempty"`
	GitCommitMsg   string                          `json:"gitCommitMsg,omitempty"`
	GitAuthor      string                          `json:"gitAuthor,omitempty"`
	CLIVersion     string                          `json:"cliVersion,omitempty"`
	CLICommit      string                          `json:"cliCommit,omitempty"`
}

// ServiceStateDocument represents one service in a deployment history record.
type ServiceStateDocument struct {
	Name        string                   `json:"name"`
	Image       string                   `json:"image"`
	ImageID     string                   `json:"imageId"`
	ContainerID string                   `json:"containerId"`
	Port        int                      `json:"port"`
	Replicas    int                      `json:"replicas"`
	Env         map[string]string        `json:"env"`
	HealthCheck HealthCheckStateDocument `json:"healthCheck"`
}

// HealthCheckStateDocument represents health check status in deployment history.
type HealthCheckStateDocument struct {
	Enabled   bool      `json:"enabled"`
	Path      string    `json:"path"`
	Healthy   bool      `json:"healthy"`
	LastCheck time.Time `json:"lastCheck"`
}

// DeploymentHistoryDocument contains all historical deployments for a project
// environment. Its JSON shape intentionally matches internal/state.DeploymentHistory.
type DeploymentHistoryDocument struct {
	ProjectName string                     `json:"projectName"`
	Environment string                     `json:"environment,omitempty"`
	Server      string                     `json:"server"`
	Deployments []*DeploymentStateDocument `json:"deployments"`
	LastUpdated time.Time                  `json:"lastUpdated"`
}

// NewDesiredStateDocument returns a desired state document initialized for the
// current canonical API and state schema versions.
func NewDesiredStateDocument(project, environment, revisionID string) DesiredStateDocument {
	return DesiredStateDocument{
		APIVersion:    APIVersionCurrent,
		Kind:          KindDesiredStateDocument,
		SchemaVersion: StateSchemaVersionCurrent,
		RevisionID:    strings.TrimSpace(revisionID),
		Project:       strings.TrimSpace(project),
		Environment:   strings.TrimSpace(environment),
		Services:      map[string]DesiredServiceDocument{},
	}
}

// NewActualStateDocument returns an aggregate actual state document initialized
// for the current canonical API and state schema versions.
func NewActualStateDocument(project, environment string) ActualStateDocument {
	return ActualStateDocument{
		APIVersion:    APIVersionCurrent,
		Kind:          KindActualStateDocument,
		SchemaVersion: StateSchemaVersionCurrent,
		Project:       strings.TrimSpace(project),
		Environment:   strings.TrimSpace(environment),
		Services:      map[string]ActualServiceDocument{},
		Nodes:         map[string]ActualNodeStateDocument{},
	}
}

// NewActualNodeStateDocument returns a per-node actual state document
// initialized for the current canonical API and state schema versions.
func NewActualNodeStateDocument(project, environment, node string) ActualNodeStateDocument {
	return ActualNodeStateDocument{
		APIVersion:    APIVersionCurrent,
		Kind:          KindActualNodeStateDocument,
		SchemaVersion: StateSchemaVersionCurrent,
		Project:       strings.TrimSpace(project),
		Environment:   strings.TrimSpace(environment),
		Node:          strings.TrimSpace(node),
		Services:      map[string]ActualServiceDocument{},
	}
}

// NewStateEventDocument returns an event document initialized for the current
// canonical API and state schema versions.
func NewStateEventDocument(project, environment, eventType string) StateEventDocument {
	return StateEventDocument{
		APIVersion:    APIVersionCurrent,
		Kind:          KindStateEventDocument,
		SchemaVersion: StateSchemaVersionCurrent,
		Type:          strings.TrimSpace(eventType),
		Project:       strings.TrimSpace(project),
		Environment:   strings.TrimSpace(environment),
		Details:       map[string]string{},
	}
}
