package takod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const envBundleEnvelopeVersion = 1

type EnvBundleRequest struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Content     string    `json:"content,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

type EnvBundleResponse struct {
	Found     bool      `json:"found"`
	Content   string    `json:"content,omitempty"`
	Path      string    `json:"path,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
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
	response, err := decodeEnvBundleEnvelope(data)
	if err != nil {
		return nil, err
	}
	response.Path = path
	return response, nil
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
	updatedAt := req.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	encodedContent := base64.StdEncoding.EncodeToString(content)
	envelope, err := json.MarshalIndent(envBundleEnvelope{
		Version:   envBundleEnvelopeVersion,
		UpdatedAt: updatedAt,
		Content:   encodedContent,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to encode environment bundle envelope: %w", err)
	}
	path, err := envBundlePath(dataDir, req)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("failed to create environment bundle directory: %w", err)
	}
	if err := writeFileAtomic(path, envelope, 0600); err != nil {
		return nil, fmt.Errorf("failed to write environment bundle: %w", err)
	}
	return &EnvBundleResponse{Found: true, Content: encodedContent, Path: path, UpdatedAt: updatedAt}, nil
}

type envBundleEnvelope struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Content   string    `json:"content"`
}

func decodeEnvBundleEnvelope(data []byte) (*EnvBundleResponse, error) {
	var envelope envBundleEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("failed to decode environment bundle envelope: %w", err)
	}
	if envelope.Version != envBundleEnvelopeVersion {
		return nil, fmt.Errorf("unsupported environment bundle envelope version %d", envelope.Version)
	}
	if envelope.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("environment bundle envelope missing updatedAt")
	}
	if strings.TrimSpace(envelope.Content) == "" {
		return nil, fmt.Errorf("environment bundle envelope missing content")
	}
	content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envelope.Content))
	if err != nil {
		return nil, fmt.Errorf("invalid environment bundle envelope content: %w", err)
	}
	return &EnvBundleResponse{
		Found:     true,
		Content:   base64.StdEncoding.EncodeToString(content),
		UpdatedAt: envelope.UpdatedAt.UTC(),
	}, nil
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
