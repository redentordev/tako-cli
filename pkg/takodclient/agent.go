package takodclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	defaultAgentResponseLimit int64 = 64 << 20
	agentErrorBodyLimit       int64 = 1 << 20
	agentStatusBodyLimit      int64 = 64 << 10
	agentMaxResponseHeaders         = 64 << 10
	agentStatusTimeout              = 15 * time.Second
	maxConcurrentStatusProbes       = 4
)

// AgentStatus contains the installation-level fields used to authenticate a
// local transport decision. Mutable project/environment metadata is
// intentionally not part of this trust decision.
type AgentStatus struct {
	Runtime         string                 `json:"runtime"`
	Version         string                 `json:"version"`
	Capabilities    []string               `json:"capabilities,omitempty"`
	Hostname        string                 `json:"hostname"`
	Identity        *nodeidentity.Identity `json:"identity,omitempty"`
	EnrollmentRoles []string               `json:"enrollmentRoles,omitempty"`
}

// AgentClient sends structured HTTP directly to a takod Unix socket. Dialer
// may open a local Unix socket or an SSH direct-streamlocal channel; callers
// do not compose shell or curl commands.
type AgentClient struct {
	socket      string
	dialer      UnixSocketDialer
	transport   *http.Transport
	httpClient  *http.Client
	statusSlots chan struct{}
}

// NewAgentClient constructs a reusable structured takod client.
func NewAgentClient(dialer UnixSocketDialer, socket string) (*AgentClient, error) {
	if dialer == nil {
		return nil, fmt.Errorf("takod Unix socket dialer is required")
	}
	if strings.TrimSpace(socket) == "" {
		socket = DefaultSocket
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: agentMaxResponseHeaders,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialUnixSocket(ctx, socket)
		},
	}
	return &AgentClient{
		socket:    socket,
		dialer:    dialer,
		transport: transport,
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		statusSlots: make(chan struct{}, maxConcurrentStatusProbes),
	}, nil
}

// LocalUnixSocketDialer opens a Unix socket in the current network namespace.
type LocalUnixSocketDialer struct {
	// ExpectedUID is the operating-system UID that must own the connected
	// takod process. The zero value intentionally requires root.
	ExpectedUID uint32
}

func (d LocalUnixSocketDialer) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("takod Unix socket path is required")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial local takod Unix socket %s: %w", path, err)
	}
	if err := verifyUnixPeerUID(connection, d.ExpectedUID); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("authenticate local takod Unix socket %s: %w", path, err)
	}
	return connection, nil
}

// NewLocalAgentClient constructs a direct, shell-free local takod client.
func NewLocalAgentClient(socket string) (*AgentClient, error) {
	return NewAgentClient(LocalUnixSocketDialer{}, socket)
}

// CloseIdleConnections releases reusable local or SSH streamlocal channels.
func (c *AgentClient) CloseIdleConnections() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// RequestJSON performs a structured takod request and returns its response
// body. It preserves the existing HTTPError taxonomy without shell status
// markers.
func (c *AgentClient) RequestJSON(ctx context.Context, method string, endpoint string, value any) (string, error) {
	return c.RequestJSONWithTimeout(ctx, method, endpoint, value, JSONRequestTimeout)
}

// RequestJSONWithTimeout is RequestJSON with an explicit request deadline.
func (c *AgentClient) RequestJSONWithTimeout(ctx context.Context, method string, endpoint string, value any, timeout time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if value != nil {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encode takod request: %w", err)
		}
		data = append(data, '\n')
		body = bytes.NewReader(data)
	}
	response, err := c.do(ctx, method, endpoint, body, "application/json")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	limit := defaultAgentResponseLimit
	if !isHTTPSuccess(response.StatusCode) {
		limit = agentErrorBodyLimit
	}
	data, err := readLimitedBody(response.Body, limit)
	if err != nil {
		return "", fmt.Errorf("read takod response %s %s: %w", normalizedMethod(method), endpoint, err)
	}
	output := sanitizeJSONOutput(string(data))
	if !isHTTPSuccess(response.StatusCode) {
		return output, &HTTPError{Method: normalizedMethod(method), Endpoint: endpoint, Status: response.StatusCode, Body: output}
	}
	return output, nil
}

// StreamRequest sends input without buffering and returns a bounded response.
func (c *AgentClient) StreamRequest(ctx context.Context, method string, endpoint string, input io.Reader, contentType string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, StreamRequestTimeout)
	defer cancel()
	response, err := c.do(ctx, method, endpoint, input, contentType)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	limit := defaultAgentResponseLimit
	if !isHTTPSuccess(response.StatusCode) {
		limit = agentErrorBodyLimit
	}
	data, err := readLimitedBody(response.Body, limit)
	if err != nil {
		return "", fmt.Errorf("read takod stream response %s %s: %w", normalizedMethod(method), endpoint, err)
	}
	output := sanitizeJSONOutput(string(data))
	if !isHTTPSuccess(response.StatusCode) {
		return output, &HTTPError{Method: normalizedMethod(method), Endpoint: endpoint, Status: response.StatusCode, Body: output}
	}
	return output, nil
}

