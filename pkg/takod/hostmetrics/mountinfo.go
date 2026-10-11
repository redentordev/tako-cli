package hostmetrics

import (
	"strings"
)

// SameFilesystem reports whether two usages describe one filesystem: the
// same non-empty statfs ID or the same non-empty device identity. Either
// match is enough (a bind mount shares both; btrfs subvolumes share only
// the mount source recorded in Device).
func (u FilesystemUsage) SameFilesystem(other FilesystemUsage) bool {
	if u.ID != "" && u.ID == other.ID {
		return true
	}
	return u.Device != "" && u.Device == other.Device
}

// mountSourceFor returns the mount source (for example /dev/sda2) of the
// mount that holds path, from /proc/self/mountinfo content. The longest
// matching mount point wins; on a tie the later (stacked) mount wins.
func mountSourceFor(mountinfo []byte, path string) string {
	best, bestLen := "", -1
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 5 || separator+2 >= len(fields) {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		if !pathWithin(path, mountPoint) || len(mountPoint) < bestLen {
			continue
		}
		best, bestLen = unescapeMountField(fields[separator+2]), len(mountPoint)
	}
	return best
}

func pathWithin(path, mountPoint string) bool {
	if mountPoint == "/" || path == mountPoint {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(mountPoint, "/")+"/")
}

// unescapeMountField decodes the octal escapes (\040 for space and so on)
// the kernel uses in mountinfo fields.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var builder strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] == '\\' && index+3 < len(field) {
			value := 0
			valid := true
			for _, digit := range field[index+1 : index+4] {
				if digit < '0' || digit > '7' {
					valid = false
					break
				}
				value = value*8 + int(digit-'0')
			}
			if valid {
				builder.WriteByte(byte(value))
				index += 3
				continue
			}
		}
		builder.WriteByte(field[index])
	}
	return builder.String()
}
