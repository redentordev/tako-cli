//go:build !linux && !darwin

package platform

import "fmt"

type OSDiskProbe struct{}

func (OSDiskProbe) AvailableBytes(string) (int64, error) {
	return 0, fmt.Errorf("platform disk admission is unsupported on this operating system")
}
