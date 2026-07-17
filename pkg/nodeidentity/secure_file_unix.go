//go:build linux || darwin

package nodeidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func createSecureFile(path string, data []byte) error {
	return createSecureFileWithHook(path, data, nil)
}

func replaceSecureFile(path string, data []byte) error {
	dir, base, err := splitIdentityPath(path)
	if err != nil {
		return err
	}
	dirFD, err := openProtectedDirectory(dir)
	if err != nil {
		return fmt.Errorf("protect identity directory: %w", err)
	}
	defer unix.Close(dirFD)
	existingFD, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open existing secure file: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(existingFD, &stat); err != nil {
		_ = unix.Close(existingFD)
		return fmt.Errorf("inspect existing secure file: %w", err)
	}
	_ = unix.Close(existingFD)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fmt.Errorf("existing secure file must be a singly-linked non-writable-by-group-or-other regular file owned by uid %d", os.Geteuid())
	}
	tmpName, tmpFD, err := createStagingFileAt(dirFD)
	if err != nil {
		return fmt.Errorf("create secure staging file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(dirFD, tmpName, 0)
		}
	}()
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	if tmp == nil {
		_ = unix.Close(tmpFD)
		return fmt.Errorf("create secure staging file handle")
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secure staging file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync secure staging file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close secure staging file: %w", err)
	}
	if err := verifyDirectoryPathStillMatches(dir, dirFD); err != nil {
		return fmt.Errorf("identity directory changed before replacement: %w", err)
	}
	if err := unix.Renameat(dirFD, tmpName, dirFD, base); err != nil {
		return fmt.Errorf("publish secure replacement: %w", err)
	}
	cleanup = false
	if err := verifyDirectoryPathStillMatches(dir, dirFD); err != nil {
		return fmt.Errorf("identity directory changed during replacement: %w", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("sync identity directory: %w", err)
	}
	return nil
}

// createSecureFileWithHook exists so tests can deterministically replace the
// pathname after the protected directory descriptor is open. Publication
// must remain relative to that descriptor, never the substituted path.
func createSecureFileWithHook(path string, data []byte, beforePublish func()) error {
	dir, base, err := splitIdentityPath(path)
	if err != nil {
		return err
	}
	dirFD, err := openProtectedDirectory(dir)
	if err != nil {
		return fmt.Errorf("protect identity directory: %w", err)
	}
	defer unix.Close(dirFD)

	tmpName, tmpFD, err := createStagingFileAt(dirFD)
	if err != nil {
		return fmt.Errorf("create identity staging file: %w", err)
	}
	defer unix.Unlinkat(dirFD, tmpName, 0)
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	if tmp == nil {
		_ = unix.Close(tmpFD)
		return fmt.Errorf("create identity staging file handle")
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write identity staging file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync identity staging file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close identity staging file: %w", err)
	}
	if beforePublish != nil {
		beforePublish()
	}
	if err := verifyDirectoryPathStillMatches(dir, dirFD); err != nil {
		return fmt.Errorf("identity directory changed before publication: %w", err)
	}
	if err := unix.Linkat(dirFD, tmpName, dirFD, base, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("installation identity already exists at %s", path)
		}
		return fmt.Errorf("publish installation identity: %w", err)
	}
	if err := verifyDirectoryPathStillMatches(dir, dirFD); err != nil {
		_ = unix.Unlinkat(dirFD, base, 0)
		_ = unix.Fsync(dirFD)
		return fmt.Errorf("identity directory changed during publication: %w", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("sync identity directory: %w", err)
	}
	return nil
}

func verifyDirectoryPathStillMatches(path string, expectedFD int) error {
	currentFD, err := openProtectedDirectory(path)
	if err != nil {
		return err
	}
	defer unix.Close(currentFD)
	var expected unix.Stat_t
	var current unix.Stat_t
	if err := unix.Fstat(expectedFD, &expected); err != nil {
		return err
	}
	if err := unix.Fstat(currentFD, &current); err != nil {
		return err
	}
	if expected.Dev != current.Dev || expected.Ino != current.Ino {
		return fmt.Errorf("opened directory no longer matches %s", path)
	}
	return nil
}

