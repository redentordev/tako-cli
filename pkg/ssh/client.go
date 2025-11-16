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
)

// Client wraps an SSH connection with additional functionality
type Client struct {
	config *ssh.ClientConfig
	host   string
	port   int
	conn   *ssh.Client
	mu     sync.Mutex
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

// NewClient creates a new SSH client
func NewClient(host string, port int, user string, keyPath string) (*Client, error) {
	// Read the private key
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH key: %w", err)
	}

	// Parse the private key
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SSH key: %w", err)
	}

	// Create SSH client config with optimized settings
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: Implement proper host key verification
		Timeout:         60 * time.Second,            // Increased to 60s for very slow/busy servers
		// Client version to avoid version negotiation issues
		ClientVersion: "SSH-2.0-Tako-CLI",
	}

	return &Client{
		config: config,
		host:   host,
		port:   port,
	}, nil
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

// Close closes the SSH connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// Execute runs a command on the remote server
func (c *Client) Execute(cmd string) (string, error) {
	return c.ExecuteWithContext(context.Background(), cmd)
}

// ExecuteWithContext runs a command with context support for cancellation
func (c *Client) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return "", err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	session, err := c.conn.NewSession()
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
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	session, err := c.conn.NewSession()
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
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return nil, err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	session, err := c.conn.NewSession()
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

// GetOrCreate gets an existing client from the pool or creates a new one
func (p *Pool) GetOrCreate(host string, port int, user string, keyPath string) (*Client, error) {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)

	p.mu.RLock()
	client, exists := p.clients[key]
	p.mu.RUnlock()

	if exists {
		return client, nil
	}

	// Create new client
	client, err := NewClient(host, port, user, keyPath)
	if err != nil {
		return nil, err
	}

	// Connect the client
	if err := client.Connect(); err != nil {
		return nil, err
	}

	// Add to pool
	p.mu.Lock()
	p.clients[key] = client
	p.mu.Unlock()

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
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	// Read file content
	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("failed to read local file: %w", err)
	}

	// Create command to write file and set permissions
	// Use base64 encoding to safely transfer binary data
	cmd := fmt.Sprintf("base64 -d > %s && chmod %o %s", remotePath, mode, remotePath)

	session, err := c.conn.NewSession()
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
