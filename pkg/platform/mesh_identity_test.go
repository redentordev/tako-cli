package platform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestEnsureMeshPublicKeyIsCreateOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wireguard")
	first, err := EnsureMeshPublicKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureMeshPublicKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("mesh identity changed: %q != %q", first, second)
	}
	if err := nodeidentity.ValidateMeshPublicKey(first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "privatekey"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private key mode = %v", info.Mode().Perm())
	}
}

func TestEnsureMeshPublicKeyRejectsSymlinkPrivateKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wireguard")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), filepath.Join(dir, "privatekey")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMeshPublicKey(dir); err == nil {
		t.Fatal("expected symlink private key rejection")
	}
}
