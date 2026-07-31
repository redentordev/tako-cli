//go:build !linux

package takod

import "fmt"

func availableHostMemoryBytes() (int64, error) {
	return 0, fmt.Errorf("host available-memory admission is unsupported on this platform")
}
