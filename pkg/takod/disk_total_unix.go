//go:build linux || darwin

package takod

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// totalDiskBytes returns the size of the filesystem holding path (or its
// nearest existing parent).
func totalDiskBytes(path string) (int64, error) {
	path = filepath.Clean(path)
	for {
		var stat unix.Statfs_t
		if err := unix.Statfs(path, &stat); err == nil {
			return int64(stat.Blocks) * int64(stat.Bsize), nil
		} else if err != unix.ENOENT {
			return 0, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, unix.ENOENT
		}
		path = parent
	}
}
