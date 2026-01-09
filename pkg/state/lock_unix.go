//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Acquire attempts to acquire the lock for an operation using flock
// This is atomic and race-condition free. Returns an error if the lock is held.
func (l *StateLock) Acquire(operation string) (*LockInfo, error) {
	lockPath := l.lockFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(l.basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Open/create the lock file (we'll use flock for the actual locking)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	// Try to acquire exclusive lock (non-blocking)
	// LOCK_EX = exclusive lock, LOCK_NB = non-blocking
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
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
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, err
	}

	// Keep the file open to maintain the flock
	l.lockFile = file

	return lockInfo, nil
}

// Release releases the lock by unlocking flock and closing the file handle
func (l *StateLock) Release(lockInfo *LockInfo) error {
	if lockInfo == nil {
		return nil
	}

	// Release the flock and close the file
	if l.lockFile != nil {
		// Verify we own the lock before releasing
		currentLock, err := l.readLock()
		if err == nil && currentLock != nil && currentLock.ID != lockInfo.ID {
			return fmt.Errorf("cannot release lock: lock ID mismatch (held by %s)", currentLock.Who)
		}

		// Unlock the flock
		syscall.Flock(int(l.lockFile.Fd()), syscall.LOCK_UN)

		// Close the file (this also releases the lock if flock wasn't called)
		l.lockFile.Close()
		l.lockFile = nil
	}

	// Optionally remove the lock file (not strictly necessary with flock)
	lockPath := l.lockFilePath()
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		// Non-fatal - the flock is already released
		return nil
	}

	return nil
}

// ForceRelease forcefully releases the lock (use with caution)
func (l *StateLock) ForceRelease() error {
	// Close our file handle if we have one
	if l.lockFile != nil {
		syscall.Flock(int(l.lockFile.Fd()), syscall.LOCK_UN)
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
