// Package stateclient provides a small typed client for takod /v1/state over
// the existing private takodclient transport.
package stateclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"

	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// ErrNotFound is returned when takod reports that a state document does not exist.
var ErrNotFound = errors.New("takod state not found")

// Client reads and writes canonical takoapi state documents through takod's
// private Unix-socket control plane.
type Client struct {
	executor any
	socket   string
	timeout  time.Duration
}

// New returns a state client using the provided private takod request executor.
func New(executor any) *Client {
	return &Client{executor: executor}
}

// WithSocket returns a shallow copy configured to use socket. An empty socket
// uses takodclient.DefaultSocket.
func (c *Client) WithSocket(socket string) *Client {
	copy := *c
	copy.socket = socket
	return &copy
}

// WithTimeout returns a shallow copy configured to use timeout for JSON
// requests. Non-positive values use takodclient's default timeout.
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	copy := *c
	copy.timeout = timeout
	return &copy
}

// ReadDesired reads the canonical desired state document.
func (c *Client) ReadDesired(project, environment string) (*takoapi.DesiredStateDocument, error) {
	return c.ReadDesiredContext(context.Background(), project, environment)
}

// ReadDesiredContext reads the canonical desired state document bounded by ctx.
func (c *Client) ReadDesiredContext(ctx context.Context, project, environment string) (*takoapi.DesiredStateDocument, error) {
	var document takoapi.DesiredStateDocument
	if err := c.read(ctx, takodclient.StateEndpoint(project, environment, takoapi.StateDocumentDesired), project, environment, takoapi.StateDocumentDesired, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteDesired writes the canonical desired state document.
func (c *Client) WriteDesired(document takoapi.DesiredStateDocument) error {
	return c.WriteDesiredContext(context.Background(), document)
}

// WriteDesiredContext writes the canonical desired state document bounded by ctx.
func (c *Client) WriteDesiredContext(ctx context.Context, document takoapi.DesiredStateDocument) error {
	return c.write(ctx, takoapi.StateDocumentDesired, document.Project, document.Environment, "", document.RevisionID, document)
}

// ReadActual reads the canonical aggregate actual state document.
func (c *Client) ReadActual(project, environment string) (*takoapi.ActualStateDocument, error) {
	return c.ReadActualContext(context.Background(), project, environment)
}

// ReadActualContext reads the canonical aggregate actual state document bounded by ctx.
func (c *Client) ReadActualContext(ctx context.Context, project, environment string) (*takoapi.ActualStateDocument, error) {
	var document takoapi.ActualStateDocument
	if err := c.read(ctx, takodclient.StateEndpoint(project, environment, takoapi.StateDocumentActual), project, environment, takoapi.StateDocumentActual, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteActual writes the canonical aggregate actual state document.
func (c *Client) WriteActual(document takoapi.ActualStateDocument) error {
	return c.WriteActualContext(context.Background(), document)
}

// WriteActualContext writes the canonical aggregate actual state document bounded by ctx.
func (c *Client) WriteActualContext(ctx context.Context, document takoapi.ActualStateDocument) error {
	return c.write(ctx, takoapi.StateDocumentActual, document.Project, document.Environment, "", "", document)
}

// ReadActualNode reads a canonical per-node actual state document.
func (c *Client) ReadActualNode(project, environment, node string) (*takoapi.ActualNodeStateDocument, error) {
	return c.ReadActualNodeContext(context.Background(), project, environment, node)
}

// ReadActualNodeContext reads a canonical per-node actual state document bounded by ctx.
func (c *Client) ReadActualNodeContext(ctx context.Context, project, environment, node string) (*takoapi.ActualNodeStateDocument, error) {
	var document takoapi.ActualNodeStateDocument
	endpoint := takodclient.StateNodeEndpoint(project, environment, takoapi.StateDocumentActualNode, node)
	if err := c.read(ctx, endpoint, project, environment, takoapi.StateDocumentActualNode, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteActualNode writes a canonical per-node actual state document.
func (c *Client) WriteActualNode(document takoapi.ActualNodeStateDocument) error {
	return c.WriteActualNodeContext(context.Background(), document)
}

// WriteActualNodeContext writes a canonical per-node actual state document bounded by ctx.
func (c *Client) WriteActualNodeContext(ctx context.Context, document takoapi.ActualNodeStateDocument) error {
	return c.write(ctx, takoapi.StateDocumentActualNode, document.Project, document.Environment, document.Node, "", document)
}

// DeleteActualNode deletes a per-node actual state document.
func (c *Client) DeleteActualNode(project, environment, node string) error {
	return c.DeleteActualNodeContext(context.Background(), project, environment, node)
}

// DeleteActualNodeContext deletes a per-node actual state document bounded by ctx.
func (c *Client) DeleteActualNodeContext(ctx context.Context, project, environment, node string) error {
	request := stateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    takoapi.StateDocumentActualNode,
		Node:        node,
	}
	output, err := c.requestJSONContext(ctx, "DELETE", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

// ReadHistory reads the deployment history document.
func (c *Client) ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error) {
	return c.ReadHistoryContext(context.Background(), project, environment)
}

// ReadHistoryContext reads the deployment history document bounded by ctx.
func (c *Client) ReadHistoryContext(ctx context.Context, project, environment string) (*takoapi.DeploymentHistoryDocument, error) {
	var document takoapi.DeploymentHistoryDocument
	if err := c.read(ctx, takodclient.StateEndpoint(project, environment, takoapi.StateDocumentHistory), project, environment, takoapi.StateDocumentHistory, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteHistory writes the deployment history document.
func (c *Client) WriteHistory(document takoapi.DeploymentHistoryDocument) error {
	return c.WriteHistoryContext(context.Background(), document)
}

// WriteHistoryContext writes the deployment history document bounded by ctx.
func (c *Client) WriteHistoryContext(ctx context.Context, document takoapi.DeploymentHistoryDocument) error {
	return c.write(ctx, takoapi.StateDocumentHistory, document.ProjectName, document.Environment, "", "", document)
}

// ReadDeployment reads a single deployment history record by deployment ID.
func (c *Client) ReadDeployment(project, environment, deploymentID string) (*takoapi.DeploymentStateDocument, error) {
	return c.ReadDeploymentContext(context.Background(), project, environment, deploymentID)
}

// ReadDeploymentContext reads a single deployment history record by deployment ID bounded by ctx.
func (c *Client) ReadDeploymentContext(ctx context.Context, project, environment, deploymentID string) (*takoapi.DeploymentStateDocument, error) {
	var document takoapi.DeploymentStateDocument
	endpoint := takodclient.StateRevisionEndpoint(project, environment, takoapi.StateDocumentDeployment, deploymentID)
	if err := c.read(ctx, endpoint, project, environment, takoapi.StateDocumentDeployment, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteDeployment writes a single deployment history record, using ID as the state revision ID.
func (c *Client) WriteDeployment(document takoapi.DeploymentStateDocument) error {
	return c.WriteDeploymentContext(context.Background(), document)
}

// WriteDeploymentContext writes a single deployment history record bounded by ctx, using ID as the state revision ID.
func (c *Client) WriteDeploymentContext(ctx context.Context, document takoapi.DeploymentStateDocument) error {
	return c.write(ctx, takoapi.StateDocumentDeployment, document.ProjectName, document.Environment, "", document.ID, document)
}

// AppendEvent appends a canonical state event document.
func (c *Client) AppendEvent(document takoapi.StateEventDocument) error {
	return c.AppendEventContext(context.Background(), document)
}

// AppendEventContext appends a canonical state event document bounded by ctx.
func (c *Client) AppendEventContext(ctx context.Context, document takoapi.StateEventDocument) error {
	return c.postEvent(ctx, document.Project, document.Environment, document)
}

// LeaseInfo describes a takod operation lease as returned by /v1/lease.
// It intentionally mirrors only public JSON fields and does not depend on
// internal/state types. Both Project and ProjectName are supported because the
// request body uses project while existing lease metadata uses projectName.
type LeaseInfo struct {
	ID          string                       `json:"id"`
	Project     string                       `json:"project,omitempty"`
	ProjectName string                       `json:"projectName,omitempty"`
	Environment string                       `json:"environment"`
	Operation   string                       `json:"operation,omitempty"`
	Who         string                       `json:"who,omitempty"`
	Holder      string                       `json:"holder,omitempty"`
	User        string                       `json:"user,omitempty"`
	Host        string                       `json:"host,omitempty"`
	PID         int                          `json:"pid,omitempty"`
	AcquiredAt  time.Time                    `json:"acquiredAt,omitempty"`
	CreatedAt   time.Time                    `json:"createdAt,omitempty"`
	ExpiresAt   time.Time                    `json:"expiresAt,omitempty"`
	TTLSeconds  int64                        `json:"ttlSeconds,omitempty"`
	Fence       *nodeidentity.OperationFence `json:"fence,omitempty"`
}

// LeaseRequest is the JSON body sent to takod /v1/lease for acquire/release.
type LeaseRequest struct {
	Project       string   `json:"project"`
	Environment   string   `json:"environment"`
	ID            string   `json:"id,omitempty"`
	Operation     string   `json:"operation,omitempty"`
	Who           string   `json:"who,omitempty"`
	PID           int      `json:"pid,omitempty"`
	TTLSeconds    int64    `json:"ttlSeconds,omitempty"`
	Renew         bool     `json:"renew,omitempty"`
	RequestID     string   `json:"requestId,omitempty"`
	TargetNodeIDs []string `json:"targetNodeIds,omitempty"`
}

// LeaseResponse is the public response shape returned by takod /v1/lease.
type LeaseResponse struct {
	Acquired bool       `json:"acquired"`
	Found    bool       `json:"found"`
	Lease    *LeaseInfo `json:"lease,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// ReadLease reads the currently held remote lease for project/environment.
func (c *Client) ReadLease(project, environment string) (*LeaseResponse, error) {
	return c.ReadLeaseContext(context.Background(), project, environment)
}

// ReadLeaseContext reads the currently held remote lease for project/environment bounded by ctx.
func (c *Client) ReadLeaseContext(ctx context.Context, project, environment string) (*LeaseResponse, error) {
	output, err := c.requestJSONContext(ctx, "GET", takodclient.LeaseEndpoint(project, environment), nil)
	if err != nil {
		return nil, err
	}
	return decodeLeaseResponse(output)
}

// AcquireLease acquires the remote operation lease described by request.
func (c *Client) AcquireLease(request LeaseRequest) (*LeaseResponse, error) {
	return c.AcquireLeaseContext(context.Background(), request)
}

// AcquireLeaseContext acquires the remote operation lease described by request bounded by ctx.
func (c *Client) AcquireLeaseContext(ctx context.Context, request LeaseRequest) (*LeaseResponse, error) {
	output, err := c.requestJSONContext(ctx, "POST", "/v1/lease", request)
	if err != nil {
		return nil, err
	}
	return decodeLeaseResponse(output)
}

// ReleaseLease releases the remote lease described by request.
func (c *Client) ReleaseLease(request LeaseRequest) (*LeaseResponse, error) {
	return c.ReleaseLeaseContext(context.Background(), request)
}

// ReleaseLeaseContext releases the remote lease described by request bounded by ctx.
func (c *Client) ReleaseLeaseContext(ctx context.Context, request LeaseRequest) (*LeaseResponse, error) {
	output, err := c.requestJSONContext(ctx, "DELETE", "/v1/lease", request)
	if err != nil {
		return nil, err
	}
	return decodeLeaseResponse(output)
}

type stateDocumentRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Document    string `json:"document"`
	Node        string `json:"node,omitempty"`
	RevisionID  string `json:"revisionId,omitempty"`
	Content     string `json:"content,omitempty"`
}

type stateDocumentResponse struct {
	Found   *bool  `json:"found"`
	Content string `json:"content,omitempty"`
}

func (c *Client) write(ctx context.Context, documentName, project, environment, node, revisionID string, document any) error {
	content, err := marshalContent(document)
	if err != nil {
		return err
	}
	request := stateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    documentName,
		Node:        node,
		RevisionID:  revisionID,
		Content:     content,
	}
	output, err := c.requestJSONContext(ctx, "PUT", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

func (c *Client) postEvent(ctx context.Context, project, environment string, document takoapi.StateEventDocument) error {
	content, err := marshalContent(document)
	if err != nil {
		return err
	}
	request := stateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    takoapi.StateDocumentEvent,
		Content:     content,
	}
	output, err := c.requestJSONContext(ctx, "POST", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

func (c *Client) read(ctx context.Context, endpoint, project, environment, documentName string, value any) error {
	output, err := c.requestJSONContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	var response stateDocumentResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return fmt.Errorf("failed to parse takod state response: %w", err)
	}
	if response.Found != nil && !*response.Found {
		return ErrNotFound
	}
	if response.Content == "" {
		return fmt.Errorf("empty takod state document %s/%s/%s", project, environment, documentName)
	}
	if err := json.Unmarshal([]byte(response.Content), value); err != nil {
		return fmt.Errorf("failed to parse takod state document %s/%s/%s: %w", project, environment, documentName, err)
	}
	return nil
}

func (c *Client) requestJSONContext(ctx context.Context, method, endpoint string, value any) (string, error) {
	if c.timeout > 0 {
		return takodclient.RequestJSONWithTimeoutContext(ctx, c.executor, c.socket, method, endpoint, value, c.timeout)
	}
	return takodclient.RequestJSONWithContext(ctx, c.executor, c.socket, method, endpoint, value)
}

func decodeFound(output string) error {
	if output == "" {
		return nil
	}
	var response stateDocumentResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return fmt.Errorf("failed to parse takod state response: %w", err)
	}
	if response.Found != nil && !*response.Found {
		return ErrNotFound
	}
	return nil
}

func decodeLeaseResponse(output string) (*LeaseResponse, error) {
	var response LeaseResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod lease response: %w", err)
	}
	normalizeLeaseResponse(&response)
	return &response, nil
}

func normalizeLeaseResponse(response *LeaseResponse) {
	if response == nil || response.Lease == nil {
		return
	}
	lease := response.Lease
	if lease.Project == "" {
		lease.Project = lease.ProjectName
	}
	if lease.ProjectName == "" {
		lease.ProjectName = lease.Project
	}
	if lease.AcquiredAt.IsZero() {
		lease.AcquiredAt = lease.CreatedAt
	}
	if lease.CreatedAt.IsZero() {
		lease.CreatedAt = lease.AcquiredAt
	}
}

func marshalContent(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
