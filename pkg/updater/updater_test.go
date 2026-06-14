package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChecksumManifestFindsExactBinary(t *testing.T) {
	content := []byte("hello\n")
	sum := sha256.Sum256(content)
	checksum := hex.EncodeToString(sum[:])

	got, err := parseChecksumManifest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  tako-linux-arm64\n"+
			checksum+"  tako-linux-amd64\n",
		"tako-linux-amd64",
	)
	if err != nil {
		t.Fatalf("parseChecksumManifest returned error: %v", err)
	}
	if got != checksum {
		t.Fatalf("checksum = %q, want %q", got, checksum)
	}
}

func TestParseChecksumManifestAcceptsBinaryModeMarker(t *testing.T) {
	content := []byte("hello\n")
	sum := sha256.Sum256(content)
	checksum := hex.EncodeToString(sum[:])

	got, err := parseChecksumManifest(checksum+" *tako-darwin-arm64\n", "tako-darwin-arm64")
	if err != nil {
		t.Fatalf("parseChecksumManifest returned error: %v", err)
	}
	if got != checksum {
		t.Fatalf("checksum = %q, want %q", got, checksum)
	}
}

func TestParseChecksumManifestRejectsMissingBinary(t *testing.T) {
	_, err := parseChecksumManifest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  tako-linux-arm64\n", "tako-linux-amd64")
	if err == nil {
		t.Fatal("expected missing binary checksum to be rejected")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want not found context", err)
	}
}

func TestParseChecksumManifestRejectsMalformedDigest(t *testing.T) {
	_, err := parseChecksumManifest("not-a-sha  tako-linux-amd64\n", "tako-linux-amd64")
	if err == nil {
		t.Fatal("expected malformed checksum to be rejected")
	}
	if !strings.Contains(err.Error(), "not a valid SHA-256") {
		t.Fatalf("error = %q, want SHA-256 context", err)
	}
}

func TestVerifyFileChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako")
	content := []byte("binary")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	sum := sha256.Sum256(content)

	if err := verifyFileChecksum(path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("verifyFileChecksum returned error: %v", err)
	}
}

func TestVerifyFileChecksumRejectsMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako")
	if err := os.WriteFile(path, []byte("binary"), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := verifyFileChecksum(path, strings.Repeat("0", sha256.Size*2))
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %q, want mismatch context", err)
	}
}