func readSecureFile(path string, limit int64) ([]byte, error) {
	dir, base, err := splitIdentityPath(path)
	if err != nil {
		return nil, err
	}
	dirFD, err := openProtectedDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("protect identity directory: %w", err)
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("installation identity %s must be a regular file, not a symlink", path)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), base)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open installation identity %s", path)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("inspect opened installation identity: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("installation identity %s must be a regular file", path)
	}
	if stat.Mode&0077 != 0 {
		return nil, fmt.Errorf("installation identity %s must not be accessible by group or other users", path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("installation identity %s: must be owned by uid %d, got uid %d", path, os.Geteuid(), stat.Uid)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("installation identity %s exceeds %d bytes", path, limit)
	}
	return data, nil
}

// Inventories contain public membership and transport identity, never private
// credentials. They are root-published but world-readable so an unprivileged
// local Tako operator can resolve application intent without gaining write
// authority. Integrity still requires a protected directory, no symlinks,
// one link, and no group/other write bits.
func readPublishedInventoryFile(path string, limit int64) ([]byte, error) {
	dir, base, err := splitIdentityPath(path)
	if err != nil {
		return nil, err
	}
	dirFD, err := openPublishedInventoryDirectory(dir, requiresRootPublishedOwner(path))
	if err != nil {
		return nil, fmt.Errorf("protect inventory directory: %w", err)
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), base)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open cluster inventory %s", path)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid())
	ownerOK := publishedOwnerAllowed(path, stat.Uid, uid)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || stat.Nlink != 1 || !ownerOK {
		if requiresRootPublishedOwner(path) {
			return nil, fmt.Errorf("published platform trust file %s must be a singly-linked regular file owned by root and not writable by group or other", path)
		}
		return nil, fmt.Errorf("cluster inventory must be a singly-linked regular file owned by root or uid %d and not writable by group or other", uid)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("cluster inventory exceeds %d bytes", limit)
	}
	return data, nil
}

func publishInventoryPermissions(path string) error {
	dir, base, err := splitIdentityPath(path)
	if err != nil {
		return err
	}
	dirFD, err := openProtectedDirectory(dir)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fmt.Errorf("refusing to publish unsafe cluster inventory")
	}
	if err := unix.Fchmod(fd, 0644); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	return unix.Fsync(dirFD)
}

func splitIdentityPath(path string) (string, string, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	base := filepath.Base(clean)
	if clean == "." || base == "." || base == string(filepath.Separator) || base == "" {
		return "", "", fmt.Errorf("identity path must name a file")
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", "", err
	}
	return filepath.Dir(abs), filepath.Base(abs), nil
}

func createStagingFileAt(dirFD int) (string, int, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", -1, err
		}
		name := ".identity." + hex.EncodeToString(random[:]) + ".tmp"
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return name, fd, err
	}
	return "", -1, fmt.Errorf("could not allocate a unique staging name")
}

func openProtectedDirectory(path string) (int, error) {
	return openProtectedDirectoryWithHook(path, nil)
}

func openProtectedDirectoryWithHook(path string, beforeComponent func(string)) (int, error) {
	return openProtectedDirectoryWithPolicy(path, beforeComponent, false, false)
}

func openPublishedInventoryDirectory(path string, requireRootFinal bool) (int, error) {
	return openProtectedDirectoryWithPolicy(path, nil, true, requireRootFinal)
}