// StreamOutput performs a request and copies its response live to output. A
// non-success response is copied to errorOutput and returned as HTTPError.
func (c *AgentClient) StreamOutput(ctx context.Context, method string, endpoint string, input io.Reader, contentType string, output io.Writer, errorOutput io.Writer) error {
	response, err := c.do(ctx, method, endpoint, input, contentType)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if !isHTTPSuccess(response.StatusCode) {
		data, readErr := readLimitedBody(response.Body, agentErrorBodyLimit)
		if readErr != nil {
			return fmt.Errorf("read takod error response %s %s: %w", normalizedMethod(method), endpoint, readErr)
		}
		body := strings.TrimSpace(string(data))
		if errorOutput != nil && body != "" {
			_, _ = io.WriteString(errorOutput, body+"\n")
		}
		return &HTTPError{Method: normalizedMethod(method), Endpoint: endpoint, Status: response.StatusCode, Body: body}
	}
	if output == nil {
		output = io.Discard
	}
	if _, err := io.Copy(output, response.Body); err != nil {
		return fmt.Errorf("stream takod response %s %s: %w", normalizedMethod(method), endpoint, err)
	}
	return nil
}

// Status returns and validates the immutable installation identity exposed by
// takod. A malformed identity cannot authorize a local transport decision.
func (c *AgentClient) Status(ctx context.Context) (*AgentStatus, error) {
	if c == nil || c.statusSlots == nil {
		return nil, fmt.Errorf("takod agent client is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, agentStatusTimeout)
	defer cancel()
	select {
	case c.statusSlots <- struct{}{}:
		defer func() { <-c.statusSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("takod status probe unavailable: %w", ctx.Err())
	}
	response, err := c.do(ctx, http.MethodGet, "/v1/status", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := readLimitedBody(response.Body, agentStatusBodyLimit)
	if err != nil {
		return nil, fmt.Errorf("read takod status: %w", err)
	}
	output := sanitizeJSONOutput(string(data))
	if !isHTTPSuccess(response.StatusCode) {
		return nil, &HTTPError{Method: http.MethodGet, Endpoint: "/v1/status", Status: response.StatusCode, Body: output}
	}
	var envelope struct {
		Runtime         string          `json:"runtime"`
		Version         string          `json:"version"`
		Capabilities    []string        `json:"capabilities"`
		Hostname        string          `json:"hostname"`
		Identity        json.RawMessage `json:"identity"`
		EnrollmentRoles []string        `json:"enrollmentRoles"`
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode takod status: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode takod status: multiple JSON values are not allowed")
		}
		return nil, fmt.Errorf("decode takod status: %w", err)
	}
	status := AgentStatus{
		Runtime:         envelope.Runtime,
		Version:         envelope.Version,
		Capabilities:    envelope.Capabilities,
		Hostname:        envelope.Hostname,
		EnrollmentRoles: envelope.EnrollmentRoles,
	}
	if len(envelope.Identity) > 0 && string(envelope.Identity) != "null" {
		identityDecoder := json.NewDecoder(bytes.NewReader(envelope.Identity))
		identityDecoder.DisallowUnknownFields()
		var identity nodeidentity.Identity
		if err := identityDecoder.Decode(&identity); err != nil {
			return nil, fmt.Errorf("decode takod installation identity: %w", err)
		}
		var identityExtra any
		if err := identityDecoder.Decode(&identityExtra); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("decode takod installation identity: multiple JSON values are not allowed")
			}
			return nil, fmt.Errorf("decode takod installation identity: %w", err)
		}
		status.Identity = &identity
	}
	if err := status.Validate(); err != nil {
		return nil, err
	}
	return &status, nil
}

// Validate defensively checks a status value before it is used as identity
// attestation. Callers must not rely on a particular IdentityProbe having
// obtained the value through AgentClient.Status.
func (s AgentStatus) Validate() error {
	if s.Runtime != "takod" {
		return fmt.Errorf("unexpected runtime %q from takod status", s.Runtime)
	}
	if s.Identity == nil {
		return nil
	}
	if !s.hasCapability(nodeidentity.Capability) {
		return fmt.Errorf("takod status returned installation identity without %s capability", nodeidentity.Capability)
	}
	if err := s.Identity.Validate(); err != nil {
		return fmt.Errorf("invalid installation identity from takod status: %w", err)
	}
	return nil
}

func (s AgentStatus) hasCapability(required string) bool {
	for _, capability := range s.Capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

// UpgradeStream opens a structured upgraded takod connection using the same
// dialer and socket as normal requests.
func (c *AgentClient) UpgradeStream(ctx context.Context, endpoint string, value any) (*UpgradedStream, error) {
	if c == nil || c.dialer == nil {
		return nil, fmt.Errorf("takod agent client is not initialized")
	}
	return UpgradeStream(ctx, c.dialer, c.socket, endpoint, value)
}

func (c *AgentClient) do(ctx context.Context, method string, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("takod agent client is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") {
		return nil, fmt.Errorf("takod endpoint must start with exactly one slash")
	}
	method = normalizedMethod(method)
	request, err := http.NewRequestWithContext(ctx, method, "http://takod"+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create takod request %s %s: %w", method, endpoint, err)
	}
	if body != nil && strings.TrimSpace(contentType) != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("takod request %s %s failed: %w", method, endpoint, err)
	}
	return response, nil
}

func normalizedMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return http.MethodGet
	}
	return method
}

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func isHTTPSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}
