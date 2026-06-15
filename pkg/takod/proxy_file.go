package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var proxyDynamicDir = "/etc/tako/proxy/dynamic"

type ProxyFileRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ProxyFileResponse struct {
	Path string `json:"path"`
}

func WriteProxyFile(ctx context.Context, req ProxyFileRequest) (*ProxyFileResponse, error) {
	name, err := validateProxyFileName(req.Name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(proxyDynamicDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create proxy dynamic directory: %w", err)
	}
	path := filepath.Join(proxyDynamicDir, name)
	if err := writeFileAtomic(path, []byte(req.Content), 0644); err != nil {
		return nil, fmt.Errorf("failed to publish proxy config file: %w", err)
	}
	return &ProxyFileResponse{Path: path}, nil
}

func RemoveProxyFile(ctx context.Context, name string) (*ProxyFileResponse, error) {
	name, err := validateProxyFileName(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(proxyDynamicDir, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove proxy config file: %w", err)
	}
	return &ProxyFileResponse{Path: path}, nil
}

func validateProxyFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("proxy file name must not contain path separators")
	}
	if !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
		return "", fmt.Errorf("proxy file name must end with .yml or .yaml")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("proxy file name contains invalid character %q", r)
	}
	return name, nil
}
