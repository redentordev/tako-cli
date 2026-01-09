package ssh

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyMode controls host key verification behavior
type HostKeyMode int

const (
	// HostKeyModeTOFU trusts on first use, verifies on subsequent connections (default)
	HostKeyModeTOFU HostKeyMode = iota
	// HostKeyModeStrict requires host to already be in known_hosts
	HostKeyModeStrict
	// HostKeyModeAsk prompts user for unknown hosts (interactive only)
	HostKeyModeAsk
	// HostKeyModeInsecure disables verification (not recommended, legacy behavior)
	HostKeyModeInsecure
)

// ParseHostKeyMode parses a string into HostKeyMode
func ParseHostKeyMode(s string) HostKeyMode {
	switch strings.ToLower(s) {
	case "strict":
		return HostKeyModeStrict
	case "ask":
		return HostKeyModeAsk
	case "insecure", "none", "off":
		return HostKeyModeInsecure
	default:
		return HostKeyModeTOFU
	}
}

// String returns string representation of HostKeyMode
func (m HostKeyMode) String() string {
	switch m {
	case HostKeyModeStrict:
		return "strict"
	case HostKeyModeAsk:
		return "ask"
	case HostKeyModeInsecure:
		return "insecure"
	default:
		return "tofu"
	}
}

// HostKeyVerifier provides host key verification with TOFU support
type HostKeyVerifier struct {
	mode           HostKeyMode
	systemHostsPath string  // ~/.ssh/known_hosts
	takoHostsPath   string  // ~/.tako/known_hosts
	promptFn       func(host, fingerprint, keyType string) (bool, error)
	mu             sync.Mutex
}

// Global verifier instance with default settings
var defaultVerifier *HostKeyVerifier
var defaultVerifierOnce sync.Once
var globalHostKeyMode = HostKeyModeTOFU

// SetGlobalHostKeyMode sets the global host key verification mode
func SetGlobalHostKeyMode(mode HostKeyMode) {
	globalHostKeyMode = mode
}

// GetGlobalHostKeyMode returns the current global host key verification mode
func GetGlobalHostKeyMode() HostKeyMode {
	return globalHostKeyMode
}

// GetDefaultVerifier returns the default host key verifier
func GetDefaultVerifier() (*HostKeyVerifier, error) {
	var initErr error
	defaultVerifierOnce.Do(func() {
		defaultVerifier, initErr = NewHostKeyVerifier(globalHostKeyMode)
	})
	if initErr != nil {
		return nil, initErr
	}
	// Update mode in case it changed
	defaultVerifier.SetMode(globalHostKeyMode)
	return defaultVerifier, nil
}

// NewHostKeyVerifier creates a new host key verifier
func NewHostKeyVerifier(mode HostKeyMode) (*HostKeyVerifier, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	// Create ~/.tako directory if it doesn't exist
	takoDir := filepath.Join(homeDir, ".tako")
	if err := os.MkdirAll(takoDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create .tako directory: %w", err)
	}

	return &HostKeyVerifier{
		mode:           mode,
		systemHostsPath: filepath.Join(homeDir, ".ssh", "known_hosts"),
		takoHostsPath:   filepath.Join(takoDir, "known_hosts"),
	}, nil
}

// SetMode sets the verification mode
func (v *HostKeyVerifier) SetMode(mode HostKeyMode) {
	v.mode = mode
}

// SetPromptFunc sets the function to prompt user for unknown hosts
func (v *HostKeyVerifier) SetPromptFunc(fn func(host, fingerprint, keyType string) (bool, error)) {
	v.promptFn = fn
}

// GetCallback returns an ssh.HostKeyCallback for use with ssh.ClientConfig
func (v *HostKeyVerifier) GetCallback() ssh.HostKeyCallback {
	if v.mode == HostKeyModeInsecure {
		return ssh.InsecureIgnoreHostKey()
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		return v.verify(hostname, remote, key)
	}
}

// verify performs the actual host key verification
func (v *HostKeyVerifier) verify(hostname string, remote net.Addr, key ssh.PublicKey) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Build list of known_hosts files to check
	var files []string
	if _, err := os.Stat(v.systemHostsPath); err == nil {
		files = append(files, v.systemHostsPath)
	}
	if _, err := os.Stat(v.takoHostsPath); err == nil {
		files = append(files, v.takoHostsPath)
	}

	// Try to verify against known_hosts files
	if len(files) > 0 {
		callback, err := knownhosts.New(files...)
		if err == nil {
			verifyErr := callback(hostname, remote, key)
			if verifyErr == nil {
				// Host key matches - success!
				return nil
			}

			// Check for key mismatch (potential MITM attack)
			var keyErr *knownhosts.KeyError
			if errors.As(verifyErr, &keyErr) && len(keyErr.Want) > 0 {
				// DANGER: Host key has changed!
				return v.hostKeyChangedError(hostname, key, keyErr.Want)
			}
			// Host not found in known_hosts - continue to handle unknown host
		}
	}

	// Host is unknown - handle based on mode
	host, port := extractHostPort(hostname, remote)
	fingerprint := ssh.FingerprintSHA256(key)

	switch v.mode {
	case HostKeyModeStrict:
		return fmt.Errorf("host key verification failed: %s is not in known_hosts\n"+
			"Fingerprint: %s (%s)\n\n"+
			"Options:\n"+
			"  1. Add host key manually: ssh-keyscan -H %s >> ~/.ssh/known_hosts\n"+
			"  2. Use TOFU mode: tako deploy --host-key-mode=tofu\n"+
			"  3. Set environment: TAKO_HOST_KEY_MODE=tofu",
			host, fingerprint, key.Type(), host)

	case HostKeyModeTOFU:
		// Trust on first use - save the key
		if err := v.addHostKey(host, port, key); err != nil {
			// Log warning but don't fail - connection is still valid
			fmt.Fprintf(os.Stderr, "Warning: Could not save host key for %s: %v\n", host, err)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: Permanently added '%s' (%s) to known hosts.\n",
				host, key.Type())
		}
		return nil

	case HostKeyModeAsk:
		if v.promptFn == nil {
			// No prompt function - fall back to TOFU behavior
			fmt.Fprintf(os.Stderr, "Warning: No interactive prompt available, using TOFU for %s\n", host)
			if err := v.addHostKey(host, port, key); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not save host key: %v\n", err)
			}
			return nil
		}

		accepted, err := v.promptFn(host, fingerprint, key.Type())
		if err != nil {
			return fmt.Errorf("failed to prompt for host key: %w", err)
		}

		if !accepted {
			return fmt.Errorf("host key verification failed: connection rejected by user for %s", host)
		}

		// User accepted - save the key
		if err := v.addHostKey(host, port, key); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not save host key: %v\n", err)
		}
		return nil
	}

	return fmt.Errorf("unknown host key verification mode")
}

