//go:build darwin

package takodclient

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyUnixPeerUID(connection net.Conn, expected uint32) error {
	syscallConn, ok := connection.(syscall.Conn)
	if !ok {
		return fmt.Errorf("connection does not expose peer credentials")
	}
	raw, err := syscallConn.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credentials == nil || credentials.Uid != expected {
		actual := uint32(^uint32(0))
		if credentials != nil {
			actual = credentials.Uid
		}
		return fmt.Errorf("peer uid %d does not match expected uid %d", actual, expected)
	}
	return nil
}