func openProtectedDirectoryWithPolicy(path string, beforeComponent func(string), allowRootOwnedFinal, requireRootFinal bool) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	components, err := safePathComponents(abs)
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := validateOpenDirectoryPolicy(fd, string(filepath.Separator), len(components) == 0, allowRootOwnedFinal, requireRootFinal); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	symlinkCount := 0
	for len(components) > 0 {
		component := components[0]
		components = components[1:]
		if beforeComponent != nil {
			beforeComponent(component)
		}
		nextFD, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			target, absolute, linkErr := trustedSymlinkTargetAt(fd, component)
			if linkErr != nil {
				_ = unix.Close(fd)
				return -1, fmt.Errorf("inspect protected path component %q after %v: %w", component, openErr, linkErr)
			}
			symlinkCount++
			if symlinkCount > 8 {
				_ = unix.Close(fd)
				return -1, fmt.Errorf("identity path contains too many system symlinks")
			}
			targetComponents, componentErr := safePathComponents(target)
			if componentErr != nil {
				_ = unix.Close(fd)
				return -1, componentErr
			}
			if absolute {
				_ = unix.Close(fd)
				fd, err = unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
				if err != nil {
					return -1, err
				}
				if err := validateOpenDirectoryPolicy(fd, string(filepath.Separator), false, allowRootOwnedFinal, requireRootFinal); err != nil {
					_ = unix.Close(fd)
					return -1, err
				}
			}
			components = append(targetComponents, components...)
			continue
		}
		_ = unix.Close(fd)
		fd = nextFD
		if err := validateOpenDirectoryPolicy(fd, component, len(components) == 0, allowRootOwnedFinal, requireRootFinal); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

func validateOpenDirectory(fd int, component string, final bool) error {
	return validateOpenDirectoryPolicy(fd, component, final, false, false)
}

func validateOpenDirectoryPolicy(fd int, component string, final bool, allowRootOwnedFinal, requireRootFinal bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if final && allowRootOwnedFinal {
		euid := uint32(os.Geteuid())
		if requireRootFinal && stat.Uid != 0 {
			return fmt.Errorf("published platform trust directory component %q must be owned by root, got uid %d", component, stat.Uid)
		}
		if stat.Uid != 0 && stat.Uid != euid {
			return fmt.Errorf("inventory directory component %q must be owned by root or uid %d, got uid %d", component, euid, stat.Uid)
		}
		if stat.Mode&0022 != 0 {
			return fmt.Errorf("inventory directory component %q must not be writable by group or other users", component)
		}
		return nil
	}
	return validateDirectoryStat(component, stat, final)
}

func safePathComponents(path string) ([]string, error) {
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		clean = strings.TrimPrefix(clean, string(filepath.Separator))
	}
	if clean == "" || clean == "." {
		return nil, nil
	}
	components := strings.Split(clean, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("identity path contains unsafe component %q", component)
		}
	}
	return components, nil
}

func trustedSymlinkTargetAt(parentFD int, component string) (string, bool, error) {
	var linkStat unix.Stat_t
	if err := unix.Fstatat(parentFD, component, &linkStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", false, err
	}
	if linkStat.Mode&unix.S_IFMT != unix.S_IFLNK || linkStat.Uid != 0 {
		return "", false, fmt.Errorf("identity path component %q must not be a symlink", component)
	}
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentFD, &parentStat); err != nil {
		return "", false, err
	}
	if parentStat.Uid != 0 || parentStat.Mode&0022 != 0 {
		return "", false, fmt.Errorf("identity path component %q is an untrusted symlink", component)
	}
	buffer := make([]byte, 4096)
	length, err := unix.Readlinkat(parentFD, component, buffer)
	if err != nil {
		return "", false, err
	}
	if length == len(buffer) {
		return "", false, fmt.Errorf("identity path symlink %q is too long", component)
	}
	target := string(buffer[:length])
	return target, filepath.IsAbs(target), nil
}

func validateDirectoryStat(component string, stat unix.Stat_t, final bool) error {
	euid := uint32(os.Geteuid())
	if final {
		if stat.Uid != euid {
			return fmt.Errorf("identity directory component %q must be owned by uid %d, got uid %d", component, euid, stat.Uid)
		}
		if stat.Mode&0022 != 0 {
			return fmt.Errorf("identity directory component %q must not be writable by group or other users", component)
		}
		return nil
	}
	if stat.Uid != 0 && stat.Uid != euid {
		return fmt.Errorf("identity path component %q has untrusted uid %d", component, stat.Uid)
	}
	if stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
		return fmt.Errorf("identity path component %q is writable without sticky-bit protection", component)
	}
	return nil
}
