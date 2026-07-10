//go:build !windows

package takod

import (
	"os"
	"syscall"
)

func serviceFileOwnershipMatches(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && int(stat.Gid) == gid
}
