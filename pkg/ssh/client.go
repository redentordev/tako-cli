package ssh

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/redentordev/tako-cli/pkg/takodclient"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

const DefaultCommandTimeout = 30 * time.Minute

const (
	connectDefaultTCPAttempts    = 3
	connectAutomationTCPAttempts = 6
	connectMaxHandshakeAttempts  = 3
	connectDefaultTimeout        = 10 * time.Second
	connectMaxTimeout            = 5 * time.Minute
	connectBackoffMax            = 10 * time.Second
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
		return nil, fmt.Errorf("host key verification failed: %w", err)
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
	clients     map[string]*Client
	mu          sync.RWMutex
	fenceSource takodclient.OperationFenceSource
}

func (p *Pool) SetOperationFenceSource(source takodclient.OperationFenceSource) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.fenceSource = source
	p.mu.Unlock()
}

func (p *Pool) OperationFenceSource() takodclient.OperationFenceSource {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fenceSource
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
		Timeout:         connectTimeout(),
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
		Timeout:         connectTimeout(),
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
		Timeout:         connectTimeout(),
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

// Connect establishes the SSH connection with retry logic and optimized TCP settings.
func (c *Client) Connect() error {
	return c.ConnectContext(context.Background())
}

// ConnectContext establishes the SSH connection with retry logic and context cancellation support.
func (c *Client) ConnectContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil // Already connected
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))

	var lastErr error
	handshakeAttempts := 0
	tcpAttempts := connectTCPAttempts()
	timeout := connectTimeout()

	for attempt := 1; attempt <= tcpAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Create TCP connection with custom dialer for better timeout control.
		dialer := &net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}

		// Establish TCP connection first.
		tcpConn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			lastErr = fmt.Errorf("TCP dial failed: %w", err)
			if attempt < tcpAttempts && isTransientDialError(err) {
				if err := sleepWithContext(ctx, connectBackoff(attempt)); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("failed to connect after %d attempt(s): %w", attempt, lastErr)
		}

		// Now establish SSH connection over TCP. Close the TCP connection on
		// cancellation so the SSH handshake unblocks promptly.
		sshConn, chans, reqs, err := newClientConnContext(ctx, tcpConn, addr, c.config)
		if err != nil {
			tcpConn.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			lastErr = fmt.Errorf("SSH handshake failed: %w", err)
			handshakeAttempts++
			if handshakeAttempts < connectMaxHandshakeAttempts {
				if err := sleepWithContext(ctx, connectBackoff(handshakeAttempts)); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("failed to establish SSH after %d handshake attempt(s): %w", handshakeAttempts, lastErr)
		}
		if err := ctx.Err(); err != nil {
			_ = sshConn.Close()
			return err
		}

		// Create SSH client.
		c.conn = ssh.NewClient(sshConn, chans, reqs)
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempt(s): %w", tcpAttempts, lastErr)
}

func newClientConnContext(ctx context.Context, tcpConn net.Conn, addr string, config *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	type result struct {
		conn  ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		conn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
		resultCh <- result{conn: conn, chans: chans, reqs: reqs, err: err}
	}()

	select {
	case <-ctx.Done():
		_ = tcpConn.Close()
		return nil, nil, nil, ctx.Err()
	case res := <-resultCh:
		return res.conn, res.chans, res.reqs, res.err
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func connectTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("TAKO_SSH_CONNECT_TIMEOUT"))
	if raw == "" {
		return connectDefaultTimeout
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return connectDefaultTimeout
		}
		duration = time.Duration(seconds) * time.Second
	}
	if duration < time.Second {
		return connectDefaultTimeout
	}
	if duration > connectMaxTimeout {
		return connectMaxTimeout
	}
	return duration
}

func connectTCPAttempts() int {
	raw := strings.TrimSpace(os.Getenv("TAKO_SSH_CONNECT_ATTEMPTS"))
	if raw == "" {
		if truthyEnv("CI") || truthyEnv("TAKO_NONINTERACTIVE") {
			return connectAutomationTCPAttempts
		}
		return connectDefaultTCPAttempts
	}
	attempts, err := strconv.Atoi(raw)
	if err != nil || attempts < 1 {
		return connectDefaultTCPAttempts
	}
	if attempts > 20 {
		return 20
	}
	return attempts
}

func connectBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	backoff := time.Duration(1<<uint(attempt-1)) * time.Second
	if backoff > connectBackoffMax {
		return connectBackoffMax
	}
	return backoff
}

func isTransientDialError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"connection reset",
		"i/o timeout",
		"operation timed out",
		"no route to host",
		"network is unreachable",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func truthyEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
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
	ctx, cancel, defaultDeadline := withDefaultCommandDeadline(ctx)
	defer cancel()

	conn, err := c.getConnectionContext(ctx)
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
		_ = session.Signal(ssh.SIGTERM)
		if defaultDeadline && ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("remote command timed out after %s", DefaultCommandTimeout)
		}
		return "", ctx.Err()
	case res := <-resultChan:
		if res.err != nil {
			// Return both the error and the output (which often contains the actual error message)
			return string(res.output), fmt.Errorf("command failed: %w", res.err)
		}
		return string(res.output), nil
	}
}

