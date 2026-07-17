package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

type controllerOperationJournalRecord struct {
	RequestID          string                       `json:"requestId"`
	RequestFingerprint string                       `json:"requestFingerprint"`
	HolderToken        string                       `json:"holderToken,omitempty"`
	Fence              nodeidentity.OperationFence  `json:"fence"`
	PreviousFence      *nodeidentity.OperationFence `json:"previousFence,omitempty"`
	Phase              string                       `json:"phase"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
	CompletedAt        time.Time                    `json:"completedAt,omitempty"`
}

type controllerOperationJournal struct {
	SchemaVersion int                                `json:"schemaVersion"`
	NextToken     uint64                             `json:"nextToken"`
	Active        *controllerOperationJournalRecord  `json:"active,omitempty"`
	History       []controllerOperationJournalRecord `json:"history,omitempty"`
	UpdatedAt     time.Time                          `json:"updatedAt"`
}

func openPlatformLock(dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "control")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("platform control directory must be a protected real directory")
	}
	return os.OpenFile(filepath.Join(dir, "platform-backup.lock"), os.O_CREATE|os.O_RDWR, 0600)
}

func openOperationBarrier(dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "control")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("platform control directory must be a protected real directory")
	}
	return os.OpenFile(filepath.Join(dir, "operation-maintenance.lock"), os.O_CREATE|os.O_RDWR, 0600)
}

// EnsureNoActiveControllerOperation is called only while the caller holds the
// exclusive snapshot lock. New HTTP operations are then blocked, so observing
// no active durable authority is a race-free snapshot precondition.
func EnsureNoActiveControllerOperation(dataDir string, controllerPublicKey ...string) error {
	path := filepath.Join(dataDir, "control", "control-operation.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return fmt.Errorf("controller operation state must be a private bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var state controllerOperationJournal
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode controller operation state before snapshot: %w", err)
	}
	if state.SchemaVersion != 1 || state.NextToken == 0 || state.UpdatedAt.IsZero() || state.UpdatedAt.After(time.Now().UTC().Add(time.Minute)) || len(state.History) > 256 {
		return fmt.Errorf("controller operation state schema is invalid")
	}
	for index := range state.History {
		record := &state.History[index]
		if err := validateControllerOperationJournalRecord(record, state.NextToken, false, firstString(controllerPublicKey)); err != nil {
			return fmt.Errorf("controller operation history is invalid: %w", err)
		}
	}
	if state.Active == nil {
		return nil
	}
	if err := validateControllerOperationJournalRecord(state.Active, state.NextToken, true, firstString(controllerPublicKey)); err != nil {
		return fmt.Errorf("controller operation state is invalid: %w", err)
	}
	if time.Now().UTC().Before(state.Active.Fence.ExpiresAt) {
		return fmt.Errorf("platform recovery snapshot refused while a controller operation is active")
	}
	return nil
}

func validateControllerOperationJournalRecord(record *controllerOperationJournalRecord, nextToken uint64, active bool, controllerPublicKey string) error {
	if record == nil || record.RequestID == "" || len(record.RequestFingerprint) != sha256.Size*2 ||
		strings.ToLower(record.RequestFingerprint) != record.RequestFingerprint || record.UpdatedAt.IsZero() || record.Fence.Token > nextToken {
		return fmt.Errorf("record identity or token is invalid")
	}
	if _, err := hex.DecodeString(record.RequestFingerprint); err != nil {
		return fmt.Errorf("record fingerprint is invalid")
	}
	if err := record.Fence.Validate(time.Time{}); err != nil {
		return fmt.Errorf("record fence is invalid")
	}
	if controllerPublicKey != "" {
		if err := nodeidentity.VerifyOperationFence(record.Fence, controllerPublicKey, time.Time{}); err != nil {
			return fmt.Errorf("record fence signature is invalid")
		}
	}
	if active {
		if record.HolderToken == "" || !record.CompletedAt.IsZero() || (record.Phase != "authority-acquired" && record.Phase != "targets-fenced" && record.Phase != "mutating") {
			return fmt.Errorf("active record shape is invalid")
		}
		holderHash, err := nodeidentity.OperationHolderTokenHash(record.HolderToken)
		if err != nil || holderHash != record.Fence.HolderTokenHash {
			return fmt.Errorf("active record holder credential is invalid")
		}
		if record.PreviousFence != nil {
			if err := record.PreviousFence.Validate(time.Time{}); err != nil || !journalFenceRenewal(*record.PreviousFence, record.Fence) {
				return fmt.Errorf("active predecessor fence is invalid")
			}
			if controllerPublicKey != "" {
				if err := nodeidentity.VerifyOperationFence(*record.PreviousFence, controllerPublicKey, time.Time{}); err != nil {
					return fmt.Errorf("active predecessor signature is invalid")
				}
			}
		}
		return nil
	}
	if record.HolderToken != "" || record.PreviousFence != nil || record.CompletedAt.IsZero() || (record.Phase != "completed" && record.Phase != "expired-reconciled") {
		return fmt.Errorf("history record shape is invalid")
	}
	return nil
}

func journalFenceRenewal(previous, current nodeidentity.OperationFence) bool {
	if previous.Kind != current.Kind || previous.ClusterID != current.ClusterID || previous.ControllerNodeID != current.ControllerNodeID ||
		previous.MembershipGeneration != current.MembershipGeneration || previous.Project != current.Project || previous.Environment != current.Environment ||
		previous.OperationID != current.OperationID || previous.Operation != current.Operation || previous.Token != current.Token ||
		previous.HolderTokenHash != current.HolderTokenHash || len(previous.TargetNodeIDs) != len(current.TargetNodeIDs) ||
		!current.IssuedAt.After(previous.IssuedAt) || !current.ExpiresAt.After(previous.ExpiresAt) {
		return false
	}
	for index := range previous.TargetNodeIDs {
		if previous.TargetNodeIDs[index] != current.TargetNodeIDs[index] {
			return false
		}
	}
	return true
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
