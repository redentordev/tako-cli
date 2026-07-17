package recovery

import (
	"fmt"
	"os"
	"path/filepath"
)

// RestoreEncrypted authenticates the bundle, then extracts only into a new
// staging directory. It never overwrites live /etc/tako or /var/lib/tako.
func RestoreEncrypted(path, destination, expectedClusterID string, masterKey []byte) (*Manifest, error) {
	destination = filepath.Clean(destination)
	if destination == "." || destination == string(filepath.Separator) {
		return nil, fmt.Errorf("recovery staging destination is invalid")
	}
	if _, err := os.Lstat(destination); err == nil || !os.IsNotExist(err) {
		return nil, fmt.Errorf("recovery staging destination must not already exist")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("recovery staging parent must be a real directory")
	}
	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".partial-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(temporary, 0700); err != nil {
		_ = os.RemoveAll(temporary)
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	manifest, err := verifyEncryptedTo(path, expectedClusterID, masterKey, temporary)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(destination); err == nil || !os.IsNotExist(err) {
		return nil, fmt.Errorf("recovery staging destination was created concurrently")
	}
	if err := os.Rename(temporary, destination); err != nil {
		return nil, fmt.Errorf("commit authenticated recovery staging directory: %w", err)
	}
	committed = true
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return manifest, nil
}
