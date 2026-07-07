package ssh

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

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
			if err != nil || marker == "revoked" {
				continue
			}
			for _, pattern := range hosts {
				for _, addr := range addresses {
					if knownHostsPatternMatches(pattern, addr) {
						return &RecordedHostKey{
							Type:        pubKey.Type(),
							Key:         base64.StdEncoding.EncodeToString(pubKey.Marshal()),
							Fingerprint: ssh.FingerprintSHA256(pubKey),
						}, nil
					}
				}
			}
		}
	}
	return nil, nil
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
