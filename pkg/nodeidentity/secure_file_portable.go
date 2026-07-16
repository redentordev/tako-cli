//go:build !linux && !darwin

package nodeidentity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Unix local transport is fail-closed outside Linux and Darwin. This fallback
// keeps identity tooling buildable on Windows while retaining exclusive
// creation and final-inode validation.
func createSecureFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := validatePortableDirectory(dir); err != nil {
		return fmt.Errorf("protect identity directory: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("installation identity already exists at %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".identity.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return fmt.Errorf("installation identity already exists at %s", path)
		}
		return err
	}
	return nil
}

func readSecureFile(path string, limit int64) ([]byte, error) {
	if err := validatePortableDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("protect identity directory: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("installation identity %s must be a regular file, not a symlink", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(pathInfo, info) || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("installation identity %s changed or is not regular", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("installation identity %s must not be accessible by group or other users", path)
	}
	if err := validateOwner(info); err != nil {
		return nil, fmt.Errorf("installation identity %s: %w", path, err)
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

func validatePortableDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a real directory, not a symlink", path)
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s must not be writable by group or other users", path)
	}
	if err := validateOwner(info); err != nil && !strings.Contains(err.Error(), "cannot determine") {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
