//go:build !windows

package recovery

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultNodeUpgradeLockBase = "/var/lib/tako/node-upgrade/locks"

var ErrNodeUpgradeLeaseActive = errors.New("node lifecycle mutation blocked by active node upgrade lease")

// AcquireNodeUpgradeLifecycleExclusion serializes controller-local membership
// mutations with the per-node upgrade transaction. The returned release keeps
// the same kernel guard held for the entire lifecycle mutation.
func AcquireNodeUpgradeLifecycleExclusion() (func(), error) {
	return acquireNodeUpgradeLifecycleExclusion(defaultNodeUpgradeLockBase)
}

// AcquireNodeUpgradeLifecycleExclusionForDataDir derives the child
// node-upgrade lock from the takod data directory. It keeps production and
// isolated test roots on the same lock-ordering path.
func AcquireNodeUpgradeLifecycleExclusionForDataDir(dataDir string) (func(), error) {
	clean := filepath.Clean(dataDir)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("platform data directory must be absolute")
	}
	if err := ensureTrustedUpgradeParent(clean); err != nil {
		return nil, err
	}
	return acquireNodeUpgradeLifecycleExclusion(filepath.Join(clean, "node-upgrade", "locks"))
}

func acquireNodeUpgradeLifecycleExclusion(lockBase string) (func(), error) {
	upgradeDir := filepath.Dir(filepath.Clean(lockBase))
	if filepath.Base(upgradeDir) != "node-upgrade" || filepath.Base(filepath.Clean(lockBase)) != "locks" {
		return nil, fmt.Errorf("node upgrade lock base has an invalid layout")
	}
	if err := ensureProtectedUpgradeDirectory(upgradeDir); err != nil {
		return nil, err
	}
	if err := ensureProtectedUpgradeDirectory(lockBase); err != nil {
		return nil, err
	}

	guardPath := filepath.Join(lockBase, ".guard")
	fd, err := unix.Open(guardPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open node upgrade lifecycle guard: %w", err)
	}
	guard := os.NewFile(uintptr(fd), guardPath)
	if guard == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open node upgrade lifecycle guard")
	}
	release := func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = guard.Close()
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0077 != 0 || !upgradeOwnerAllowed(stat.Uid) {
		_ = guard.Close()
		return nil, fmt.Errorf("node upgrade lifecycle guard must be a private singly-linked regular file with trusted ownership")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = guard.Close()
		return nil, fmt.Errorf("lock node upgrade lifecycle guard: %w", err)
	}

	nodeLock := filepath.Join(lockBase, "node")
	info, err := os.Lstat(nodeLock)
	if os.IsNotExist(err) {
		return release, nil
	}
	if err != nil {
		release()
		return nil, fmt.Errorf("inspect node upgrade lease: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !fileOwnerAllowed(info) {
		release()
		return nil, fmt.Errorf("unsafe node upgrade lease path")
	}
	expiry, err := readNodeUpgradeExpiry(filepath.Join(nodeLock, "expiry"))
	if err != nil {
		release()
		return nil, err
	}
	if expiry > time.Now().Unix() {
		release()
		return nil, ErrNodeUpgradeLeaseActive
	}
	if err := os.RemoveAll(nodeLock); err != nil {
		release()
		return nil, fmt.Errorf("remove expired node upgrade lease: %w", err)
	}
	return release, nil
}

func readNodeUpgradeExpiry(path string) (int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open node upgrade lease expiry: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return 0, fmt.Errorf("open node upgrade lease expiry")
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0077 != 0 || stat.Size > 64 || !upgradeOwnerAllowed(stat.Uid) {
		return 0, fmt.Errorf("node upgrade lease expiry must be a private bounded regular file with trusted ownership")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return 0, fmt.Errorf("read node upgrade lease expiry: %w", err)
	}
	expiry, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return expiry, nil
}

func ensureProtectedUpgradeDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create node upgrade lock directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !fileOwnerAllowed(info) {
		return fmt.Errorf("node upgrade lock directory must be a protected real directory with trusted ownership")
	}
	return nil
}

func ensureTrustedUpgradeParent(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create platform data directory for node upgrade locks: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || !fileOwnerAllowed(info) {
		return fmt.Errorf("platform data directory must be a protected real directory with trusted ownership")
	}
	return nil
}

func fileOwnerAllowed(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && upgradeOwnerAllowed(stat.Uid)
}

func upgradeOwnerAllowed(uid uint32) bool {
	return uid == 0 || uid == uint32(os.Geteuid())
}

func AcquireMutationLock(dataDir string) (func(), error) {
	file, err := openPlatformLock(dataDir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}

func AcquireSnapshotLock(dataDir string) (func(), error) {
	file, err := openPlatformLock(dataDir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}

func AcquireOperationBarrier(dataDir string) (func(), error) {
	file, err := openOperationBarrier(dataDir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}

func AcquireMaintenanceBarrier(dataDir string) (func(), error) {
	file, err := openOperationBarrier(dataDir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
