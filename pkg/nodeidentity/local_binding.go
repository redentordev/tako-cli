package nodeidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	LocalBindingKind        = "LocalNodeBinding"
	DefaultLocalBindingPath = "/etc/tako/local-node.json"
	maxLocalBindingBytes    = 64 << 10
)

// LocalBinding is the public, integrity-protected subset of installation
// identity needed by an unprivileged local operator. It contains no private
// keys or bearer credentials.
type LocalBinding struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	ClusterID  string `json:"clusterId"`
	NodeID     string `json:"nodeId"`
	NodeName   string `json:"nodeName"`
	WorkerUID  int    `json:"workerUid,omitempty"`
}

func (b LocalBinding) Validate() error {
	if b.APIVersion != InventoryAPIVersion || b.Kind != LocalBindingKind {
		return fmt.Errorf("local node binding apiVersion/kind is invalid")
	}
	if err := (Reference{ClusterID: b.ClusterID, NodeID: b.NodeID}).Validate(); err != nil {
		return err
	}
	if !nodeNamePattern.MatchString(strings.TrimSpace(b.NodeName)) {
		return fmt.Errorf("local node binding has invalid node name")
	}
	if b.WorkerUID < 0 {
		return fmt.Errorf("local node binding worker UID is invalid")
	}
	return nil
}

func WriteLocalBinding(path string, binding LocalBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := os.Lstat(path); err == nil {
		if err := replaceSecureFile(path, data); err != nil {
			return err
		}
	} else if os.IsNotExist(err) {
		if err := createSecureFile(path, data); err != nil {
			return err
		}
	} else {
		return err
	}
	return publishInventoryPermissions(path)
}

func ReadLocalBinding(path string) (*LocalBinding, error) {
	data, err := readPublishedInventoryFile(path, maxLocalBindingBytes)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var binding LocalBinding
	if err := decoder.Decode(&binding); err != nil {
		return nil, fmt.Errorf("decode local node binding: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("decode local node binding: multiple JSON values are not allowed")
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return &binding, nil
}
