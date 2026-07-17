//go:build !linux && !darwin

package takod

import (
	"fmt"
	"path/filepath"
)

func diskFilesystemIdentity(path string) (string, error) {
	volume := filepath.VolumeName(filepath.Clean(path))
	if volume == "" {
		return "", fmt.Errorf("filesystem identity is unavailable for %s", path)
	}
	return volume, nil
}
