//go:build !linux && !darwin

package takod

import "fmt"

func totalDiskBytes(string) (int64, error) {
	return 0, fmt.Errorf("filesystem size is unsupported on this operating system")
}
