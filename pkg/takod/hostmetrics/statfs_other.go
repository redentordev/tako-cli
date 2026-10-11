//go:build !linux

package hostmetrics

import "errors"

// Statfs is only implemented on Linux, the only platform takod runs on.
func Statfs(path string) (FilesystemUsage, error) {
	return FilesystemUsage{}, errors.New("statfs is not supported on this platform")
}
