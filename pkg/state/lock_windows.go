//go:build windows

package state

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Acquire attempts to acquire the lock for an operation using LockFileEx
// This is atomic and race-condition free. Returns an error if the lock is held.
func (l *StateLock) Acquire(operation string) (*LockInfo, error) {
	lockPath := l.lockFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(l.basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Open/create the lock file
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	// Try to acquire exclusive lock (non-blocking) using Windows API
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	overlapped := &windows.Overlapped{}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
	if err != nil {
		file.Close()

		// Lock is held by another process - read lock info for error message
		existingLock, readErr := l.readLock()
		if readErr == nil && existingLock != nil {
			// Check if lock info indicates it's stale (process no longer exists)
			if time.Since(existingLock.Created) > LockTimeout {
				// Lock file exists but is stale - try to force acquire
				return l.forceAcquire(operation)
			}
			return nil, fmt.Errorf("state is locked by %s (operation: %s, started: %s ago). "+
				"If you believe this is stale, delete %s",
				existingLock.Who,
				existingLock.Operation,
				time.Since(existingLock.Created).Round(time.Second),
				lockPath)
		}
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	// We have the lock - now write our lock info
	lockInfo := l.createLockInfo(operation)

	if err := l.writeLockInfo(file, lockInfo); err != nil {
		overlapped := &windows.Overlapped{}
		windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		file.Close()
		return nil, err
	}

	// Keep the file open to maintain the lock
	l.lockFile = file

	return lockInfo, nil
}

// Release releases the lock by unlocking and closing the file handle
func (l *StateLock) Release(lockInfo *LockInfo) error {
	if lockInfo == nil {
		return nil
	}

	// Release the lock and close the file
	if l.lockFile != nil {
		// Verify we own the lock before releasing
		currentLock, err := l.readLock()
		if err == nil && currentLock != nil && currentLock.ID != lockInfo.ID {
			return fmt.Errorf("cannot release lock: lock ID mismatch (held by %s)", currentLock.Who)
		}

		// Unlock the file
		overlapped := &windows.Overlapped{}
		windows.UnlockFileEx(windows.Handle(l.lockFile.Fd()), 0, 1, 0, overlapped)

		// Close the file
		l.lockFile.Close()
		l.lockFile = nil
	}

	// Optionally remove the lock file
	lockPath := l.lockFilePath()
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		// Non-fatal - the lock is already released
		return nil
	}

	return nil
}

// ForceRelease forcefully releases the lock (use with caution)
func (l *StateLock) ForceRelease() error {
	// Close our file handle if we have one
	if l.lockFile != nil {
		overlapped := &windows.Overlapped{}
		windows.UnlockFileEx(windows.Handle(l.lockFile.Fd()), 0, 1, 0, overlapped)
		l.lockFile.Close()
		l.lockFile = nil
	}

	// Remove the lock file
	lockPath := l.lockFilePath()
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to force release lock: %w", err)
	}
	return nil
}
