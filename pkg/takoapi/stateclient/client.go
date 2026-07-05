// Package stateclient provides a small typed client for takod /v1/state over
// the existing private takodclient transport.
package stateclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// ErrNotFound is returned when takod reports that a state document does not exist.
var ErrNotFound = errors.New("takod state not found")

// Client reads and writes canonical takoapi state documents through takod's
// private Unix-socket control plane.
type Client struct {
	executor takodclient.RequestExecutor
	socket   string
	timeout  time.Duration
}

// New returns a state client using the provided private takod request executor.
func New(executor takodclient.RequestExecutor) *Client {
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
	var document takoapi.DesiredStateDocument
	if err := c.read(takodclient.StateEndpoint(project, environment, takoapi.StateDocumentDesired), project, environment, takoapi.StateDocumentDesired, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteDesired writes the canonical desired state document.
func (c *Client) WriteDesired(document takoapi.DesiredStateDocument) error {
	return c.write(takoapi.StateDocumentDesired, document.Project, document.Environment, "", document.RevisionID, document)
}

// ReadActual reads the canonical aggregate actual state document.
func (c *Client) ReadActual(project, environment string) (*takoapi.ActualStateDocument, error) {
	var document takoapi.ActualStateDocument
	if err := c.read(takodclient.StateEndpoint(project, environment, takoapi.StateDocumentActual), project, environment, takoapi.StateDocumentActual, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteActual writes the canonical aggregate actual state document.
func (c *Client) WriteActual(document takoapi.ActualStateDocument) error {
	return c.write(takoapi.StateDocumentActual, document.Project, document.Environment, "", "", document)
}

// ReadActualNode reads a canonical per-node actual state document.
func (c *Client) ReadActualNode(project, environment, node string) (*takoapi.ActualNodeStateDocument, error) {
	var document takoapi.ActualNodeStateDocument
	endpoint := takodclient.StateNodeEndpoint(project, environment, takoapi.StateDocumentActualNode, node)
	if err := c.read(endpoint, project, environment, takoapi.StateDocumentActualNode, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteActualNode writes a canonical per-node actual state document.
func (c *Client) WriteActualNode(document takoapi.ActualNodeStateDocument) error {
	return c.write(takoapi.StateDocumentActualNode, document.Project, document.Environment, document.Node, "", document)
}

// DeleteActualNode deletes a per-node actual state document.
func (c *Client) DeleteActualNode(project, environment, node string) error {
	request := stateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    takoapi.StateDocumentActualNode,
		Node:        node,
	}
	output, err := c.requestJSON("DELETE", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

// ReadHistory reads the deployment history document.
func (c *Client) ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error) {
	var document takoapi.DeploymentHistoryDocument
	if err := c.read(takodclient.StateEndpoint(project, environment, takoapi.StateDocumentHistory), project, environment, takoapi.StateDocumentHistory, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteHistory writes the deployment history document.
func (c *Client) WriteHistory(document takoapi.DeploymentHistoryDocument) error {
	return c.write(takoapi.StateDocumentHistory, document.ProjectName, document.Environment, "", "", document)
}

// ReadDeployment reads a single deployment history record by deployment ID.
func (c *Client) ReadDeployment(project, environment, deploymentID string) (*takoapi.DeploymentStateDocument, error) {
	var document takoapi.DeploymentStateDocument
	endpoint := takodclient.StateRevisionEndpoint(project, environment, takoapi.StateDocumentDeployment, deploymentID)
	if err := c.read(endpoint, project, environment, takoapi.StateDocumentDeployment, &document); err != nil {
		return nil, err
	}
	return &document, nil
}

// WriteDeployment writes a single deployment history record, using ID as the state revision ID.
func (c *Client) WriteDeployment(document takoapi.DeploymentStateDocument) error {
	return c.write(takoapi.StateDocumentDeployment, document.ProjectName, document.Environment, "", document.ID, document)
}

// AppendEvent appends a canonical state event document.
func (c *Client) AppendEvent(document takoapi.StateEventDocument) error {
	return c.postEvent(document.Project, document.Environment, document)
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

func (c *Client) write(documentName, project, environment, node, revisionID string, document any) error {
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
	output, err := c.requestJSON("PUT", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

func (c *Client) postEvent(project, environment string, document takoapi.StateEventDocument) error {
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
	output, err := c.requestJSON("POST", "/v1/state", request)
	if err != nil {
		return err
	}
	return decodeFound(output)
}

func (c *Client) read(endpoint, project, environment, documentName string, value any) error {
	output, err := c.requestJSON("GET", endpoint, nil)
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

func (c *Client) requestJSON(method, endpoint string, value any) (string, error) {
	if c.timeout > 0 {
		return takodclient.RequestJSONWithTimeout(c.executor, c.socket, method, endpoint, value, c.timeout)
	}
	return takodclient.RequestJSON(c.executor, c.socket, method, endpoint, value)
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

func marshalContent(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
