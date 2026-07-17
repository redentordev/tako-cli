//go:build windows

package recovery

import (
	"errors"
	"fmt"
)

var ErrNodeUpgradeLeaseActive = errors.New("node lifecycle mutation blocked by active node upgrade lease")

func AcquireMutationLock(string) (func(), error)       { return func() {}, nil }
func AcquireOperationBarrier(string) (func(), error)   { return func() {}, nil }
func AcquireMaintenanceBarrier(string) (func(), error) { return func() {}, nil }
func AcquireNodeUpgradeLifecycleExclusion() (func(), error) {
	return func() {}, nil
}
func AcquireNodeUpgradeLifecycleExclusionForDataDir(string) (func(), error) {
	return func() {}, nil
}
func AcquireSnapshotLock(string) (func(), error) {
	return nil, fmt.Errorf("platform recovery snapshots require a Unix controller host")
}
