//go:build linux

package hostmetrics

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Statfs returns usage for the filesystem holding path. The ID combines the
// statfs filesystem ID and the device number so bind mounts and the same
// filesystem reached through two paths deduplicate.
func Statfs(path string) (FilesystemUsage, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return FilesystemUsage{}, err
	}
	var st unix.Stat_t
	device := uint64(0)
	if err := unix.Stat(path, &st); err == nil {
		device = uint64(st.Dev)
	}
	blockSize := uint64(stat.Bsize)
	if stat.Frsize > 0 {
		blockSize = uint64(stat.Frsize)
	}
	return FilesystemUsage{
		Path:          path,
		ID:            fmt.Sprintf("fsid:%d:%d/dev:%d", stat.Fsid.Val[0], stat.Fsid.Val[1], device),
		TotalBytes:    stat.Blocks * blockSize,
		FreeBytes:     stat.Bfree * blockSize,
		AvailBytes:    stat.Bavail * blockSize,
		Inodes:        stat.Files,
		InodesFree:    stat.Ffree,
		HasInodeCount: stat.Files > 0,
	}, nil
}
