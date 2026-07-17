package takodclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
)

// upgradeHandshakeTimeout bounds the HTTP request/response exchange before
// the connection becomes a raw frame stream (which has no deadline: an
// interactive session lives until it exits).
const upgradeHandshakeTimeout = 2 * time.Minute

// UnixSocketDialer opens a full-duplex connection to a Unix socket on the
// takod node, typically through an SSH direct-streamlocal channel.
type UnixSocketDialer interface {
	DialUnixSocket(ctx context.Context, path string) (net.Conn, error)
}

// UpgradedStream is a hijacked takod connection speaking the ptystream frame
// protocol. Reader buffers bytes that arrived with the handshake response;
// always read frames from Reader, never from Conn directly.
type UpgradedStream struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

// Close closes the underlying connection.
func (s *UpgradedStream) Close() error {
	return s.Conn.Close()
}

// UpgradeStream dials the takod socket, POSTs value to endpoint with an
// Upgrade: tako-pty/1 handshake, and returns the connection as a raw frame
// stream once the server switches protocols. A non-101 response is returned
// as an error carrying the response body.
func UpgradeStream(ctx context.Context, dialer UnixSocketDialer, socket string, endpoint string, value any) (*UpgradedStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return nil, fmt.Errorf("takod endpoint must start with /")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("failed to encode upgrade request: %w", err)
	}

	conn, err := dialer.DialUnixSocket(ctx, socket)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	// SSH streamlocal channels do not support SetDeadline, so the handshake
	// timeout (and ctx cancellation) closes the connection instead, which
	// unblocks the request write / response read below. The stop deferral
	// disarms the guard once the handshake completes.
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, upgradeHandshakeTimeout)
	defer cancelHandshake()
	stopGuard := context.AfterFunc(handshakeCtx, func() { _ = conn.Close() })
	defer stopGuard()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://takod"+endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", ptystream.Protocol)
	if err := attachOperationFenceHeader(request); err != nil {
		return nil, err
	}
	if err := request.Write(conn); err != nil {
		return nil, fmt.Errorf("takod upgrade request %s failed: %w", endpoint, err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("takod upgrade response for %s failed: %w", endpoint, err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		_ = response.Body.Close()
		return nil, fmt.Errorf("takod request POST %s returned HTTP %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if got := response.Header.Get("Upgrade"); got != ptystream.Protocol {
		return nil, fmt.Errorf("takod upgraded to unexpected protocol %q, want %q", got, ptystream.Protocol)
	}

	ok = true
	return &UpgradedStream{Conn: conn, Reader: reader}, nil
}
