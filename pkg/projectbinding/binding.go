package projectbinding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	APIVersion      = "tako.redentor.dev/v1alpha1"
	Kind            = "ProjectClusterBinding"
	RelativePath    = ".tako/platform-cluster.json"
	maxDocumentSize = 64 << 10
)

// Binding is a create-once acknowledgement that a project workspace belongs
// to one platform cluster. It prevents a checkout on an enrolled worker from
// silently treating that worker as an unrelated control plane.
type Binding struct {
	APIVersion          string     `json:"apiVersion"`
	Kind                string     `json:"kind"`
	Project             string     `json:"project"`
	ClusterID           string     `json:"clusterId"`
	LocalNodeID         string     `json:"localNodeId,omitempty"`
	LocalNodeName       string     `json:"localNodeName,omitempty"`
	ControllerNodeID    string     `json:"controllerNodeId,omitempty"`
	ControllerNodeName  string     `json:"controllerNodeName,omitempty"`
	InventoryGeneration uint64     `json:"inventoryGeneration,omitempty"`
	InventoryUpdatedAt  *time.Time `json:"inventoryUpdatedAt,omitempty"`
	AttachedAt          time.Time  `json:"attachedAt"`
}

// Context is the trusted, node-local platform identity relevant to a project
// workspace. It deliberately contains no application-supplied server fields.
type Context struct {
	ClusterID           string
	LocalNodeID         string
	LocalNodeName       string
	ControllerNodeID    string
	ControllerNodeName  string
	InventoryGeneration uint64
	InventoryUpdatedAt  time.Time
}

func New(project string, platform Context, now time.Time) (*Binding, error) {
	var inventoryUpdatedAt *time.Time
	if !platform.InventoryUpdatedAt.IsZero() {
		updatedAt := platform.InventoryUpdatedAt.UTC()
		inventoryUpdatedAt = &updatedAt
	}
	binding := &Binding{
		APIVersion: APIVersion, Kind: Kind, Project: strings.TrimSpace(project),
		ClusterID: platform.ClusterID, LocalNodeID: platform.LocalNodeID, LocalNodeName: platform.LocalNodeName,
		ControllerNodeID: platform.ControllerNodeID, ControllerNodeName: platform.ControllerNodeName,
		InventoryGeneration: platform.InventoryGeneration, InventoryUpdatedAt: inventoryUpdatedAt, AttachedAt: now.UTC(),
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return binding, nil
}

func (b *Binding) Validate() error {
	if b == nil {
		return fmt.Errorf("project cluster binding is required")
	}
	if b.APIVersion != APIVersion || b.Kind != Kind {
		return fmt.Errorf("unsupported project cluster binding %s %s", b.APIVersion, b.Kind)
	}
	if strings.TrimSpace(b.Project) == "" || b.Project != strings.TrimSpace(b.Project) {
		return fmt.Errorf("project cluster binding has an invalid project name")
	}
	if err := nodeidentity.ValidateClusterID(b.ClusterID); err != nil {
		return fmt.Errorf("project cluster binding cluster identity: %w", err)
	}
	if (b.LocalNodeID == "") != (b.LocalNodeName == "") {
		return fmt.Errorf("project cluster binding local node ID and name must be present together")
	}
	if b.LocalNodeID != "" {
		if err := nodeidentity.ValidateNodeID(b.LocalNodeID); err != nil {
			return fmt.Errorf("project cluster binding local identity: %w", err)
		}
	}
	if (b.ControllerNodeID == "") != (b.ControllerNodeName == "") {
		return fmt.Errorf("project cluster binding controller node ID and name must be present together")
	}
	if b.ControllerNodeID != "" {
		if err := nodeidentity.ValidateNodeID(b.ControllerNodeID); err != nil {
			return fmt.Errorf("project cluster binding controller identity: %w", err)
		}
	}
	if (b.InventoryGeneration == 0) != (b.InventoryUpdatedAt == nil) {
		return fmt.Errorf("project cluster binding inventory generation and timestamp must be present together")
	}
	if b.InventoryUpdatedAt != nil && b.InventoryUpdatedAt.IsZero() {
		return fmt.Errorf("project cluster binding inventory timestamp must not be zero")
	}
	if b.AttachedAt.IsZero() {
		return fmt.Errorf("project cluster binding attachment timestamp is required")
	}
	return nil
}

func (b *Binding) Matches(project string, platform Context) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Project != strings.TrimSpace(project) {
		return fmt.Errorf("workspace is attached for project %q, not %q", b.Project, strings.TrimSpace(project))
	}
	if !strings.EqualFold(b.ClusterID, platform.ClusterID) {
		return fmt.Errorf("workspace is attached to cluster %s, but this node belongs to cluster %s", b.ClusterID, platform.ClusterID)
	}
	return nil
}

func PathForConfig(configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		return "", fmt.Errorf("config path is required")
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Join(filepath.Dir(abs), filepath.FromSlash(RelativePath)), nil
}

func ReadOptional(path string) (*Binding, error) {
	data, err := readSecureBindingFileOptional(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return decodeBinding(data)
}

func decodeBinding(data []byte) (*Binding, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var binding Binding
	if err := decoder.Decode(&binding); err != nil {
		return nil, fmt.Errorf("decode project cluster binding: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode project cluster binding: multiple JSON documents")
		}
		return nil, fmt.Errorf("decode project cluster binding trailing content: %w", err)
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return &binding, nil
}

// Create publishes a binding without replacing an existing document. A
// concurrent creator may win; callers receive the durable winner for
// comparison instead of silently overwriting it.
func Create(path string, binding Binding) (*Binding, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode project cluster binding: %w", err)
	}
	data = append(data, '\n')
	winner, err := createSecureBindingFile(path, data)
	if err != nil {
		return nil, err
	}
	return decodeBinding(winner)
}
