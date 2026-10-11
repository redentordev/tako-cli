package ssh

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProbeOptionsNormalizeDefaultsAndCap(t *testing.T) {
	got := ProbeOptions{}.normalized()
	if got.Attempts != DefaultProbeAttempts || got.Timeout != DefaultProbeTimeout {
		t.Fatalf("zero options normalized to %+v", got)
	}
	got = ProbeOptions{Attempts: 1, Timeout: time.Minute}.normalized()
	if got.Attempts != 1 || got.Timeout != DefaultProbeTimeout {
		t.Fatalf("oversized timeout normalized to %+v, want capped at %s", got, DefaultProbeTimeout)
	}
}

func TestProbeOptionsOverrideConnectEnvironment(t *testing.T) {
	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "15")
	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "120s")
	t.Setenv("CI", "true")

	client := &Client{config: &ssh.ClientConfig{Timeout: 2 * time.Minute}}
	client.applyProbeOptions(ProbeOptions{Attempts: 2, Timeout: 3 * time.Second})

	if got := client.tcpAttempts(); got != 2 {
		t.Fatalf("tcpAttempts = %d, want 2 (env must be ignored)", got)
	}
	if got := client.effectiveConnectTimeout(); got != 3*time.Second {
		t.Fatalf("connect timeout = %s, want 3s (env must be ignored)", got)
	}
	if got := client.handshakeAttempts(); got != 2 {
		t.Fatalf("handshakeAttempts = %d, want 2", got)
	}
	if client.config.Timeout != 3*time.Second {
		t.Fatalf("ssh config timeout = %s, want 3s", client.config.Timeout)
	}

	plain := &Client{}
	if got := plain.tcpAttempts(); got != 15 {
		t.Fatalf("non-probe tcpAttempts = %d, want env value 15", got)
	}
}

func TestProbeBoundedDialHonorsAttempts(t *testing.T) {
	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "10")

	// A listener that drops every connection makes each SSH handshake fail,
	// which the client retries until its handshake budget is spent.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan struct{}, 32)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	client := &Client{
		host:   "127.0.0.1",
		port:   port,
		config: &ssh.ClientConfig{User: "probe", HostKeyCallback: ssh.InsecureIgnoreHostKey()},
	}
	client.applyProbeOptions(ProbeOptions{Attempts: 2, Timeout: 2 * time.Second})

	start := time.Now()
	err = client.ConnectContext(context.Background())
	if err == nil {
		client.Close()
		t.Fatal("expected probe to a closed port to fail")
	}
	if !strings.Contains(err.Error(), "after 2 handshake attempt(s)") {
		t.Fatalf("error = %v, want exactly 2 attempts", err)
	}
	if got := len(accepted); got != 2 {
		t.Fatalf("server accepted %d connections, want 2", got)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("bounded probe took %s", elapsed)
	}
}
