package ssh

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Client wraps an SSH connection with additional functionality
type Client struct {
	config    *ssh.ClientConfig
	host      string
	port      int
	conn      *ssh.Client
	agentConn net.Conn // Track SSH agent connection for cleanup
	mu        sync.Mutex
}

// parsePrivateKey parses an SSH private key, handling various formats
// Supports: RSA, Ed25519, ECDSA, DSA keys in PEM or OpenSSH format
// AWS .pem files are standard PEM format and work automatically
func parsePrivateKey(key []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		// Check if this is a passphrase-protected key
		if err.Error() == "ssh: this private key is passphrase protected" {
			return nil, fmt.Errorf("SSH key is passphrase-protected. Tako does not support passphrase-protected keys.\n" +
				"Options:\n" +
				"  1. Use ssh-agent: ssh-add ~/.ssh/your_key (then Tako will use agent forwarding)\n" +
				"  2. Create an unencrypted copy: ssh-keygen -p -f ~/.ssh/your_key -m PEM -N ''\n" +
				"  3. Use password authentication instead: password: ${SSH_PASSWORD}")
		}
		// Check for common format issues
		if !isPEMFormat(key) && !isOpenSSHFormat(key) {
			return nil, fmt.Errorf("failed to parse SSH key: unrecognized key format. Expected PEM or OpenSSH format")
		}
		return nil, fmt.Errorf("failed to parse SSH key: %w", err)
	}
	return signer, nil
}

// isPEMFormat checks if the key data looks like PEM format
func isPEMFormat(data []byte) bool {
	return len(data) > 11 && string(data[:11]) == "-----BEGIN "
}

// isOpenSSHFormat checks if the key data looks like OpenSSH format
func isOpenSSHFormat(data []byte) bool {
	return len(data) > 36 && string(data[:36]) == "-----BEGIN OPENSSH PRIVATE KEY-----"
}

// getHostKeyCallback returns the appropriate host key callback based on global settings
func getHostKeyCallback() (ssh.HostKeyCallback, error) {
	verifier, err := GetDefaultVerifier()
	if err != nil {
		return nil, fmt.Errorf("host key verification failed: %w\n\n"+
			"SSH connections require host key verification for security.\n"+
			"To bypass (NOT recommended), set TAKO_HOST_KEY_MODE=insecure", err)
	}
	return verifier.GetCallback(), nil
}

// agentAuthResult holds both the auth method and connection for proper cleanup
type agentAuthResult struct {
	authMethod ssh.AuthMethod
	conn       net.Conn
}

// getSSHAgentAuth returns an ssh.AuthMethod using the SSH agent if available
// Also returns the connection so it can be properly closed later
func getSSHAgentAuth() *agentAuthResult {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil
	}

	agentClient := agent.NewClient(conn)
	return &agentAuthResult{
		authMethod: ssh.PublicKeysCallback(agentClient.Signers),
		conn:       conn,
	}
}

// Pool manages a pool of SSH connections
type Pool struct {
	clients map[string]*Client
	mu      sync.RWMutex
}

// NewPool creates a new SSH connection pool
func NewPool() *Pool {
	return &Pool{
		clients: make(map[string]*Client),
	}
}

// NewClient creates a new SSH client with key-based authentication
// Also tries ssh-agent for passphrase-protected keys
func NewClient(host string, port int, user string, keyPath string) (*Client, error) {
	var authMethods []ssh.AuthMethod
	var agentConn net.Conn // Track agent connection for cleanup

	// Try to read and parse the key file
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH key: %w", err)
	}

	// Parse the private key
	signer, err := parsePrivateKey(key)
	if err != nil {
		// If key parsing failed (e.g., passphrase protected), try ssh-agent
		if agentAuth := getSSHAgentAuth(); agentAuth != nil {
			authMethods = append(authMethods, agentAuth.authMethod)
			agentConn = agentAuth.conn
		} else {
			// No agent available, return the original error
			return nil, err
		}
	} else {
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	// Also add ssh-agent as fallback if available (only if we don't already have an agent connection)
	if len(authMethods) == 1 && agentConn == nil {
		if agentAuth := getSSHAgentAuth(); agentAuth != nil {
			authMethods = append(authMethods, agentAuth.authMethod)
			agentConn = agentAuth.conn
		}
	}

	// Get host key callback from verifier
	hostKeyCallback, err := getHostKeyCallback()
	if err != nil {
		// Clean up agent connection on error
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("failed to setup host key verification: %w", err)
	}

	// Create SSH client config with optimized settings
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         60 * time.Second, // Increased to 60s for very slow/busy servers
		// Client version to avoid version negotiation issues
		ClientVersion: "SSH-2.0-Tako-CLI",
	}

	return &Client{
		config:    config,
		host:      host,
		port:      port,
		agentConn: agentConn,
	}, nil
}

