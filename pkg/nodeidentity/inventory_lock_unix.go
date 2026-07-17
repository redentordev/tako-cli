//go:build !windows

package nodeidentity

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// AcquireInventoryMutationLock serializes protected inventory publication
// with consumers that must validate and act on one exact inventory inode.
func AcquireInventoryMutationLock(path string) (func(), error) {
	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	if lock == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open protected inventory lock")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0077 != 0 || !publishedOwnerAllowed(path, stat.Uid, uint32(os.Geteuid())) {
		_ = lock.Close()
		return nil, fmt.Errorf("protected inventory lock must be a private singly-linked regular file with trusted ownership")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}, nil
}
