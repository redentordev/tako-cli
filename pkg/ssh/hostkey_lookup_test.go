package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func writeKnownHostsFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func generateHostKeyFixture(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return sshPub
}

func TestLookupRecordedHostKeyPlainEntry(t *testing.T) {
	key := generateHostKeyFixture(t)
	path := writeKnownHostsFixture(t,
		"# comment",
		knownhosts.Line([]string{"203.0.113.7"}, key),
	)

	got, err := lookupRecordedHostKeyIn("203.0.113.7", 22, []string{path})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatal("lookup returned nil for recorded host")
	}
	if got.Type != "ssh-ed25519" {
		t.Fatalf("type = %q, want ssh-ed25519", got.Type)
	}
	if got.Fingerprint != ssh.FingerprintSHA256(key) {
		t.Fatalf("fingerprint = %q, want %q", got.Fingerprint, ssh.FingerprintSHA256(key))
	}
	if got.Key == "" || strings.Contains(got.Key, " ") {
		t.Fatalf("key = %q, want bare base64", got.Key)
	}
}

func TestLookupRecordedHostKeyNonDefaultPortEntry(t *testing.T) {
	key := generateHostKeyFixture(t)
	path := writeKnownHostsFixture(t, knownhosts.Line([]string{"[203.0.113.7]:2222"}, key))

	got, err := lookupRecordedHostKeyIn("203.0.113.7", 2222, []string{path})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatal("lookup returned nil for recorded host with port")
	}

	miss, err := lookupRecordedHostKeyIn("203.0.113.7", 22, []string{path})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if miss != nil {
		t.Fatal("port-22 lookup matched a port-2222 entry")
	}
}

func TestLookupRecordedHostKeyHashedEntry(t *testing.T) {
	key := generateHostKeyFixture(t)
	hashed := knownhosts.HashHostname("203.0.113.7")
	line := hashed + " " + key.Type() + " " + strings.Fields(string(ssh.MarshalAuthorizedKey(key)))[1]
	path := writeKnownHostsFixture(t, line)

	got, err := lookupRecordedHostKeyIn("203.0.113.7", 22, []string{path})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatal("lookup returned nil for hashed entry")
	}
	if got.Fingerprint != ssh.FingerprintSHA256(key) {
		t.Fatalf("fingerprint = %q, want %q", got.Fingerprint, ssh.FingerprintSHA256(key))
	}
}

func TestLookupRecordedHostKeyMissingFilesAndHosts(t *testing.T) {
	got, err := lookupRecordedHostKeyIn("203.0.113.7", 22, []string{filepath.Join(t.TempDir(), "absent")})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != nil {
		t.Fatal("lookup found a key with no known_hosts files")
	}
}