// NewClientWithPassword creates a new SSH client with password-based authentication
func NewClientWithPassword(host string, port int, user string, password string) (*Client, error) {
	// Get host key callback from verifier
	hostKeyCallback, err := getHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("failed to setup host key verification: %w", err)
	}

	// Create SSH client config with password authentication
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: hostKeyCallback,
		Timeout:         60 * time.Second, // Increased to 60s for very slow/busy servers
		// Client version to avoid version negotiation issues
		ClientVersion: "SSH-2.0-Tako-CLI",
	}

	return &Client{
		config: config,
		host:   host,
		port:   port,
	}, nil
}

// NewClientWithAuth creates a new SSH client with either key or password authentication
// If both keyPath and password are provided, key-based auth is preferred
// Also tries ssh-agent as fallback for passphrase-protected keys
func NewClientWithAuth(host string, port int, user string, keyPath string, password string) (*Client, error) {
	var authMethods []ssh.AuthMethod
	var agentConn net.Conn // Track agent connection for cleanup

	// Try key-based authentication first if keyPath is provided
	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err == nil {
			signer, err := parsePrivateKey(key)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			} else {
				// Key parsing failed, try ssh-agent
				if agentAuth := getSSHAgentAuth(); agentAuth != nil {
					authMethods = append(authMethods, agentAuth.authMethod)
					agentConn = agentAuth.conn
				}
			}
		}
	}

	// Try ssh-agent if no key methods yet
	if len(authMethods) == 0 && agentConn == nil {
		if agentAuth := getSSHAgentAuth(); agentAuth != nil {
			authMethods = append(authMethods, agentAuth.authMethod)
			agentConn = agentAuth.conn
		}
	}

	// Add password authentication as fallback or primary if no key
	if password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}

	// Ensure we have at least one auth method
	if len(authMethods) == 0 {
		// Clean up agent connection on error
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("no valid authentication method provided (need either SSH key or password)")
	}

	// Get host key callback from verifier
	hostKeyCallback, err := getHostKeyCallback()
	if err != nil {
		// Clean up agent connection on error
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("failed to setup host key verification: %w", err)
	}

	// Create SSH client config
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         60 * time.Second, // Increased to 60s for very slow/busy servers
		ClientVersion:   "SSH-2.0-Tako-CLI",
	}

	return &Client{
		config:    config,
		host:      host,
		port:      port,
		agentConn: agentConn,
	}, nil
}

// ServerConfig represents SSH connection parameters
// This mirrors config.ServerConfig to avoid circular imports
type ServerConfig struct {
	Host     string
	Port     int
	User     string
	SSHKey   string
	Password string
}

// NewClientFromConfig creates a new SSH client from server configuration
// Automatically chooses between key-based and password-based authentication
func NewClientFromConfig(cfg ServerConfig) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}

	// Use appropriate auth method based on config
	if cfg.Password != "" && cfg.SSHKey == "" {
		return NewClientWithPassword(cfg.Host, cfg.Port, cfg.User, cfg.Password)
	}

	if cfg.SSHKey != "" || cfg.Password != "" {
		return NewClientWithAuth(cfg.Host, cfg.Port, cfg.User, cfg.SSHKey, cfg.Password)
	}

	// Fallback to key-based (default key path should be set by validator)
	return NewClient(cfg.Host, cfg.Port, cfg.User, cfg.SSHKey)
}

