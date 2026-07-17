//go:build linux || darwin

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockJournalFile(file *os.File) (func(), error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
}
