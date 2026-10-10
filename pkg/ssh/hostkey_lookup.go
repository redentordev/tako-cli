package ssh

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// NewClientFromConfigPinned constructs an SSH client whose in-memory callback
// accepts only the exact host key captured during enrollment. It is immune to
// later known_hosts removal, TOFU mode changes, and reconnect races.
func NewClientFromConfigPinned(config ServerConfig, expected RecordedHostKey) (*Client, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(expected.Key))
	if err != nil || expected.Type == "" || expected.Fingerprint == "" {
		return nil, fmt.Errorf("pinned SSH host key is invalid")
	}
	parsed, err := ssh.ParsePublicKey(keyBytes)
	if err != nil || parsed.Type() != expected.Type || ssh.FingerprintSHA256(parsed) != expected.Fingerprint {
		return nil, fmt.Errorf("pinned SSH host key fields are inconsistent")
	}
	client, err := NewClientFromConfig(config)
	if err != nil {
		return nil, err
	}
	client.config.HostKeyCallback = func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		if key.Type() != expected.Type || fingerprint != expected.Fingerprint || subtle.ConstantTimeCompare(key.Marshal(), keyBytes) != 1 {
			return fmt.Errorf("SSH host key for %s does not match the key pinned during platform enrollment", hostname)
		}
		return nil
	}
	// Offer only the pinned key type so a server holding several host keys
	// presents the pinned one instead of failing the pin.
	client.verifiesKnownHosts = false
	client.pinnedHostKeyAlgorithms = hostKeyAlgorithmsForTypes(map[string]bool{expected.Type: true})
	return client, nil
}

// hostKeyAlgorithmPreference orders known key types strongest first.
var hostKeyAlgorithmPreference = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoRSA,
}

// hostKeyAlgorithmsForTypes maps recorded host key types to the algorithms a
// client may negotiate for them. RSA keys sign with SHA-2 when the server
// supports it, falling back to ssh-rsa for old servers.
func hostKeyAlgorithmsForTypes(types map[string]bool) []string {
	var algorithms []string
	add := func(keyType string) {
		if keyType == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
			return
		}
		algorithms = append(algorithms, keyType)
	}
	remaining := make(map[string]bool, len(types))
	for keyType, ok := range types {
		if ok && keyType != "" {
			remaining[keyType] = true
		}
	}
	for _, keyType := range hostKeyAlgorithmPreference {
		if remaining[keyType] {
			add(keyType)
			delete(remaining, keyType)
		}
	}
	rest := make([]string, 0, len(remaining))
	for keyType := range remaining {
		rest = append(rest, keyType)
	}
	sort.Strings(rest)
	for _, keyType := range rest {
		add(keyType)
	}
	return algorithms
}

// knownHostKeyAlgorithms returns the host key algorithms to offer when dialing
// addr (host:port): those for key types already recorded in Tako's or the
// user's known_hosts. It returns nil when nothing is recorded, so first-use
// connections negotiate normally and TOFU records whatever the server offers.
func knownHostKeyAlgorithms(addr string) []string {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return hostKeyAlgorithmsForTypes(knownHostKeyTypesIn(host, port, []string{
		filepath.Join(homeDir, ".tako", "known_hosts"),
		filepath.Join(homeDir, ".ssh", "known_hosts"),
	}))
}

// knownHostKeyTypesIn returns the key types recorded for host:port across
// files. Revoked and @cert-authority lines are not trusted host keys and are
// skipped; host key verification still rejects revoked keys.
func knownHostKeyTypesIn(host string, port int, files []string) map[string]bool {
	addresses := knownHostsAddresses(host, port)
	types := map[string]bool{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			marker, hosts, pubKey, _, _, err := ssh.ParseKnownHosts([]byte(line))
			if err != nil || marker != "" {
				continue
			}
			for _, pattern := range hosts {
				for _, candidate := range addresses {
					if knownHostsPatternMatches(pattern, candidate) {
						types[pubKey.Type()] = true
					}
				}
			}
		}
	}
	return types
}

// RecordedHostKey is the SSH host key recorded for a host in a known_hosts
// file. Key is the base64-encoded wire form; Fingerprint is SHA256.
type RecordedHostKey struct {
	Type        string
	Key         string
	Fingerprint string
}

// LookupRecordedHostKey returns the host key recorded for host:port in
// Tako's known_hosts (~/.tako/known_hosts), falling back to the user's
// ~/.ssh/known_hosts. It returns nil with no error when no key is recorded.
func LookupRecordedHostKey(host string, port int) (*RecordedHostKey, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return lookupRecordedHostKeyIn(host, port, []string{
		filepath.Join(homeDir, ".tako", "known_hosts"),
		filepath.Join(homeDir, ".ssh", "known_hosts"),
	})
}

func lookupRecordedHostKeyIn(host string, port int, files []string) (*RecordedHostKey, error) {
	addresses := knownHostsAddresses(host, port)
	var recorded *RecordedHostKey
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			marker, hosts, pubKey, _, _, err := ssh.ParseKnownHosts([]byte(line))
			if err != nil {
				continue
			}
			for _, pattern := range hosts {
				for _, addr := range addresses {
					if knownHostsPatternMatches(pattern, addr) {
						if marker == "revoked" {
							return nil, fmt.Errorf("SSH host key for %s is explicitly revoked in %s", addr, path)
						}
						if recorded == nil {
							recorded = &RecordedHostKey{
								Type:        pubKey.Type(),
								Key:         base64.StdEncoding.EncodeToString(pubKey.Marshal()),
								Fingerprint: ssh.FingerprintSHA256(pubKey),
							}
						}
					}
				}
			}
		}
	}
	return recorded, nil
}

// knownHostsAddresses lists the address spellings a host may be recorded
// under: bare host for the default port, bracketed host:port otherwise.
func knownHostsAddresses(host string, port int) []string {
	if port == 22 || port == 0 {
		return []string{host, fmt.Sprintf("[%s]:22", host)}
	}
	return []string{fmt.Sprintf("[%s]:%d", host, port)}
}

// knownHostsPatternMatches reports whether one known_hosts host pattern
// covers addr, handling hashed entries (|1|salt|hash = HMAC-SHA1).
func knownHostsPatternMatches(pattern string, addr string) bool {
	if strings.HasPrefix(pattern, "|1|") {
		parts := strings.Split(pattern, "|")
		if len(parts) != 4 {
			return false
		}
		salt, err := base64.StdEncoding.DecodeString(parts[2])
		if err != nil {
			return false
		}
		hash, err := base64.StdEncoding.DecodeString(parts[3])
		if err != nil {
			return false
		}
		mac := hmac.New(sha1.New, salt)
		mac.Write([]byte(addr))
		return hmac.Equal(mac.Sum(nil), hash)
	}
	return pattern == addr
}
