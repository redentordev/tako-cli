//go:build !linux && !darwin

package platform

import (
	"fmt"
	"os"
)

func ensureOwnedDurableFile(path string, uid int, gid int) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("initialize durable platform file %s: %w", path, err)
	}
	if err := file.Chmod(0640); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure platform durable file %s: %w", path, err)
	}
	if err := file.Chown(uid, gid); err != nil {
		_ = file.Close()
		return fmt.Errorf("own platform durable file %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync platform durable file %s: %w", path, err)
	}
	return file.Close()
}
