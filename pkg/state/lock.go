package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// NewStateLock creates a new state lock manager
func NewStateLock(basePath string) *StateLock {
	return &StateLock{basePath: basePath}
}

// lockFilePath returns the path to the lock file
func (l *StateLock) lockFilePath() string {
	return filepath.Join(l.basePath, LockFileName)
}

// Acquire attempts to acquire the lock for an operation
// Returns an error if the lock is held by another process
func (l *StateLock) Acquire(operation string) (*LockInfo, error) {
	lockPath := l.lockFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(l.basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Check for existing lock
	existingLock, err := l.readLock()
	if err == nil && existingLock != nil {
		// Check if lock is expired
		if time.Since(existingLock.Created) > LockTimeout {
			// Lock is stale, we can take it
			if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to remove stale lock: %w", err)
			}
		} else {
			// Lock is active
			return nil, fmt.Errorf("state is locked by %s (operation: %s, started: %s ago). "+
				"If you believe this is stale, delete %s",
				existingLock.Who,
				existingLock.Operation,
				time.Since(existingLock.Created).Round(time.Second),
				lockPath)
		}
	}

	// Create lock info
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = "unknown"
	}

	lockInfo := &LockInfo{
		ID:        fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		Operation: operation,
		Who:       fmt.Sprintf("%s@%s", username, hostname),
		Created:   time.Now(),
		PID:       os.Getpid(),
	}

	// Write lock file atomically
	data, err := json.MarshalIndent(lockInfo, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lock info: %w", err)
	}

	// Use O_EXCL to ensure atomic creation
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			// Race condition - another process created the lock
			return nil, fmt.Errorf("lock was acquired by another process")
		}
		return nil, fmt.Errorf("failed to create lock file: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		os.Remove(lockPath)
		return nil, fmt.Errorf("failed to write lock file: %w", err)
	}

	return lockInfo, nil
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

// Release releases the lock
func (l *StateLock) Release(lockInfo *LockInfo) error {
	if lockInfo == nil {
		return nil
	}

	lockPath := l.lockFilePath()

	// Verify we own the lock before releasing
	currentLock, err := l.readLock()
	if err != nil {
		// Lock file doesn't exist, nothing to release
		return nil
	}

	if currentLock.ID != lockInfo.ID {
		return fmt.Errorf("cannot release lock: lock ID mismatch (held by %s)", currentLock.Who)
	}

	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to release lock: %w", err)
	}

	return nil
}

// ForceRelease forcefully releases the lock (use with caution)
func (l *StateLock) ForceRelease() error {
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
