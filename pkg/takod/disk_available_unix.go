//go:build linux || darwin

package takod

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func availableDiskBytes(path string) (int64, error) {
	path = filepath.Clean(path)
	for {
		var stat unix.Statfs_t
		if err := unix.Statfs(path, &stat); err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), nil
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
