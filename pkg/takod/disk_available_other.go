//go:build !linux && !darwin

package takod

import "fmt"

func availableDiskBytes(string) (int64, error) {
	return 0, fmt.Errorf("free-disk admission is unsupported on this operating system")
}
