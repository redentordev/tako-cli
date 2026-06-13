package takod

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type EnvBundleRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Content     string `json:"content,omitempty"`
}

type EnvBundleResponse struct {
	Found   bool   `json:"found"`
	Content string `json:"content,omitempty"`
	Path    string `json:"path,omitempty"`
}

func ReadEnvBundle(ctx context.Context, dataDir string, req EnvBundleRequest) (*EnvBundleResponse, error) {
	if err := validateEnvBundleRequest(req, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := envBundlePath(dataDir, req)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &EnvBundleResponse{Found: false, Path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read environment bundle: %w", err)
	}
	return &EnvBundleResponse{
		Found:   true,
		Content: base64.StdEncoding.EncodeToString(data),
		Path:    path,
	}, nil
}

func WriteEnvBundle(ctx context.Context, dataDir string, req EnvBundleRequest) (*EnvBundleResponse, error) {
	if err := validateEnvBundleRequest(req, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.Content))
	if err != nil {
		return nil, fmt.Errorf("invalid environment bundle content: %w", err)
	}
	path, err := envBundlePath(dataDir, req)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("failed to create environment bundle directory: %w", err)
	}
	if err := writeFileAtomic(path, content, 0600); err != nil {
		return nil, fmt.Errorf("failed to write environment bundle: %w", err)
	}
	return &EnvBundleResponse{Found: true, Content: req.Content, Path: path}, nil
}

func validateEnvBundleRequest(req EnvBundleRequest, requireContent bool) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if requireContent && strings.TrimSpace(req.Content) == "" {
		return fmt.Errorf("content is required")
	}
	return nil
}

func envBundlePath(dataDir string, req EnvBundleRequest) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	return filepath.Join(dataDir, "env", req.Project, req.Environment+".enc"), nil
}
