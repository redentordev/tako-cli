//go:build !windows

package projectbinding

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var secureBindingTestHook func(string)
var secureBindingUnlinkat = unix.Unlinkat
var secureBindingFsync = unix.Fsync
var secureBindingWinnerRead = readBindingAt

func readSecureBindingFileOptional(path string) ([]byte, error) {
	dirFD, dirStat, err := openSecureBindingDirectory(filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	if secureBindingTestHook != nil {
		secureBindingTestHook("read-directory-open")
	}
	data, err := readBindingAt(dirFD, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if err := verifyBindingDirectoryPath(filepath.Dir(path), dirStat); err != nil {
		return nil, err
	}
	return data, nil
}

func createSecureBindingFile(path string, data []byte) ([]byte, error) {
	dir := filepath.Dir(path)
	dirFD, dirStat, err := openSecureBindingDirectory(dir, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)

	tmpName, err := secureBindingTempName()
	if err != nil {
		return nil, err
	}
	tmpFD, err := unix.Openat(dirFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("create project cluster binding temporary file: %w", err)
	}
	tmpFile := os.NewFile(uintptr(tmpFD), tmpName)
	tmpOpen := true
	defer func() {
		if tmpOpen {
			_ = tmpFile.Close()
		}
		_ = secureBindingUnlinkat(dirFD, tmpName, 0)
	}()
	if _, err := tmpFile.Write(data); err != nil {
		return nil, fmt.Errorf("write project cluster binding temporary file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync project cluster binding temporary file: %w", err)
	}
	var stagedStat unix.Stat_t
	if err := unix.Fstat(tmpFD, &stagedStat); err != nil {
		return nil, fmt.Errorf("inspect staged project cluster binding: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close project cluster binding temporary file: %w", err)
	}
	tmpOpen = false
	if secureBindingTestHook != nil {
		secureBindingTestHook("create-temporary-synced")
	}
	if err := verifyBindingDirectoryPath(dir, dirStat); err != nil {
		return nil, err
	}

	base := filepath.Base(path)
	linkErr := unix.Linkat(dirFD, tmpName, dirFD, base, 0)
	created := linkErr == nil
	if unlinkErr := secureBindingUnlinkat(dirFD, tmpName, 0); unlinkErr != nil {
		if created {
			if rollbackErr := unlinkBindingIfSame(dirFD, base, stagedStat); rollbackErr != nil {
				return nil, fmt.Errorf("project cluster binding publication outcome is ambiguous after temporary-link cleanup failed (%v) and rollback failed: %w", unlinkErr, rollbackErr)
			}
		}
		return nil, fmt.Errorf("remove project cluster binding temporary link after publication rollback: %w", unlinkErr)
	}
	if linkErr != nil && !errors.Is(linkErr, unix.EEXIST) {
		return nil, fmt.Errorf("publish create-once project cluster binding: %w", linkErr)
	}
	if err := secureBindingFsync(dirFD); err != nil {
		if created {
			if rollbackErr := unlinkBindingIfSame(dirFD, base, stagedStat); rollbackErr != nil {
				return nil, fmt.Errorf("project cluster binding publication outcome is ambiguous after directory sync failed (%v) and rollback failed: %w", err, rollbackErr)
			}
			return nil, fmt.Errorf("sync project cluster binding directory after publication rollback: %w", err)
		}
	}
	winner, err := secureBindingWinnerRead(dirFD, base)
	if err != nil {
		if created {
			if rollbackErr := unlinkBindingIfSame(dirFD, base, stagedStat); rollbackErr != nil {
				return nil, fmt.Errorf("project cluster binding publication outcome is ambiguous after winner validation failed (%v) and rollback failed: %w", err, rollbackErr)
			}
		}
		return nil, err
	}
	if err := verifyBindingDirectoryPath(dir, dirStat); err != nil {
		if created {
			if rollbackErr := unlinkBindingIfSame(dirFD, base, stagedStat); rollbackErr != nil {
				return nil, fmt.Errorf("%v; project cluster binding rollback failed: %w", err, rollbackErr)
			}
		}
		return nil, err
	}
	return winner, nil
}

func unlinkBindingIfSame(dirFD int, name string, expected unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("inspect published project cluster binding for rollback: %w", err)
	}
	if !sameBindingInode(current, expected) {
		return fmt.Errorf("published project cluster binding changed before rollback")
	}
	if err := secureBindingUnlinkat(dirFD, name, 0); err != nil {
		return fmt.Errorf("remove published project cluster binding during rollback: %w", err)
	}
	if err := secureBindingFsync(dirFD); err != nil {
		return fmt.Errorf("sync project cluster binding rollback: %w", err)
	}
	return nil
}

func openSecureBindingDirectory(path string, create bool) (int, unix.Stat_t, error) {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return -1, unix.Stat_t{}, fmt.Errorf("create Tako workspace state directory: %w", err)
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return -1, unix.Stat_t{}, os.ErrNotExist
		}
		if errors.Is(err, unix.ELOOP) {
			return -1, unix.Stat_t{}, fmt.Errorf("Tako workspace state path must be a directory and not a symlink")
		}
		return -1, unix.Stat_t{}, fmt.Errorf("open Tako workspace state directory without following links: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect opened Tako workspace state directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("Tako workspace state path must be a directory")
	}
	if stat.Mode&0022 != 0 {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("Tako workspace state directory must not be group- or world-writable (mode %04o)", stat.Mode&0777)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("Tako workspace state directory must be owned by effective user %d, got %d", os.Geteuid(), stat.Uid)
	}
	return fd, stat, nil
}

func readBindingAt(dirFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("project cluster binding must be a regular file and not a symlink")
		}
		return nil, fmt.Errorf("open project cluster binding without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, fmt.Errorf("inspect opened project cluster binding: %w", err)
	}
	if err := validateBindingFileStat(opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read project cluster binding: %w", err)
	}
	if len(data) > maxDocumentSize {
		return nil, fmt.Errorf("project cluster binding exceeds %d bytes", maxDocumentSize)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || !sameBindingInode(opened, current) {
		return nil, fmt.Errorf("project cluster binding changed while it was read")
	}
	if err := validateBindingFileStat(current); err != nil {
		return nil, err
	}
	return data, nil
}

func validateBindingFileStat(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("project cluster binding must be a regular file")
	}
	if stat.Mode&0022 != 0 {
		return fmt.Errorf("project cluster binding must not be group- or world-writable (mode %04o)", stat.Mode&0777)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("project cluster binding must be owned by effective user %d, got %d", os.Geteuid(), stat.Uid)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("project cluster binding must have exactly one link, got %d", stat.Nlink)
	}
	if stat.Size > maxDocumentSize {
		return fmt.Errorf("project cluster binding exceeds %d bytes", maxDocumentSize)
	}
	return nil
}

func verifyBindingDirectoryPath(path string, expected unix.Stat_t) error {
	fd, current, err := openSecureBindingDirectory(path, false)
	if err != nil {
		return fmt.Errorf("revalidate Tako workspace state directory: %w", err)
	}
	unix.Close(fd)
	if current.Dev != expected.Dev || current.Ino != expected.Ino {
		return fmt.Errorf("Tako workspace state directory changed during binding operation")
	}
	return nil
}

func sameBindingInode(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func secureBindingTempName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate project cluster binding temporary name: %w", err)
	}
	return ".platform-cluster." + hex.EncodeToString(random[:]) + ".tmp", nil
}