// ExecuteStream runs a command and streams output in real-time.
func (c *Client) ExecuteStream(cmd string, stdout, stderr io.Writer) error {
	return c.ExecuteStreamWithContext(context.Background(), cmd, stdout, stderr)
}

// ExecuteStreamWithContext runs a command, streams output in real-time, and respects context cancellation.
func (c *Client) ExecuteStreamWithContext(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	ctx, cancel, defaultDeadline := withDefaultCommandDeadline(ctx)
	defer cancel()

	conn, err := c.getConnectionContext(ctx)
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

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}
	if err := waitSessionWithContext(ctx, session); err != nil {
		if defaultDeadline && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("remote command timed out after %s", DefaultCommandTimeout)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}

// ExecuteStreamWithInput runs a command feeding input to stdin while
// streaming output in real-time, respecting context cancellation.
func (c *Client) ExecuteStreamWithInput(ctx context.Context, cmd string, input io.Reader, stdout, stderr io.Writer) error {
	ctx, cancel, defaultDeadline := withDefaultCommandDeadline(ctx)
	defer cancel()

	conn, err := c.getConnectionContext(ctx)
	if err != nil {
		return err
	}

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	session.Stdin = input
	session.Stdout = stdout
	session.Stderr = stderr

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}
	if err := waitSessionWithContext(ctx, session); err != nil {
		if defaultDeadline && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("remote command timed out after %s", DefaultCommandTimeout)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}

// ExecuteWithInput runs a command on the remote server, streams input to stdin,
// and returns combined stdout/stderr output.
func (c *Client) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	ctx, cancel, defaultDeadline := withDefaultCommandDeadline(ctx)
	defer cancel()

	conn, err := c.getConnectionContext(ctx)
	if err != nil {
		return "", err
	}

	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	var output lockedBuffer
	session.Stdout = &output
	session.Stderr = &output

	if err := session.Start(cmd); err != nil {
		stdin.Close()
		return "", fmt.Errorf("failed to start command: %w", err)
	}

	copyErrCh := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stdin, input)
		closeErr := stdin.Close()
		if copyErr != nil {
			copyErrCh <- copyErr
			return
		}
		copyErrCh <- closeErr
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- session.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		if defaultDeadline && ctx.Err() == context.DeadlineExceeded {
			return output.String(), fmt.Errorf("remote command timed out after %s", DefaultCommandTimeout)
		}
		return output.String(), ctx.Err()
	case err := <-waitCh:
		if copyErr := <-copyErrCh; copyErr != nil && err == nil {
			return output.String(), fmt.Errorf("failed to stream command input: %w", copyErr)
		}
		if err != nil {
			return output.String(), fmt.Errorf("command failed: %w", err)
		}
		return output.String(), nil
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func withDefaultCommandDeadline(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}, false
	}
	defaultCtx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
	return defaultCtx, cancel, true
}

func waitSessionWithContext(ctx context.Context, session *ssh.Session) error {
	ctx, cancel, defaultDeadline := withDefaultCommandDeadline(ctx)
	defer cancel()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- session.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		if defaultDeadline && ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("remote command timed out after %s", DefaultCommandTimeout)
		}
		return ctx.Err()
	case err := <-waitCh:
		return err
	}
}

// ExecuteInteractive runs a command interactively with a remote PTY.
// It puts the local terminal in raw mode and forwards stdin/stdout/stderr
// bidirectionally, supporting full interactive sessions (shells, editors, etc.).
func (c *Client) ExecuteInteractive(cmd string) error {
	conn, err := c.getConnection()
	if err != nil {
		return err
	}

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Get local terminal size
	fd := int(os.Stdin.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		// Fallback to reasonable defaults if not a terminal
		w, h = 80, 24
	}

	// Request remote PTY
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", h, w, modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}

	// Put local terminal in raw mode
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set raw terminal: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Wire up stdin/stdout/stderr
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Listen for terminal resize signals (Unix only, no-op on other platforms)
	stopResize := watchTerminalResize(fd, session)
	defer stopResize()

	// Start and wait for the command
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	return session.Wait()
}

// StreamingSession represents a streaming SSH session
type StreamingSession struct {
	session *ssh.Session
	Stdout  io.Reader
	Stderr  io.Reader
}

// Wait waits for the streaming command to finish.
func (s *StreamingSession) Wait() error {
	return s.WaitContext(context.Background())
}

// WaitContext waits for the streaming command to finish or for ctx to expire.
func (s *StreamingSession) WaitContext(ctx context.Context) error {
	if s.session == nil {
		return nil
	}
	return waitSessionWithContext(ctx, s.session)
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
// DialUnixSocket opens a direct-streamlocal@openssh.com channel to a Unix
// socket on the remote host, returning a full-duplex connection to it. This
// is how interactive takod endpoints are reached: real HTTP over the socket,
// no remote curl process. Requires sshd's default
// AllowStreamLocalForwarding=yes.
func (c *Client) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	if path == "" {
		return nil, fmt.Errorf("unix socket path is required")
	}
	conn, err := c.getConnectionContext(ctx)
	if err != nil {
		return nil, err
	}
	socketConn, err := conn.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("failed to dial remote unix socket %s (sshd must allow streamlocal forwarding): %w", path, err)
	}
	return socketConn, nil
}

