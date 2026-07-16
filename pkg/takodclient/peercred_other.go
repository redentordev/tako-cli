//go:build !linux && !darwin

package takodclient

import (
	"fmt"
	"net"
)

func verifyUnixPeerUID(_ net.Conn, _ uint32) error {
	return fmt.Errorf("local takod peer credential verification is unsupported on this platform")
}
