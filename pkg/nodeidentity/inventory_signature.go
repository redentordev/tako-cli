package nodeidentity

import (
	"encoding/json"
	"fmt"
	"time"
)

const SignedInventoryKind = "SignedClusterInventory"

// SignedInventorySnapshot lets the controller publish a newer trust snapshot
// to only the nodes an operation actually targets. Nodes verify it against
// their already trusted controller key and reject generation rollback.
type SignedInventorySnapshot struct {
	Kind      string           `json:"kind"`
	Inventory ClusterInventory `json:"inventory"`
	IssuedAt  time.Time        `json:"issuedAt"`
	Signature string           `json:"signature"`
}

func signedInventoryEvidence(snapshot SignedInventorySnapshot) ([]byte, error) {
	snapshot.Signature = ""
	return json.Marshal(snapshot)
}

func SignInventorySnapshot(snapshot *SignedInventorySnapshot, controller *Installation) error {
	if snapshot == nil || controller == nil || snapshot.Kind != SignedInventoryKind || snapshot.Inventory.ClusterID != controller.ClusterID || snapshot.Inventory.ControllerNodeID != controller.NodeID {
		return fmt.Errorf("inventory snapshot does not match controller identity")
	}
	if snapshot.IssuedAt.IsZero() {
		return fmt.Errorf("inventory snapshot issue time is required")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return err
	}
	evidence, err := signedInventoryEvidence(*snapshot)
	if err != nil {
		return err
	}
	snapshot.Signature, err = controller.SignAllocation(evidence)
	return err
}

func VerifyInventorySnapshot(snapshot SignedInventorySnapshot, controllerPublicKey string, now time.Time) error {
	if snapshot.Kind != SignedInventoryKind || snapshot.IssuedAt.IsZero() || snapshot.Signature == "" {
		return fmt.Errorf("signed inventory snapshot is incomplete")
	}
	if !now.IsZero() && snapshot.IssuedAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("signed inventory snapshot was issued in the future")
	}
	if err := snapshot.Inventory.Validate(); err != nil {
		return err
	}
	evidence, err := signedInventoryEvidence(snapshot)
	if err != nil {
		return err
	}
	return VerifyAllocationSignature(controllerPublicKey, evidence, snapshot.Signature)
}
