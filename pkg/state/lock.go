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
		username = os.Getenv("USERNAME") // Windows
	}
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

// writeLockInfo writes lock info to the lock file
func (l *StateLock) writeLockInfo(file *os.File, lockInfo *LockInfo) error {
	// Truncate and write new lock info
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("failed to truncate lock file: %w", err)
	}

	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek lock file: %w", err)
	}

	data, err := json.MarshalIndent(lockInfo, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal lock info: %w", err)
	}

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("failed to write lock file: %w", err)
	}

	// Sync to disk
	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync lock file: %w", err)
	}

	return nil
}
