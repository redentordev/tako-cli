package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestPinnedClientCallbackRejectsReconnectKeyChange(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	recorded := RecordedHostKey{Type: key.Type(), Key: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
	client, err := NewClientFromConfigPinned(ServerConfig{Host: "node.example", User: "root", Password: "test"}, recorded)
	if err != nil {
		t.Fatal(err)
	}
	callback := client.config.HostKeyCallback
	if err := callback("node.example:22", &net.TCPAddr{}, key); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(otherPublic)
	if err := callback("node.example:22", &net.TCPAddr{}, other); err == nil {
		t.Fatal("changed reconnect key was accepted")
	}
}

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

func TestLookupRecordedHostKeyTreatsRevokedMatchAsFatal(t *testing.T) {
	revoked := generateHostKeyFixture(t)
	stale := generateHostKeyFixture(t)
	path := writeKnownHostsFixture(t,
		knownhosts.Line([]string{"203.0.113.7"}, stale),
		"@revoked "+knownhosts.Line([]string{"203.0.113.7"}, revoked),
	)
	if got, err := lookupRecordedHostKeyIn("203.0.113.7", 22, []string{path}); err == nil || got != nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked host lookup = %#v, err=%v", got, err)
	}
}

func TestPinnedClientRejectsInconsistentStoredFields(t *testing.T) {
	key := generateHostKeyFixture(t)
	recorded := RecordedHostKey{Type: key.Type(), Key: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: "SHA256:not-the-key"}
	if _, err := NewClientFromConfigPinned(ServerConfig{Host: "node.example", User: "root", Password: "test"}, recorded); err == nil {
		t.Fatal("inconsistent pinned key fields were accepted")
	}
}
