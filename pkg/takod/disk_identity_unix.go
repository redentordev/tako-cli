//go:build linux || darwin

package takod

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func diskFilesystemIdentity(path string) (string, error) {
	path = filepath.Clean(path)
	for {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err == nil {
			return fmt.Sprintf("dev:%d", stat.Dev), nil
		} else if err != unix.ENOENT {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", unix.ENOENT
		}
		path = parent
	}
}
