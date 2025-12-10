package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Multiplexer manages SSH connection multiplexing for improved performance
type Multiplexer struct {
	config      MultiplexConfig
	connections map[string]*MuxConnection
	mu          sync.RWMutex
}

// MultiplexConfig holds multiplexing configuration
type MultiplexConfig struct {
	ControlPath    string
	ControlPersist time.Duration
	MaxSessions    int
}

// MuxConnection represents a multiplexed SSH connection
type MuxConnection struct {
	key         string
	host        string
	port        int
	user        string
	sshKey      string
	controlPath string
	established time.Time
	lastUsed    time.Time
	mu          sync.Mutex
}

// NewMultiplexer creates a new SSH multiplexer
func NewMultiplexer() *Multiplexer {
	controlDir := filepath.Join(os.TempDir(), "tako-ssh-mux")
	os.MkdirAll(controlDir, 0700)

	return &Multiplexer{
		config: MultiplexConfig{
			ControlPath:    filepath.Join(controlDir, "mux-%h-%p-%r"),
			ControlPersist: 10 * time.Minute,
			MaxSessions:    10,
		},
		connections: make(map[string]*MuxConnection),
	}
}

// GetConnection retrieves or creates a multiplexed connection
func (m *Multiplexer) GetConnection(host string, port int, user string, sshKey string) (*MuxConnection, error) {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)

	// Try to get existing connection
	m.mu.RLock()
	conn, exists := m.connections[key]
	m.mu.RUnlock()

	if exists && conn.IsHealthy() {
		conn.mu.Lock()
		conn.lastUsed = time.Now()
		conn.mu.Unlock()
		return conn, nil
	}

	// Create new connection
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	conn, exists = m.connections[key]
	if exists && conn.IsHealthy() {
		conn.mu.Lock()
		conn.lastUsed = time.Now()
		conn.mu.Unlock()
		return conn, nil
	}

	// Establish new multiplexed connection
	newConn, err := m.establish(host, port, user, sshKey)
	if err != nil {
		return nil, fmt.Errorf("failed to establish mux connection: %w", err)
	}

	m.connections[key] = newConn
	return newConn, nil
}

// establish creates a new multiplexed SSH connection
func (m *Multiplexer) establish(host string, port int, user string, sshKey string) (*MuxConnection, error) {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)
	controlPath := m.buildControlPath(host, port, user)

	// Get Tako's known_hosts file path for host key verification
	takoKnownHosts, _ := GetKnownHostsPath()

	// Build SSH command with multiplexing options
	// Use proper host key verification based on current mode
	args := []string{
		"-fNM", // Background master mode
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=%s", controlPath),
		"-o", fmt.Sprintf("ControlPersist=%d", int(m.config.ControlPersist.Seconds())),
		"-o", "ServerAliveInterval=60",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
		"-p", fmt.Sprintf("%d", port),
	}

	// Configure host key verification based on global mode
	switch GetGlobalHostKeyMode() {
	case HostKeyModeInsecure:
		args = append(args, "-o", "StrictHostKeyChecking=no")
		args = append(args, "-o", "UserKnownHostsFile=/dev/null")
	case HostKeyModeStrict:
		args = append(args, "-o", "StrictHostKeyChecking=yes")
		if takoKnownHosts != "" {
			args = append(args, "-o", fmt.Sprintf("UserKnownHostsFile=%s", takoKnownHosts))
		}
	default: // TOFU or Ask
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
		if takoKnownHosts != "" {
			args = append(args, "-o", fmt.Sprintf("UserKnownHostsFile=%s", takoKnownHosts))
		}
	}

	if sshKey != "" {
		args = append(args, "-i", sshKey)
	}

	args = append(args, fmt.Sprintf("%s@%s", user, host))

	cmd := exec.Command("ssh", args...)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to establish master connection: %w", err)
	}

	conn := &MuxConnection{
		key:         key,
		host:        host,
		port:        port,
		user:        user,
		sshKey:      sshKey,
		controlPath: controlPath,
		established: time.Now(),
		lastUsed:    time.Now(),
	}

	return conn, nil
}

// buildControlPath generates the control socket path
func (m *Multiplexer) buildControlPath(host string, port int, user string) string {
	// Create a safe filename from connection details
	safeHost := filepath.Base(host)
	return filepath.Join(
		filepath.Dir(m.config.ControlPath),
		fmt.Sprintf("mux-%s-%d-%s", safeHost, port, user),
	)
}

// IsHealthy checks if the multiplexed connection is still active
func (c *MuxConnection) IsHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if control socket exists
	if _, err := os.Stat(c.controlPath); os.IsNotExist(err) {
		return false
	}

	// Check with SSH control check command
	cmd := exec.Command("ssh",
		"-O", "check",
		"-o", fmt.Sprintf("ControlPath=%s", c.controlPath),
		fmt.Sprintf("%s@%s", c.user, c.host),
	)

	return cmd.Run() == nil
}

// Execute runs a command using the multiplexed connection
func (c *MuxConnection) Execute(command string) (string, error) {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()

	args := []string{
		"-o", fmt.Sprintf("ControlPath=%s", c.controlPath),
		"-o", "ControlMaster=no",
		"-p", fmt.Sprintf("%d", c.port),
	}

	if c.sshKey != "" {
		args = append(args, "-i", c.sshKey)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", c.user, c.host),
		command,
	)

	cmd := exec.Command("ssh", args...)
	output, err := cmd.CombinedOutput()

	return string(output), err
}

// Close closes the multiplexed connection
func (c *MuxConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmd := exec.Command("ssh",
		"-O", "exit",
		"-o", fmt.Sprintf("ControlPath=%s", c.controlPath),
		fmt.Sprintf("%s@%s", c.user, c.host),
	)

	if err := cmd.Run(); err != nil {
		// Try to remove the socket file manually
		os.Remove(c.controlPath)
	}

	return nil
}

// CloseAll closes all multiplexed connections
func (m *Multiplexer) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, conn := range m.connections {
		conn.Close()
	}

	m.connections = make(map[string]*MuxConnection)
}

// Cleanup removes stale connections
func (m *Multiplexer) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, conn := range m.connections {
		if !conn.IsHealthy() {
			conn.Close()
			delete(m.connections, key)
		}
	}
}
