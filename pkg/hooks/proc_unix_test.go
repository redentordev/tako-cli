//go:build !windows

package hooks

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid is a live (non-zombie) process.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	// A killed child reparented to a non-reaping init lingers as a zombie.
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}
