//go:build linux || darwin

package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func ensureOwnedDurableFile(path string, uid int, gid int) error {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_APPEND|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0640)
	if err != nil {
		return fmt.Errorf("initialize durable platform file %s: %w", path, err)
	}
	closeOnError := func(err error) error {
		_ = unix.Close(fd)
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return closeOnError(fmt.Errorf("inspect durable platform file %s: %w", path, err))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return closeOnError(fmt.Errorf("durable platform file %s must be a singly-linked regular file", path))
	}
	if err := unix.Fchmod(fd, 0640); err != nil {
		return closeOnError(fmt.Errorf("secure platform durable file %s: %w", path, err))
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return closeOnError(fmt.Errorf("own platform durable file %s: %w", path, err))
	}
	if err := unix.Fsync(fd); err != nil {
		return closeOnError(fmt.Errorf("sync platform durable file %s: %w", path, err))
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close platform durable file %s: %w", path, err)
	}
	return nil
}
