//go:build !windows

package nodeidentity

import (
	"fmt"
	"os"
	"syscall"
)

func validateOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine file owner")
	}
	want := uint32(os.Geteuid())
	if stat.Uid != want {
		return fmt.Errorf("must be owned by uid %d, got uid %d", want, stat.Uid)
	}
	return nil
}
