//go:build windows

package nodeidentity

func AcquireInventoryMutationLock(string) (func(), error) { return func() {}, nil }
