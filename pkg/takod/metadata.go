package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type MetadataRequest struct {
	Node  any `json:"node,omitempty"`
	Peers any `json:"peers,omitempty"`
}

type MetadataResponse struct {
	NodePath  string `json:"nodePath,omitempty"`
	PeersPath string `json:"peersPath,omitempty"`
}

func WriteMetadata(ctx context.Context, dataDir string, req MetadataRequest) (*MetadataResponse, error) {
	if req.Node == nil && req.Peers == nil {
		return nil, fmt.Errorf("node or peers metadata is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dataDir == "" {
		return nil, fmt.Errorf("data directory is required")
	}
	response := &MetadataResponse{}
	if req.Node != nil {
		path := filepath.Join(dataDir, "node.json")
		if err := writeMetadataDocument(path, req.Node); err != nil {
			return nil, fmt.Errorf("failed to write node metadata: %w", err)
		}
		response.NodePath = path
	}
	if req.Peers != nil {
		path := filepath.Join(dataDir, "mesh", "peers.json")
		if err := writeMetadataDocument(path, req.Peers); err != nil {
			return nil, fmt.Errorf("failed to write peer metadata: %w", err)
		}
		response.PeersPath = path
	}
	return response, nil
}

func writeMetadataDocument(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if !metadataDocumentLooksObject(data) {
		return fmt.Errorf("metadata document must be a JSON object")
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0600)
}

func metadataDocumentLooksObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