// Connect establishes the SSH connection with retry logic and optimized TCP settings
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil // Already connected
	}

	addr := fmt.Sprintf("%s:%d", c.host, c.port)

	// Retry connection with exponential backoff
	maxRetries := 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Create TCP connection with custom dialer for better timeout control
		dialer := &net.Dialer{
			Timeout:   60 * time.Second, // Increased to 60s
			KeepAlive: 30 * time.Second,
		}

		// Establish TCP connection first
		tcpConn, err := dialer.Dial("tcp", addr)
		if err != nil {
			lastErr = fmt.Errorf("TCP dial failed: %w", err)
			if attempt < maxRetries {
				backoff := time.Duration(1<<uint(attempt-1)) * time.Second
				time.Sleep(backoff)
			}
			continue
		}

		// Now establish SSH connection over TCP
		sshConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, c.config)
		if err != nil {
			tcpConn.Close()
			lastErr = fmt.Errorf("SSH handshake failed: %w", err)
			if attempt < maxRetries {
				backoff := time.Duration(1<<uint(attempt-1)) * time.Second
				time.Sleep(backoff)
			}
			continue
		}

		// Create SSH client
		c.conn = ssh.NewClient(sshConn, chans, reqs)
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempts: %w", maxRetries, lastErr)
}

// Close closes the SSH connection and any associated resources
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error

	// Close SSH connection
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			lastErr = err
		}
		c.conn = nil
	}

	// Close SSH agent connection to prevent file descriptor leak
	if c.agentConn != nil {
		if err := c.agentConn.Close(); err != nil && lastErr == nil {
			lastErr = err
		}
		c.agentConn = nil
	}

	return lastErr
}

// Host returns the host address of the SSH connection
func (c *Client) Host() string {
	return c.host
}

// Port returns the port of the SSH connection
func (c *Client) Port() int {
	return c.port
}

// Execute runs a command on the remote server
func (c *Client) Execute(cmd string) (string, error) {
	return c.ExecuteWithContext(context.Background(), cmd)
}

// ExecuteWithContext runs a command with context support for cancellation
func (c *Client) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	conn, err := c.getConnection()
	if err != nil {
		return "", err
	}

	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Set up output capture
	type result struct {
		output []byte
		err    error
	}
	resultChan := make(chan result, 1)

	go func() {
		output, err := session.CombinedOutput(cmd)
		resultChan <- result{output: output, err: err}
	}()

	// Wait for completion or context cancellation
	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGTERM)
		return "", ctx.Err()
	case res := <-resultChan:
		if res.err != nil {
			// Return both the error and the output (which often contains the actual error message)
			return string(res.output), fmt.Errorf("command failed: %w", res.err)
		}
		return string(res.output), nil
	}
}

// ExecuteStream runs a command and streams output in real-time
func (c *Client) ExecuteStream(cmd string, stdout, stderr io.Writer) error {
	conn, err := c.getConnection()
	if err != nil {
		return err
	}

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	session.Stdout = stdout
	session.Stderr = stderr

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}

// StreamingSession represents a streaming SSH session
type StreamingSession struct {
	session *ssh.Session
	Stdout  io.Reader
	Stderr  io.Reader
}

// Close closes the streaming session
func (s *StreamingSession) Close() error {
	if s.session != nil {
		return s.session.Close()
	}
	return nil
}

// StartStream creates a new streaming session for long-running commands
func (c *Client) StartStream(cmd string) (*StreamingSession, error) {
	conn, err := c.getConnection()
	if err != nil {
		return nil, err
	}

	session, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := session.Start(cmd); err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	return &StreamingSession{
		session: session,
		Stdout:  stdout,
		Stderr:  stderr,
	}, nil
}

// IsConnected checks if the client is connected
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// IsHealthy checks if the connection is still alive by sending a keepalive request
// This helps detect stale connections that appear connected but are actually dead
func (c *Client) IsHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return false
	}

	// Send a keepalive request to verify the connection is still alive
	// This is more reliable than just checking if conn != nil
	_, _, err := c.conn.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// getConnection returns the current connection, establishing it if needed
// This method properly handles the connection state to avoid TOCTOU race conditions
func (c *Client) getConnection() (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		// Release lock during connection to avoid blocking other operations
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			c.mu.Lock() // Re-acquire before returning
			return nil, err
		}
		c.mu.Lock() // Re-acquire after connection
	}

	// Double-check connection is still valid
	if c.conn == nil {
		return nil, fmt.Errorf("connection failed: connection is nil after connect")
	}

	return c.conn, nil
}

