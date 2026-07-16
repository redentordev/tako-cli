//go:build !linux

package platform

import "fmt"

func stagePlatformBinary(string, string) error {
	return fmt.Errorf("platform service binary staging is supported only on Linux")
}
