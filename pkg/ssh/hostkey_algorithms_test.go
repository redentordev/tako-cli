package ssh

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestKnownHostKeyTypesInCollectsTrustedTypesForHost(t *testing.T) {
	ed := generateHostKeyFixture(t)
	ec := ecdsaHostSigner(t).PublicKey()
	other := generateHostKeyFixture(t)
	revoked := ecdsaHostSigner(t).PublicKey()
	path := writeKnownHostsFixture(t,
		knownhosts.Line([]string{"203.0.113.7"}, ed),
		knownhosts.Line([]string{"[203.0.113.7]:2222"}, ec),
		knownhosts.Line([]string{"198.51.100.9"}, other),
		"@revoked "+knownhosts.Line([]string{"203.0.113.7"}, revoked),
	)

	got := knownHostKeyTypesIn("203.0.113.7", 22, []string{path})
	if want := map[string]bool{ssh.KeyAlgoED25519: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("port 22 types = %v, want %v", got, want)
	}
	got = knownHostKeyTypesIn("203.0.113.7", 2222, []string{path})
	if want := map[string]bool{ssh.KeyAlgoECDSA256: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("port 2222 types = %v, want %v", got, want)
	}
	if got := knownHostKeyTypesIn("192.0.2.1", 22, []string{path}); len(got) != 0 {
		t.Fatalf("unknown host types = %v, want none", got)
	}
}

func TestHostKeyAlgorithmsForTypesOrdersStrongestFirst(t *testing.T) {
	got := hostKeyAlgorithmsForTypes(map[string]bool{
		ssh.KeyAlgoRSA:      true,
		ssh.KeyAlgoECDSA256: true,
		ssh.KeyAlgoED25519:  true,
	})
	want := []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("algorithms = %v, want %v", got, want)
	}
	if got := hostKeyAlgorithmsForTypes(map[string]bool{}); got != nil {
		t.Fatalf("empty types = %v, want nil", got)
	}
}

func TestPinnedClientOffersOnlyPinnedKeyType(t *testing.T) {
	key := ecdsaHostSigner(t).PublicKey()
	recorded := RecordedHostKey{Type: key.Type(), Key: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
	client, err := NewClientFromConfigPinned(ServerConfig{Host: "node.example", User: "root", Password: "test"}, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.hostKeyAlgorithms("node.example:22"), []string{ssh.KeyAlgoECDSA256}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned algorithms = %v, want %v", got, want)
	}
}

// A server holding both ed25519 and ecdsa host keys negotiates ecdsa with Go's
// default preference order. With only the ed25519 key in known_hosts, the
// client must offer ed25519 instead of reporting a changed host key.
func TestConnectNegotiatesHostKeyTypeRecordedInKnownHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "1")

	edPublic, edPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edSigner, err := ssh.NewSignerFromKey(edPrivate)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveSSHHandshakes(t, edSigner, ecdsaHostSigner(t), rsaHostSigner(t))

	edKey, err := ssh.NewPublicKey(edPublic)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(addr)
	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{fmt.Sprintf("[%s]:%s", host, port)}, edKey)
	if err := os.WriteFile(knownHostsPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}

	portNumber := 0
	fmt.Sscanf(port, "%d", &portNumber)
	client := &Client{
		config:             &ssh.ClientConfig{User: "tako", HostKeyCallback: callback, Timeout: 5 * time.Second},
		host:               host,
		port:               portNumber,
		verifiesKnownHosts: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.ConnectContext(ctx); err != nil {
		t.Fatalf("connect with ed25519-only known_hosts: %v", err)
	}
	client.Close()
}

func serveSSHHandshakes(t *testing.T, hostKeys ...ssh.Signer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	config := &ssh.ServerConfig{NoClientAuth: true}
	for _, key := range hostKeys {
		config.AddHostKey(key)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				serverConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer serverConn.Close()
				go ssh.DiscardRequests(reqs)
				for newChannel := range chans {
					newChannel.Reject(ssh.Prohibited, "test server")
				}
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func ecdsaHostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func rsaHostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
