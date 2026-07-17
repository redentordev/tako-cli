//go:build !windows

package platform

import (
	"fmt"
	"os"
	"syscall"
)

func validateMembershipDirectoryOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("membership directory must be owned by uid %d", os.Geteuid())
	}
	return nil
}

func validateMembershipFileOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fmt.Errorf("membership state must be singly linked and owned by uid %d", os.Geteuid())
	}
	return nil
}
