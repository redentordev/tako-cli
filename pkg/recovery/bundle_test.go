package recovery

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestCreateAndVerifyRecoveryBundle(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "membership.json"), []byte("control state\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recovery.tar.gz")
	result, err := Create(path, "11111111-1111-4111-8111-111111111111", []Source{{Path: filepath.Join(root, "state"), Archive: "var/lib/tako/platform", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 == "" || len(result.Manifest.Entries) != 2 {
		t.Fatalf("result = %#v", result)
	}
	manifest, err := Verify(path, result.Manifest.ClusterID)
	if err != nil || len(manifest.Entries) != 2 {
		t.Fatalf("verify = %#v, %v", manifest, err)
	}
}

func TestRecoveryBundleRejectsUnsafeSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../../etc/shadow", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	_, err := Create(filepath.Join(t.TempDir(), "recovery.tar.gz"), "11111111-1111-4111-8111-111111111111", []Source{{Path: root, Archive: "state", Required: true}})
	if err == nil {
		t.Fatal("unsafe symlink was archived")
	}
}

func TestRecoveryBundleRejectsParentSymlinkAtSourceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("..", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(filepath.Join(t.TempDir(), "recovery.tar.gz"), "11111111-1111-4111-8111-111111111111", []Source{{Path: root, Archive: "state", Required: true}}); err == nil {
		t.Fatal("parent symlink was archived")
	}
}

func TestEncryptedRecoveryAuthenticatesVerifiesAndRestoresToStaging(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "membership.json"), []byte("control state\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encrypted := filepath.Join(t.TempDir(), "recovery.tako-recovery")
	result, err := CreateEncrypted(encrypted, "11111111-1111-4111-8111-111111111111", []Source{{Path: root, Archive: "var/lib/tako/control", Required: true}}, key)
	if err != nil || result.SHA256 == "" {
		t.Fatalf("encrypted result=%#v err=%v", result, err)
	}
	manifest, err := VerifyEncrypted(encrypted, result.Manifest.ClusterID, key)
	if err != nil || manifest.ClusterID != result.Manifest.ClusterID {
		t.Fatalf("verify encrypted: %#v %v", manifest, err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if _, err := RestoreEncrypted(encrypted, staging, result.Manifest.ClusterID, key); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(staging, "var/lib/tako/control/membership.json"))
	if err != nil || !bytes.Equal(data, []byte("control state\n")) {
		t.Fatalf("restored data=%q err=%v", data, err)
	}
	wrong := append([]byte(nil), key...)
	wrong[0] ^= 0xff
	if _, err := VerifyEncrypted(encrypted, result.Manifest.ClusterID, wrong); err == nil {
		t.Fatal("wrong recovery key was accepted")
	}
	tampered, err := os.ReadFile(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	tampered[len(tampered)-1] ^= 0xff
	tamperedPath := filepath.Join(t.TempDir(), "tampered.tako-recovery")
	if err := os.WriteFile(tamperedPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEncrypted(tamperedPath, result.Manifest.ClusterID, key); err == nil {
		t.Fatal("tampered recovery bundle was accepted")
	}
}

func TestSnapshotRefusesActiveControllerOperation(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "control"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "control", "control-operation.json")
	journal := func(expiresAt time.Time) []byte {
		holderToken := "test-recovery-holder-token"
		holderHash, err := nodeidentity.OperationHolderTokenHash(holderToken)
		if err != nil {
			t.Fatal(err)
		}
		issuedAt := expiresAt.Add(-time.Minute)
		fence := nodeidentity.OperationFence{
			Kind: nodeidentity.OperationFenceKind, ClusterID: "11111111-1111-4111-8111-111111111111", ControllerNodeID: "22222222-2222-4222-8222-222222222222",
			MembershipGeneration: 1, Project: "demo", Environment: "production", OperationID: "op-active", Operation: "deploy", Token: 1, HolderTokenHash: holderHash,
			TargetNodeIDs: []string{"22222222-2222-4222-8222-222222222222"}, IssuedAt: issuedAt, ExpiresAt: expiresAt, Signature: "structurally-present",
		}
		state := map[string]any{
			"schemaVersion": 1, "nextToken": 1, "updatedAt": issuedAt,
			"active": map[string]any{"requestId": "request", "requestFingerprint": strings.Repeat("a", 64), "holderToken": holderToken, "fence": fence, "phase": "mutating", "updatedAt": issuedAt},
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := os.WriteFile(path, journal(time.Now().UTC().Add(time.Hour)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoActiveControllerOperation(dataDir); err == nil {
		t.Fatal("snapshot accepted active controller operation")
	}
	idleJournal, err := json.Marshal(map[string]any{"schemaVersion": 1, "nextToken": 1, "active": nil, "updatedAt": time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, idleJournal, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoActiveControllerOperation(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, journal(time.Now().UTC().Add(-time.Minute)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoActiveControllerOperation(dataDir); err != nil {
		t.Fatalf("expired controller residue blocked snapshot: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoActiveControllerOperation(dataDir); err == nil {
		t.Fatal("malformed empty controller journal was accepted")
	}
	corruptHistory, err := json.Marshal(map[string]any{"schemaVersion": 1, "nextToken": 1, "updatedAt": time.Now().UTC(), "history": []any{map[string]any{"phase": "completed"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corruptHistory, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoActiveControllerOperation(dataDir); err == nil {
		t.Fatal("malformed controller history was accepted")
	}
}
