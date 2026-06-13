package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	stateDocumentDesired = "desired"
	stateDocumentActual  = "actual"
	stateDocumentEvent   = "event"
)

type StateDocumentRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Document    string `json:"document"`
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
	if err := os.WriteFile(path, []byte(req.Content), 0600); err != nil {
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
		if err := os.WriteFile(archivePath, []byte(req.Content), 0600); err != nil {
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
	case stateDocumentDesired, stateDocumentActual, stateDocumentEvent:
	default:
		return fmt.Errorf("invalid state document")
	}
	if req.RevisionID != "" && !isSafeStateRevisionID(req.RevisionID) {
		return fmt.Errorf("invalid revision ID")
	}
	if requireContent && strings.TrimSpace(req.Content) == "" {
		return fmt.Errorf("content is required")
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
	case stateDocumentEvent:
		return filepath.Join(dataDir, "events", req.Project, req.Environment+".jsonl"), nil
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
