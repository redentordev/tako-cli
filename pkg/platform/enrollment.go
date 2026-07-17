package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

type WorkerEnrollmentIdentity struct {
	ClusterID           string `json:"clusterId"`
	NodeID              string `json:"nodeId"`
	NodeName            string `json:"nodeName"`
	AllocationPublicKey string `json:"allocationPublicKey"`
	MeshPublicKey       string `json:"meshPublicKey"`
}

// PrepareWorkerIdentity is the candidate-side create-once enrollment step.
// It deliberately creates only immutable worker identity. The controller must
// still consume a bound join token and publish membership before the node can
// become ready or schedulable.
func PrepareWorkerIdentity(path, clusterID, nodeID, nodeName string, now time.Time) (*WorkerEnrollmentIdentity, bool, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, false, fmt.Errorf("worker identity path must be absolute")
	}
	existing, err := nodeidentity.ReadOptional(path)
	if err != nil {
		return nil, false, fmt.Errorf("read worker installation identity: %w", err)
	}
	if existing != nil {
		if !existing.Matches(clusterID, nodeID) || existing.NodeName != strings.TrimSpace(nodeName) || !slices.Equal(existing.EnrollmentRoles, []string{nodeidentity.RoleWorker}) {
			return nil, false, fmt.Errorf("candidate already has a different installation identity")
		}
		return workerEnrollmentIdentity(existing), true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, false, fmt.Errorf("create worker identity directory: %w", err)
	}
	installation, err := nodeidentity.New(clusterID, nodeID, nodeName, []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		return nil, false, err
	}
	if err := nodeidentity.Create(path, *installation); err != nil {
		return nil, false, fmt.Errorf("create worker installation identity: %w", err)
	}
	return workerEnrollmentIdentity(installation), false, nil
}

func workerEnrollmentIdentity(installation *nodeidentity.Installation) *WorkerEnrollmentIdentity {
	return &WorkerEnrollmentIdentity{
		ClusterID: installation.ClusterID, NodeID: installation.NodeID, NodeName: installation.NodeName,
		AllocationPublicKey: installation.AllocationPublicKey,
	}
}