func (c *Client) getConnection() (*ssh.Client, error) {
	return c.getConnectionContext(context.Background())
}

func (c *Client) getConnectionContext(ctx context.Context) (*ssh.Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		// Release lock during connection to avoid blocking other operations.
		c.mu.Unlock()
		if err := c.ConnectContext(ctx); err != nil {
			c.mu.Lock() // Re-acquire before returning.
			return nil, err
		}
		c.mu.Lock() // Re-acquire after connection.
	}

	// Double-check connection is still valid.
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
	return p.getOrCreateWithAuth(host, port, user, keyPath, password, nil)
}

// GetOrCreateWithAuthPinned is the platform-membership runtime path. It never
// consults mutable known_hosts after enrollment and caches by exact pin.
func (p *Pool) GetOrCreateWithAuthPinned(host string, port int, user string, keyPath string, password string, expected RecordedHostKey) (*Client, error) {
	return p.getOrCreateWithAuth(host, port, user, keyPath, password, &expected)
}

func (p *Pool) getOrCreateWithAuth(host string, port int, user string, keyPath string, password string, expected *RecordedHostKey) (*Client, error) {
	key := fmt.Sprintf("%s@%s:%d", user, host, port)
	if expected != nil {
		key += "#" + expected.Fingerprint
	}

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

	p.mu.Lock()
	if client, exists = p.clients[key]; exists {
		if client.IsHealthy() {
			p.mu.Unlock()
			return client, nil
		}
		client.Close()
		delete(p.clients, key)
	}
	p.mu.Unlock()

	client, err := newPooledClient(host, port, user, keyPath, password, expected)
	if err != nil {
		return nil, err
	}

	if err := client.Connect(); err != nil {
		client.Close()
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, exists := p.clients[key]; exists {
		if existing.IsHealthy() {
			client.Close()
			return existing, nil
		}
		existing.Close()
		delete(p.clients, key)
	}
	p.clients[key] = client

	return client, nil
}

func newPooledClient(host string, port int, user string, keyPath string, password string, expected *RecordedHostKey) (*Client, error) {
	if expected != nil {
		return NewClientFromConfigPinned(ServerConfig{Host: host, Port: port, User: user, SSHKey: keyPath, Password: password}, *expected)
	}
	if password != "" && keyPath == "" {
		return NewClientWithPassword(host, port, user, password)
	}
	if keyPath != "" || password != "" {
		return NewClientWithAuth(host, port, user, keyPath, password)
	}
	return nil, fmt.Errorf("no authentication method provided")
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

// UploadReaderPrivateTemp uploads into a fresh 0700 remote directory created
// by mktemp. The unguessable directory prevents another local account from
// pre-placing a symlink at the destination before a privileged install.
func (c *Client) UploadReaderPrivateTemp(ctx context.Context, reader io.Reader, mode os.FileMode) (string, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	output, err := c.ExecuteWithContext(ctx, "umask 077 && mktemp -d \"${TMPDIR:-/tmp}/tako-upload.XXXXXXXXXX\"")
	if err != nil {
		return "", func() {}, fmt.Errorf("create private remote upload directory: %w", err)
	}
	directory := strings.TrimSpace(output)
	if directory == "" || strings.ContainsAny(directory, "\r\n") || !strings.HasPrefix(directory, "/") {
		return "", func() {}, fmt.Errorf("remote mktemp returned an invalid path")
	}
	remotePath := path.Join(directory, "payload")
	cleanup := func() { _, _ = c.Execute("rm -rf -- " + shellQuote(directory)) }
	if err := c.UploadReader(reader, remotePath, mode); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return remotePath, cleanup, nil
}

// CopyFileWithMode uploads a file to the remote server with specific permissions
func (c *Client) CopyFileWithMode(localPath, remotePath string, mode os.FileMode) error {
	// Read file content
	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("failed to read local file: %w", err)
	}

	cmd := buildRemoteUploadCommand(remotePath, mode)
	encodedReader := newBase64Reader(content)
	if _, err := c.ExecuteWithInput(context.Background(), cmd, encodedReader); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	return nil
}

func newBase64Reader(content []byte) io.Reader {
	reader, writer := io.Pipe()
	go func() {
		encoder := base64.NewEncoder(base64.StdEncoding, writer)
		_, writeErr := encoder.Write(content)
		closeErr := encoder.Close()
		if writeErr != nil {
			_ = writer.CloseWithError(writeErr)
			return
		}
		_ = writer.CloseWithError(closeErr)
	}()
	return reader
}

func buildRemoteUploadCommand(remotePath string, mode os.FileMode) string {
	quotedPath := shellQuote(remotePath)
	return fmt.Sprintf("base64 -d > %s && chmod %o %s", quotedPath, mode, quotedPath)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
