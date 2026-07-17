package nodeidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const OperationFenceKind = "ControllerOperationFence"

// OperationFence is a controller-signed, monotonically fenced mutation grant.
// Workers accept it only after the controller has explicitly activated the
// exact token for that node. The token binds one project/environment operation
// to one membership generation and an explicit target set.
type OperationFence struct {
	Kind                 string    `json:"kind"`
	ClusterID            string    `json:"clusterId"`
	ControllerNodeID     string    `json:"controllerNodeId"`
	MembershipGeneration uint64    `json:"membershipGeneration"`
	Project              string    `json:"project"`
	Environment          string    `json:"environment"`
	OperationID          string    `json:"operationId"`
	Operation            string    `json:"operation"`
	Token                uint64    `json:"token"`
	HolderTokenHash      string    `json:"holderTokenHash"`
	TargetNodeIDs        []string  `json:"targetNodeIds"`
	IssuedAt             time.Time `json:"issuedAt"`
	ExpiresAt            time.Time `json:"expiresAt"`
	Signature            string    `json:"signature"`
}

func (f OperationFence) Validate(now time.Time) error {
	if f.Kind != OperationFenceKind {
		return fmt.Errorf("operation fence kind is invalid")
	}
	if err := ValidateClusterID(f.ClusterID); err != nil {
		return err
	}
	if err := ValidateNodeID(f.ControllerNodeID); err != nil {
		return fmt.Errorf("operation fence controller identity is invalid: %w", err)
	}
	if f.MembershipGeneration == 0 || f.Token == 0 {
		return fmt.Errorf("operation fence generation and token are required")
	}
	if len(f.HolderTokenHash) != sha256.Size*2 {
		return fmt.Errorf("operation fence holder credential hash is invalid")
	}
	if _, err := hex.DecodeString(f.HolderTokenHash); err != nil || strings.ToLower(f.HolderTokenHash) != f.HolderTokenHash {
		return fmt.Errorf("operation fence holder credential hash is invalid")
	}
	if strings.TrimSpace(f.Project) == "" || strings.TrimSpace(f.Environment) == "" || strings.TrimSpace(f.OperationID) == "" || strings.TrimSpace(f.Operation) == "" {
		return fmt.Errorf("operation fence scope is incomplete")
	}
	if f.IssuedAt.IsZero() || f.ExpiresAt.IsZero() || !f.ExpiresAt.After(f.IssuedAt) {
		return fmt.Errorf("operation fence validity window is invalid")
	}
	if f.ExpiresAt.Sub(f.IssuedAt) > 24*time.Hour {
		return fmt.Errorf("operation fence validity exceeds 24h")
	}
	if !now.IsZero() {
		if f.IssuedAt.After(now.Add(time.Minute)) {
			return fmt.Errorf("operation fence was issued in the future")
		}
		if !now.Before(f.ExpiresAt) {
			return fmt.Errorf("operation fence has expired")
		}
	}
	if len(f.TargetNodeIDs) == 0 || !sort.StringsAreSorted(f.TargetNodeIDs) {
		return fmt.Errorf("operation fence targets must be non-empty and sorted")
	}
	seen := make(map[string]struct{}, len(f.TargetNodeIDs))
	for _, nodeID := range f.TargetNodeIDs {
		if err := ValidateNodeID(nodeID); err != nil {
			return fmt.Errorf("operation fence target identity is invalid: %w", err)
		}
		if _, ok := seen[nodeID]; ok {
			return fmt.Errorf("operation fence target %s is duplicated", nodeID)
		}
		seen[nodeID] = struct{}{}
	}
	if strings.TrimSpace(f.Signature) == "" {
		return fmt.Errorf("operation fence signature is required")
	}
	return nil
}

func OperationHolderTokenHash(holderToken string) (string, error) {
	if strings.TrimSpace(holderToken) == "" || len(holderToken) > 256 {
		return "", fmt.Errorf("operation holder credential is invalid")
	}
	digest := sha256.Sum256([]byte(holderToken))
	return hex.EncodeToString(digest[:]), nil
}

func (f OperationFence) Targets(nodeID string) bool {
	nodeID = strings.ToLower(strings.TrimSpace(nodeID))
	index := sort.SearchStrings(f.TargetNodeIDs, nodeID)
	return index < len(f.TargetNodeIDs) && f.TargetNodeIDs[index] == nodeID
}

func operationFenceEvidence(f OperationFence) ([]byte, error) {
	f.Signature = ""
	return json.Marshal(f)
}

func SignOperationFence(fence *OperationFence, installation *Installation) error {
	if fence == nil || installation == nil || fence.ClusterID != installation.ClusterID || fence.ControllerNodeID != installation.NodeID {
		return fmt.Errorf("operation fence does not match controller identity")
	}
	originalSignature := fence.Signature
	fence.Signature = "pending"
	if err := fence.Validate(time.Time{}); err != nil {
		fence.Signature = originalSignature
		return err
	}
	fence.Signature = ""
	message, err := operationFenceEvidence(*fence)
	if err != nil {
		return err
	}
	fence.Signature, err = installation.SignAllocation(message)
	return err
}

func VerifyOperationFence(fence OperationFence, controllerPublicKey string, now time.Time) error {
	if err := fence.Validate(now); err != nil {
		return err
	}
	message, err := operationFenceEvidence(fence)
	if err != nil {
		return err
	}
	return VerifyAllocationSignature(controllerPublicKey, message, fence.Signature)
}
