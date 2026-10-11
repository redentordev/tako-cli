package ssh

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultProbeAttempts and DefaultProbeTimeout bound a reachability probe
	// so a blocked port fails in seconds rather than riding the full
	// rate-limit-aware retry budget of a normal connection.
	DefaultProbeAttempts = 2
	DefaultProbeTimeout  = 10 * time.Second
)

// ProbeOptions bounds a ProbeAuth dial. Zero values use the probe defaults;
// the TAKO_SSH_CONNECT_* environment overrides are never consulted.
type ProbeOptions struct {
	// Attempts is the maximum number of TCP dials (and SSH handshakes).
	Attempts int
	// Timeout is the per-dial TCP and handshake timeout.
	Timeout time.Duration
}

func (o ProbeOptions) normalized() ProbeOptions {
	if o.Attempts < 1 {
		o.Attempts = DefaultProbeAttempts
	}
	if o.Timeout <= 0 || o.Timeout > DefaultProbeTimeout {
		o.Timeout = DefaultProbeTimeout
	}
	return o
}

// ProbeAuth opens a fresh, non-pooled SSH connection to host:port with the
// given credentials, then closes it. It proves that a new connection can be
// established (for example after a firewall change) without reusing a live
// session. An empty password restricts authentication to the key file and
// the SSH agent. When pinned is non-nil the host key must match it exactly.
func ProbeAuth(host string, port int, user string, keyPath string, password string, opts ProbeOptions, pinned *RecordedHostKey) error {
	opts = opts.normalized()

	var (
		client *Client
		err    error
	)
	switch {
	case pinned != nil:
		client, err = NewClientFromConfigPinned(ServerConfig{Host: host, Port: port, User: user, SSHKey: keyPath, Password: password}, *pinned)
	case password != "" && keyPath == "":
		client, err = NewClientWithPassword(host, port, user, password)
	default:
		client, err = NewClientWithAuth(host, port, user, keyPath, password)
	}
	if err != nil {
		return err
	}
	defer client.Close()

	client.applyProbeOptions(opts)

	// Overall deadline: every attempt may use its full timeout plus the
	// short first-retry backoff.
	deadline := time.Duration(opts.Attempts)*opts.Timeout + time.Duration(opts.Attempts)*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	if err := client.ConnectContext(ctx); err != nil {
		return fmt.Errorf("SSH probe to %s:%d failed: %w", host, port, err)
	}
	return nil
}

func (c *Client) applyProbeOptions(opts ProbeOptions) {
	c.dialAttempts = opts.Attempts
	c.dialTimeout = opts.Timeout
	c.dialHandshakeAttempts = opts.Attempts
	if c.config != nil {
		cfg := *c.config
		cfg.Timeout = opts.Timeout
		c.config = &cfg
	}
}

func (c *Client) tcpAttempts() int {
	if c.dialAttempts > 0 {
		return c.dialAttempts
	}
	return connectTCPAttempts()
}

func (c *Client) effectiveConnectTimeout() time.Duration {
	if c.dialTimeout > 0 {
		return c.dialTimeout
	}
	return connectTimeout()
}

func (c *Client) handshakeAttempts() int {
	if c.dialHandshakeAttempts > 0 {
		return c.dialHandshakeAttempts
	}
	return connectMaxHandshakeAttempts
}
