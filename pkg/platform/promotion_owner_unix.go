//go:build !windows

package platform

import (
	"fmt"
	"os"
	"syscall"
)

func validatePassivePromotionAncestorOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return fmt.Errorf("ancestor must be owned by root or uid %d", os.Geteuid())
	}
	return nil
}