// GetOrCreate gets an existing client from the pool or creates a new one
// Uses proper locking to avoid race conditions with double-checked locking
func (p *Pool) GetOrCreate(host string, port int, user string, keyPath string) (*Client, error) {
	return p.GetOrCreateWithAuth(host, port, user, keyPath, "")
}

// GetOrCreateWithAuth gets an existing client from the pool or creates a new one
// Supports both key-based and password-based authentication
// Automatically removes and replaces unhealthy connections
func (p *Pool) GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*Client, error) {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)

	// First, try with read lock
	p.mu.RLock()
	client, exists := p.clients[key]
	p.mu.RUnlock()

	if exists {
		// Verify connection is still healthy before returning
		if client.IsHealthy() {
			return client, nil
		}
		// Connection is stale, need to remove and recreate
		p.mu.Lock()
		// Double-check it's still the same client (another goroutine may have replaced it)
		if currentClient, stillExists := p.clients[key]; stillExists && currentClient == client {
			client.Close() // Clean up stale connection
			delete(p.clients, key)
		}
		p.mu.Unlock()
		// Fall through to create new connection
	}

	// Acquire write lock for creation
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check: another goroutine might have created it while we waited for the lock
	if client, exists = p.clients[key]; exists {
		// Verify health again under write lock
		if client.IsHealthy() {
			return client, nil
		}
		// Still stale, clean up
		client.Close()
		delete(p.clients, key)
	}

	// Create new client with appropriate authentication
	var err error
	if password != "" && keyPath == "" {
		// Password-only authentication
		client, err = NewClientWithPassword(host, port, user, password)
	} else if keyPath != "" || password != "" {
		// Key with optional password fallback
		client, err = NewClientWithAuth(host, port, user, keyPath, password)
	} else {
		return nil, fmt.Errorf("no authentication method provided")
	}

	if err != nil {
		return nil, err
	}

	// Connect the client (while holding the lock to prevent duplicate connections)
	if err := client.Connect(); err != nil {
		// Clean up on connection failure
		client.Close()
		return nil, err
	}

	// Add to pool
	p.clients[key] = client

	return client, nil
}

// Get retrieves a client from the pool
func (p *Pool) Get(host string, port int, user string) *Client {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.clients[key]
}

// CloseAll closes all connections in the pool
func (p *Pool) CloseAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var lastErr error
	for _, client := range p.clients {
		if err := client.Close(); err != nil {
			lastErr = err
		}
	}

	p.clients = make(map[string]*Client)
	return lastErr
}

// Remove removes a client from the pool and closes it
func (p *Pool) Remove(host string, port int, user string) error {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)

	p.mu.Lock()
	defer p.mu.Unlock()

	client, exists := p.clients[key]
	if !exists {
		return nil
	}

	err := client.Close()
	delete(p.clients, key)
	return err
}

// UploadReader uploads content from a reader to a remote file
func (c *Client) UploadReader(reader io.Reader, remotePath string, mode os.FileMode) error {
	// Create temporary local file
	tmpFile, err := os.CreateTemp("", "tako-upload-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write reader content to temp file
	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	tmpFile.Close()

	// Upload using existing CopyFile method
	return c.CopyFileWithMode(tmpFile.Name(), remotePath, mode)
}

// CopyFileWithMode uploads a file to the remote server with specific permissions
func (c *Client) CopyFileWithMode(localPath, remotePath string, mode os.FileMode) error {
	conn, err := c.getConnection()
	if err != nil {
		return err
	}

	// Read file content
	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("failed to read local file: %w", err)
	}

	// Create command to write file and set permissions
	// Use base64 encoding to safely transfer binary data
	cmd := fmt.Sprintf("base64 -d > %s && chmod %o %s", remotePath, mode, remotePath)

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Set up stdin pipe
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	// Start command
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Write base64-encoded content
	encoder := base64.NewEncoder(base64.StdEncoding, stdin)
	if _, err := encoder.Write(content); err != nil {
		stdin.Close()
		return fmt.Errorf("failed to write content: %w", err)
	}
	encoder.Close()
	stdin.Close()

	// Wait for completion
	if err := session.Wait(); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	return nil
}
