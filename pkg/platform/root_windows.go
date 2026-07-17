//go:build windows

package platform

func runningAsRoot() bool { return false }
