//go:build linux

package platform

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func stagePlatformBinary(source string, destination string) error {
	if destination != DefaultBinaryPath {
		return fmt.Errorf("platform service binary destination must be %s", DefaultBinaryPath)
	}
	sourceFD, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open source Tako binary: %w", err)
	}
	sourceFile := os.NewFile(uintptr(sourceFD), source)
	defer sourceFile.Close()
	sourceInfo, err := sourceFile.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Mode().Perm()&0111 == 0 || sourceInfo.Size() == 0 {
		return fmt.Errorf("source Tako binary must be a regular file")
	}
	sourceStat, ok := sourceInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect source Tako binary")
	}
	if err := ensureProtectedBinaryDirectory(filepath.Dir(destination)); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".tako-stage-*")
	if err != nil {
		return fmt.Errorf("create staged Tako binary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, sourceFile); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy staged Tako binary: %w", err)
	}
	afterInfo, err := sourceFile.Stat()
	if err != nil {
		_ = temporary.Close()
		return err
	}
	afterStat, ok := afterInfo.Sys().(*syscall.Stat_t)
	if !ok || sourceStat.Ino != afterStat.Ino || sourceStat.Size != afterStat.Size || sourceStat.Mtim != afterStat.Mtim || sourceStat.Ctim != afterStat.Ctim {
		_ = temporary.Close()
		return fmt.Errorf("source Tako binary changed while it was staged")
	}
	if err := temporary.Chmod(0755); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chown(0, 0); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish staged Tako binary: %w", err)
	}
	destinationInfo, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if err := validatePublishedBinary(destinationInfo, 0, 0); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func validatePublishedBinary(info os.FileInfo, expectedUID uint32, expectedGID uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != expectedUID || stat.Gid != expectedGID || info.Mode().Perm() != 0755 {
		return fmt.Errorf("staged Tako binary is not a protected root-owned executable")
	}
	return nil
}

func ensureProtectedBinaryDirectory(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	for current := path; current != "/"; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("platform binary ancestor %s must be a root-owned, non-writable directory", current)
		}
	}
	return nil
}

func syncDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fsync(fd)
}
