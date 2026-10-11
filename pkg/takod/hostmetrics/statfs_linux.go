//go:build linux

package hostmetrics

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Statfs returns usage for the filesystem holding path. ID is the statfs
// filesystem ID and Device the st_dev number; a match on either means the
// same filesystem (FilesystemUsage.SameFilesystem). On btrfs every
// subvolume has its own st_dev and f_fsid, so Device is the mount source
// (the pool's block device) instead.
func Statfs(path string) (FilesystemUsage, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return FilesystemUsage{}, err
	}
	usage := FilesystemUsage{Path: path}
	if stat.Fsid.Val[0] != 0 || stat.Fsid.Val[1] != 0 {
		usage.ID = fmt.Sprintf("fsid:%d:%d", stat.Fsid.Val[0], stat.Fsid.Val[1])
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err == nil && st.Dev != 0 {
		usage.Device = fmt.Sprintf("dev:%d", uint64(st.Dev))
	}
	if int64(stat.Type) == unix.BTRFS_SUPER_MAGIC {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		if mountinfo, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
			if source := mountSourceFor(mountinfo, resolved); source != "" {
				usage.Device = "btrfs:" + source
			}
		}
	}
	blockSize := uint64(stat.Bsize)
	if stat.Frsize > 0 {
		blockSize = uint64(stat.Frsize)
	}
	usage.TotalBytes = stat.Blocks * blockSize
	usage.FreeBytes = stat.Bfree * blockSize
	usage.AvailBytes = stat.Bavail * blockSize
	usage.Inodes = stat.Files
	usage.InodesFree = stat.Ffree
	usage.HasInodeCount = stat.Files > 0
	return usage, nil
}
