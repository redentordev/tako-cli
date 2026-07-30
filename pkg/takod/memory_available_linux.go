//go:build linux

package takod

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func availableHostMemoryBytes() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kilobytes, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kilobytes > (1<<63-1)/1024 {
				return 0, fmt.Errorf("invalid MemAvailable value")
			}
			return kilobytes * 1024, nil
		}
	}
	return 0, fmt.Errorf("MemAvailable is unavailable")
}
