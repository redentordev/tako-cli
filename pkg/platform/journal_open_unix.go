//go:build linux || darwin

package platform

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openJournalAppend(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_APPEND|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0640)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("journal path must be a regular file")
	}
	if err := file.Chmod(0640); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
