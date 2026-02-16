//go:build windows

package ssh

import "golang.org/x/crypto/ssh"

// watchTerminalResize is a no-op on Windows since SIGWINCH is not available.
func watchTerminalResize(fd int, session *ssh.Session) func() {
	return func() {}
}
