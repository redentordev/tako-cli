package cmd

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestParseProxyPortSpec(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want proxyPortSpec
	}{
		{name: "remote only", raw: "5432", want: proxyPortSpec{RemotePort: 5432}},
		{name: "local and remote", raw: "15432:5432", want: proxyPortSpec{LocalPort: 15432, RemotePort: 5432}},
		{name: "random local port", raw: "0:3000", want: proxyPortSpec{LocalPort: 0, RemotePort: 3000}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxyPortSpec(tt.raw)
			if err != nil {
				t.Fatalf("parseProxyPortSpec returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseProxyPortSpec() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseProxyPortSpecRejectsInvalidPorts(t *testing.T) {
	for _, raw := range []string{"", "abc", "5432:abc", "1:2:3", "70000", "1000:0"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseProxyPortSpec(raw); err == nil {
				t.Fatal("expected invalid port spec to be rejected")
			}
		})
	}
}

func TestValidateProxyTargetResponseAcceptsMatchingPrivateAddress(t *testing.T) {
	response, err := validateProxyTargetResponse(validProxyTargetResponse(), "demo", "production", "web", 3000)
	if err != nil {
		t.Fatalf("validateProxyTargetResponse returned error: %v", err)
	}
	if response.Address != "172.20.0.20:3000" {
		t.Fatalf("address = %q, want 172.20.0.20:3000", response.Address)
	}
}

func TestValidateProxyTargetResponseRejectsMismatchedService(t *testing.T) {
	response := validProxyTargetResponse()
	response.Service = "db"

	if _, err := validateProxyTargetResponse(response, "demo", "production", "web", 3000); err == nil {
		t.Fatal("expected service mismatch to be rejected")
	}
}

func TestValidateProxyTargetResponseRejectsWrongPort(t *testing.T) {
	response := validProxyTargetResponse()
	response.Port = 5432
	response.Address = "172.20.0.20:5432"

	if _, err := validateProxyTargetResponse(response, "demo", "production", "web", 3000); err == nil {
		t.Fatal("expected port mismatch to be rejected")
	}
}

func TestValidateProxyTargetResponseRejectsNonPrivateHost(t *testing.T) {
	response := validProxyTargetResponse()
	response.Host = "203.0.113.10"
	response.Address = "203.0.113.10:3000"

	if _, err := validateProxyTargetResponse(response, "demo", "production", "web", 3000); err == nil {
		t.Fatal("expected public host to be rejected")
	}
}

func TestValidateProxyTargetResponseRejectsAddressMismatch(t *testing.T) {
	response := validProxyTargetResponse()
	response.Address = "172.20.0.21:3000"

	if _, err := validateProxyTargetResponse(response, "demo", "production", "web", 3000); err == nil {
		t.Fatal("expected address mismatch to be rejected")
	}
}

func TestProxyLocalConnectionCopiesBothDirections(t *testing.T) {
	localClient, localServer := net.Pipe()
	defer localClient.Close()

	dialer := &recordingProxyDialer{remoteConn: make(chan net.Conn, 1)}
	done := make(chan struct{})
	go func() {
		proxyLocalConnection(dialer, "172.20.0.20:3000", localServer)
		close(done)
	}()

	remote := <-dialer.remoteConn
	defer remote.Close()

	if dialer.network != "tcp" || dialer.address != "172.20.0.20:3000" {
		t.Fatalf("dialed %s %s, want tcp 172.20.0.20:3000", dialer.network, dialer.address)
	}

	remoteDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(remote, buf); err != nil {
			remoteDone <- err
			return
		}
		if string(buf) != "ping" {
			remoteDone <- fmt.Errorf("remote read %q, want ping", string(buf))
			return
		}
		_, err := remote.Write([]byte("pong"))
		remoteDone <- err
	}()

	if _, err := localClient.Write([]byte("ping")); err != nil {
		t.Fatalf("write to local proxy side: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(localClient, buf); err != nil {
		t.Fatalf("read from local proxy side: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("local read %q, want pong", string(buf))
	}
	_ = localClient.Close()

	select {
	case err := <-remoteDone:
		if err != nil {
			t.Fatalf("remote side failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for remote side")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy connection to close")
	}
}

func validProxyTargetResponse() takod.ProxyTargetResponse {
	return takod.ProxyTargetResponse{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Container:   "web-1",
		ContainerID: "container-1",
		Host:        "172.20.0.20",
		Port:        3000,
		Address:     "172.20.0.20:3000",
	}
}

type recordingProxyDialer struct {
	network    string
	address    string
	remoteConn chan net.Conn
}

func (d *recordingProxyDialer) Dial(network string, address string) (net.Conn, error) {
	d.network = network
	d.address = address
	local, remote := net.Pipe()
	d.remoteConn <- remote
	return local, nil
}
