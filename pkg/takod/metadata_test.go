package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteMetadataWritesNodeAndPeers(t *testing.T) {
	dataDir := t.TempDir()
	response, err := WriteMetadata(context.Background(), dataDir, MetadataRequest{
		Node: map[string]any{
			"runtime": "takod",
			"node":    "node-a",
		},
		Peers: map[string]any{
			"peers": []string{"node-a", "node-b"},
		},
	})
	if err != nil {
		t.Fatalf("WriteMetadata returned error: %v", err)
	}
	if response.NodePath == "" || response.PeersPath == "" {
		t.Fatalf("expected metadata paths in response: %#v", response)
	}

	nodeData, err := os.ReadFile(filepath.Join(dataDir, "node.json"))
	if err != nil {
		t.Fatalf("failed to read node metadata: %v", err)
	}
	var node map[string]any
	if err := json.Unmarshal(nodeData, &node); err != nil {
		t.Fatalf("failed to parse node metadata: %v", err)
	}
	if node["node"] != "node-a" {
		t.Fatalf("node metadata = %#v", node)
	}

	peersData, err := os.ReadFile(filepath.Join(dataDir, "mesh", "peers.json"))
	if err != nil {
		t.Fatalf("failed to read peer metadata: %v", err)
	}
	var peers map[string]any
	if err := json.Unmarshal(peersData, &peers); err != nil {
		t.Fatalf("failed to parse peer metadata: %v", err)
	}
	if _, ok := peers["peers"]; !ok {
		t.Fatalf("peer metadata = %#v", peers)
	}
}

func TestWriteMetadataRejectsMissingDocuments(t *testing.T) {
	if _, err := WriteMetadata(context.Background(), t.TempDir(), MetadataRequest{}); err == nil {
		t.Fatal("expected empty metadata request to fail")
	}
}

func TestWriteMetadataRejectsNonObjectDocument(t *testing.T) {
	if _, err := WriteMetadata(context.Background(), t.TempDir(), MetadataRequest{Node: []string{"not-object"}}); err == nil {
		t.Fatal("expected non-object metadata to fail")
	}
}