// hostKeyChangedError returns a detailed error for host key mismatch
func (v *HostKeyVerifier) hostKeyChangedError(hostname string, key ssh.PublicKey, want []knownhosts.KnownKey) error {
	var wantTypes []string
	for _, k := range want {
		wantTypes = append(wantTypes, k.Key.Type())
	}

	host, _ := extractHostPort(hostname, nil)

	return fmt.Errorf(`
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!
Someone could be eavesdropping on you right now (man-in-the-middle attack)!
It is also possible that a host key has just been changed.

Host: %s
Received key type: %s
Received fingerprint: %s
Expected key types: %s

If this change is expected (e.g., server reinstall), remove the old key:
  ssh-keygen -R %s

Then run Tako again to accept the new key.

If this is unexpected, DO NOT PROCEED - investigate immediately!`,
		host,
		key.Type(),
		ssh.FingerprintSHA256(key),
		strings.Join(wantTypes, ", "),
		host)
}

// addHostKey adds a new host key to Tako's known_hosts file
func (v *HostKeyVerifier) addHostKey(host string, port int, key ssh.PublicKey) error {
	// Format address for known_hosts
	var addr string
	if port == 22 || port == 0 {
		addr = host
	} else {
		addr = fmt.Sprintf("[%s]:%d", host, port)
	}

	// Generate known_hosts line
	line := knownhosts.Line([]string{addr}, key)

	// Append to Tako's known_hosts file
	f, err := os.OpenFile(v.takoHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open known_hosts: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("failed to write host key: %w", err)
	}

	return nil
}

// extractHostPort extracts host and port from SSH connection info
func extractHostPort(hostname string, remote net.Addr) (string, int) {
	host, portStr, err := net.SplitHostPort(hostname)
	if err != nil {
		// Might not have port, try remote address
		if remote != nil {
			host, portStr, _ = net.SplitHostPort(remote.String())
		}
		if host == "" {
			host = hostname
		}
	}

	port := 22
	if portStr != "" {
		fmt.Sscanf(portStr, "%d", &port)
	}

	return host, port
}

// InteractivePrompt prompts the user to accept an unknown host key
// Use this with SetPromptFunc for interactive CLI sessions
func InteractivePrompt(host, fingerprint, keyType string) (bool, error) {
	fmt.Printf(`The authenticity of host '%s' can't be established.
%s key fingerprint is %s.
Are you sure you want to continue connecting (yes/no)? `, host, keyType, fingerprint)

	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	response = strings.TrimSpace(strings.ToLower(response))

	switch response {
	case "yes", "y":
		return true, nil
	case "no", "n":
		return false, nil
	default:
		fmt.Println("Please type 'yes' or 'no'.")
		return InteractivePrompt(host, fingerprint, keyType)
	}
}

// GetKnownHostsPath returns the path to Tako's known_hosts file
func GetKnownHostsPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".tako", "known_hosts"), nil
}

// RemoveHostKey removes a host key from Tako's known_hosts file
// This should be called when destroying infrastructure to allow clean reconnection
func RemoveHostKey(host string) error {
	knownHostsPath, err := GetKnownHostsPath()
	if err != nil {
		return err
	}

	// Read existing file
	data, err := os.ReadFile(knownHostsPath)
	if os.IsNotExist(err) {
		return nil // Nothing to remove
	}
	if err != nil {
		return fmt.Errorf("failed to read known_hosts: %w", err)
	}

	// Filter out lines matching the host
	lines := strings.Split(string(data), "\n")
	var newLines []string
	removed := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			newLines = append(newLines, line)
			continue
		}

		// Check if this line is for the host we want to remove
		// Format: host algo key [comment]
		// Also handle hashed hosts: |1|base64|base64
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			hostPart := parts[0]
			// Check if host matches directly or is in a comma-separated list
			if hostPart == host || strings.HasPrefix(hostPart, host+",") || strings.Contains(hostPart, ","+host) {
				removed = true
				continue // Skip this line (remove it)
			}
		}
		newLines = append(newLines, line)
	}

	if !removed {
		return nil // Host was not in file
	}

	// Write back
	content := strings.Join(newLines, "\n")
	if err := os.WriteFile(knownHostsPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write known_hosts: %w", err)
	}

	return nil
}
