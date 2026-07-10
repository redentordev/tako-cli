package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseServiceFileOwner parses a numeric uid or uid:gid ownership value.
// Named users are intentionally unsupported because takod cannot resolve
// container image passwd databases on the host.
func ParseServiceFileOwner(value string) (uid int, gid int, configured bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, false, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) > 2 || parts[0] == "" {
		return 0, 0, false, fmt.Errorf("owner must be a numeric uid or uid:gid")
	}
	uid64, parseErr := strconv.ParseUint(parts[0], 10, 31)
	if parseErr != nil {
		return 0, 0, false, fmt.Errorf("owner uid must be numeric")
	}
	gid64 := uid64
	if len(parts) == 2 {
		if parts[1] == "" {
			return 0, 0, false, fmt.Errorf("owner gid must be numeric")
		}
		gid64, parseErr = strconv.ParseUint(parts[1], 10, 31)
		if parseErr != nil {
			return 0, 0, false, fmt.Errorf("owner gid must be numeric")
		}
	}
	return int(uid64), int(gid64), true, nil
}
