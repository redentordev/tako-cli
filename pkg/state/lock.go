package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// LockFileName is the name of the lock file
	LockFileName = ".lock"

	// LockTimeout is how long a lock is considered valid
	LockTimeout = 30 * time.Minute

	// LockPollInterval is how often to check for lock release
	LockPollInterval = 1 * time.Second

	// MaxLockWait is the maximum time to wait for a lock
	MaxLockWait = 5 * time.Minute
)

// LockInfo contains information about who holds the lock
type LockInfo struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"` // deploy, destroy, rollback, etc.
	Who       string    `json:"who"`       // user@hostname
	Created   time.Time `json:"created"`
	PID       int       `json:"pid"`
}

// StateLock manages state file locking to prevent concurrent modifications
type StateLock struct {
	basePath string
	lockFile *os.File // File handle for flock - kept open while lock is held
}

// NewStateLock creates a new state lock manager
func NewStateLock(basePath string) *StateLock {
	return &StateLock{basePath: basePath}
}

// createLockInfo creates a new lock info struct with current process details
func (l *StateLock) createLockInfo(operation string) *LockInfo {
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = "unknown"
	}

	return &LockInfo{
		ID:        fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		Operation: operation,
		Who:       fmt.Sprintf("%s@%s", username, hostname),
		Created:   time.Now(),
		PID:       os.Getpid(),
	}
}

// lockFilePath returns the path to the lock file
func (l *StateLock) lockFilePath() string {
	return filepath.Join(l.basePath, LockFileName)
}

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

	// Truncate and write new lock info
	if err := file.Truncate(0); err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("failed to truncate lock file: %w", err)
	}

	if _, err := file.Seek(0, 0); err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("failed to seek lock file: %w", err)
	}

	data, err := json.MarshalIndent(lockInfo, "", "  ")
	if err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("failed to marshal lock info: %w", err)
	}

	if _, err := file.Write(data); err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("failed to write lock file: %w", err)
	}

	// Sync to disk
	if err := file.Sync(); err != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("failed to sync lock file: %w", err)
	}

	// Keep the file open to maintain the flock
	l.lockFile = file

	return lockInfo, nil
}

// forceAcquire attempts to acquire a stale lock by removing and recreating it
func (l *StateLock) forceAcquire(operation string) (*LockInfo, error) {
	lockPath := l.lockFilePath()

	// Remove the stale lock file
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove stale lock: %w", err)
	}

	// Try to acquire again
	return l.Acquire(operation)
}

// AcquireWithWait attempts to acquire the lock, waiting if necessary
func (l *StateLock) AcquireWithWait(operation string) (*LockInfo, error) {
	deadline := time.Now().Add(MaxLockWait)

	for time.Now().Before(deadline) {
		lockInfo, err := l.Acquire(operation)
		if err == nil {
			return lockInfo, nil
		}

		// Wait and retry
		time.Sleep(LockPollInterval)
	}

	return nil, fmt.Errorf("timed out waiting for lock after %s", MaxLockWait)
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

// IsLocked checks if the state is currently locked
func (l *StateLock) IsLocked() bool {
	lockInfo, err := l.readLock()
	if err != nil || lockInfo == nil {
		return false
	}

	// Check if lock is expired
	if time.Since(lockInfo.Created) > LockTimeout {
		return false
	}

	return true
}

// GetLockInfo returns information about the current lock holder
func (l *StateLock) GetLockInfo() (*LockInfo, error) {
	return l.readLock()
}

// readLock reads the current lock file
func (l *StateLock) readLock() (*LockInfo, error) {
	lockPath := l.lockFilePath()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var lockInfo LockInfo
	if err := json.Unmarshal(data, &lockInfo); err != nil {
		return nil, err
	}

	return &lockInfo, nil
}
