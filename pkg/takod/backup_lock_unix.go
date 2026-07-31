//go:build !windows

package takod

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireBackupDirectoryLock(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, ".backup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open backup publication lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %v", errBackupDirectoryLocked, err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
