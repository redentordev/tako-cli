package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	stateDocumentDesired    = "desired"
	stateDocumentActual     = "actual"
	stateDocumentActualNode = "actual-node"
	stateDocumentEvent      = "event"
	stateDocumentHistory    = "history"
	stateDocumentDeployment = "deployment"
)

type StateDocumentRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Document    string `json:"document"`
	Node        string `json:"node,omitempty"`
	RevisionID  string `json:"revisionId,omitempty"`
	Content     string `json:"content,omitempty"`
}

type StateDocumentResponse struct {
	Found   bool   `json:"found"`
	Content string `json:"content,omitempty"`
	Path    string `json:"path,omitempty"`
}

func ReadStateDocument(ctx context.Context, dataDir string, req StateDocumentRequest) (*StateDocumentResponse, error) {
	if err := validateStateDocumentRequest(req, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := stateDocumentPath(dataDir, req, false)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &StateDocumentResponse{Found: false, Path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read state document: %w", err)
	}
	return &StateDocumentResponse{Found: true, Content: string(data), Path: path}, nil
}

func WriteStateDocument(ctx context.Context, dataDir string, req StateDocumentRequest) (*StateDocumentResponse, error) {
	if err := validateStateDocumentRequest(req, true); err != nil {
		return nil, err
	}
	if req.Document == stateDocumentEvent {
		return nil, fmt.Errorf("state events are append-only")
	}
	if err := validateStateDocumentContent(req.Document, req.Content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := stateDocumentPath(dataDir, req, false)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}
	if err := writeFileAtomic(path, []byte(req.Content), 0600); err != nil {
		return nil, fmt.Errorf("failed to write state document: %w", err)
	}
	if req.Document == stateDocumentDesired && req.RevisionID != "" {
		archivePath, err := stateDocumentPath(dataDir, req, true)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create revision archive directory: %w", err)
		}
		if err := writeFileAtomic(archivePath, []byte(req.Content), 0600); err != nil {
			return nil, fmt.Errorf("failed to write desired revision archive: %w", err)
		}
	}
	return &StateDocumentResponse{Found: true, Content: req.Content, Path: path}, nil
}

func AppendStateEvent(ctx context.Context, dataDir string, req StateDocumentRequest) (*StateDocumentResponse, error) {
	req.Document = stateDocumentEvent
	if err := validateStateDocumentRequest(req, true); err != nil {
		return nil, err
	}
	if err := validateStateDocumentContent(req.Document, req.Content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := stateDocumentPath(dataDir, req, false)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create event directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open event log: %w", err)
	}
	defer file.Close()
	content := strings.TrimRight(req.Content, "\n") + "\n"
	if _, err := file.WriteString(content); err != nil {
		return nil, fmt.Errorf("failed to append event: %w", err)
	}
	return &StateDocumentResponse{Found: true, Path: path}, nil
}

func validateStateDocumentRequest(req StateDocumentRequest, requireContent bool) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	switch req.Document {
	case stateDocumentDesired, stateDocumentActual, stateDocumentActualNode, stateDocumentEvent, stateDocumentHistory, stateDocumentDeployment:
	default:
		return fmt.Errorf("invalid state document")
	}
	if req.Document == stateDocumentActualNode && !isSafeRuntimeName(req.Node) {
		return fmt.Errorf("invalid node name")
	}
	if req.Document == stateDocumentDeployment && req.RevisionID == "" {
		return fmt.Errorf("deployment ID is required")
	}
	if req.RevisionID != "" && !isSafeStateRevisionID(req.RevisionID) {
		return fmt.Errorf("invalid revision ID")
	}
	if requireContent && strings.TrimSpace(req.Content) == "" {
		return fmt.Errorf("content is required")
	}
	return nil
}

func validateStateDocumentContent(document string, content string) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("invalid %s state document JSON: %w", document, err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s state document must contain a single JSON value", document)
		}
		return fmt.Errorf("invalid %s state document JSON: %w", document, err)
	}

	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%s state document must be a JSON object", document)
	}
	return nil
}

func stateDocumentPath(dataDir string, req StateDocumentRequest, archive bool) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	switch req.Document {
	case stateDocumentDesired:
		if archive {
			return filepath.Join(dataDir, "desired", req.Project, req.Environment, "revisions", req.RevisionID+".json"), nil
		}
		return filepath.Join(dataDir, "desired", req.Project, req.Environment, "revision.json"), nil
	case stateDocumentActual:
		return filepath.Join(dataDir, "actual", req.Project, req.Environment, "containers.json"), nil
	case stateDocumentActualNode:
		return filepath.Join(dataDir, "actual", req.Project, req.Environment, "nodes", req.Node+".json"), nil
	case stateDocumentEvent:
		return filepath.Join(dataDir, "events", req.Project, req.Environment+".jsonl"), nil
	case stateDocumentHistory:
		return filepath.Join(dataDir, "history", req.Project, req.Environment, "history.json"), nil
	case stateDocumentDeployment:
		return filepath.Join(dataDir, "history", req.Project, req.Environment, "deployments", req.RevisionID+".json"), nil
	default:
		return "", fmt.Errorf("invalid state document")
	}
}

func isSafeStateRevisionID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
