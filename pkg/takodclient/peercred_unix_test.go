//go:build linux || darwin

package takodclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalUnixSocketDialerAuthenticatesPeerUID(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tako-peercred-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "takod.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{}, 2)
	go func() {
		for index := 0; index < 2; index++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.Close()
			accepted <- struct{}{}
		}
	}()

	dialer := LocalUnixSocketDialer{ExpectedUID: uint32(os.Geteuid())}
	connection, err := dialer.DialUnixSocket(context.Background(), path)
	if err != nil {
		t.Fatalf("matching peer UID was rejected: %v", err)
	}
	_ = connection.Close()
	<-accepted

	wrongUID := uint32(os.Geteuid()) + 1
	if _, err := (LocalUnixSocketDialer{ExpectedUID: wrongUID}).DialUnixSocket(context.Background(), path); err == nil || !strings.Contains(err.Error(), "does not match expected uid") {
		t.Fatalf("wrong peer UID error = %v", err)
	}
	<-accepted
}
