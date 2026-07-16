//go:build !windows

package platform

import "os"

func runningAsRoot() bool { return os.Geteuid() == 0 }
