// Package hostmetrics reads host metrics natively from /proc, /sys and
// statfs (add-server-status-history design D1). Parsers are pure functions
// over file contents so they can be tested with fixture files; Source reads
// from injectable proc and sys roots.
package hostmetrics

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// CPUTimes is the aggregate "cpu" line of /proc/stat in clock ticks.
// Busy is total minus idle and iowait, the same formula as the retired
// bash agent.
type CPUTimes struct {
	Total uint64
	Busy  uint64
}

// ParseCPUStat parses /proc/stat and returns the aggregate CPU times and the
// number of per-CPU lines.
func ParseCPUStat(data []byte) (CPUTimes, int, error) {
	var times CPUTimes
	found := false
	cpus := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			if _, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err == nil {
				cpus++
			}
			continue
		}
		if len(fields) < 5 {
			return CPUTimes{}, 0, fmt.Errorf("short cpu line in /proc/stat")
		}
		values := make([]uint64, 0, 8)
		// user nice system idle iowait irq softirq steal; guest and
		// guest_nice are already included in user and nice.
		for index := 1; index < len(fields) && index <= 8; index++ {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return CPUTimes{}, 0, fmt.Errorf("invalid cpu value %q in /proc/stat", fields[index])
			}
			values = append(values, value)
		}
		var total uint64
		for _, value := range values {
			total += value
		}
		idle := values[3]
		if len(values) > 4 {
			idle += values[4]
		}
		times = CPUTimes{Total: total, Busy: total - idle}
		found = true
	}
	if !found {
		return CPUTimes{}, 0, fmt.Errorf("no aggregate cpu line in /proc/stat")
	}
	return times, cpus, nil
}

// Memory is the subset of /proc/meminfo used by Tako, in kB.
type Memory struct {
	TotalKB     uint64
	AvailableKB uint64
	SwapTotalKB uint64
	SwapFreeKB  uint64
}

// ParseMeminfo parses /proc/meminfo. MemTotal and MemAvailable are required.
func ParseMeminfo(data []byte) (Memory, error) {
	var memory Memory
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memory.TotalKB = value
		case "MemAvailable:":
			memory.AvailableKB = value
		case "SwapTotal:":
			memory.SwapTotalKB = value
		case "SwapFree:":
			memory.SwapFreeKB = value
		default:
			continue
		}
		seen[fields[0]] = true
	}
	if !seen["MemTotal:"] || !seen["MemAvailable:"] || memory.TotalKB == 0 {
		return Memory{}, fmt.Errorf("/proc/meminfo lacks MemTotal or MemAvailable")
	}
	if memory.AvailableKB > memory.TotalKB {
		memory.AvailableKB = memory.TotalKB
	}
	if memory.SwapFreeKB > memory.SwapTotalKB {
		memory.SwapFreeKB = memory.SwapTotalKB
	}
	return memory, nil
}

// Load is /proc/loadavg.
type Load struct {
	One, Five, Fifteen float64
	// Text keeps the kernel's formatting for the legacy string fields.
	OneText, FiveText, FifteenText string
}

// ParseLoadavg parses /proc/loadavg.
func ParseLoadavg(data []byte) (Load, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("short /proc/loadavg")
	}
	values := make([]float64, 3)
	for index := 0; index < 3; index++ {
		value, err := strconv.ParseFloat(fields[index], 64)
		if err != nil || value < 0 {
			return Load{}, fmt.Errorf("invalid load value %q", fields[index])
		}
		values[index] = value
	}
	return Load{One: values[0], Five: values[1], Fifteen: values[2], OneText: fields[0], FiveText: fields[1], FifteenText: fields[2]}, nil
}

// ParseUptime parses /proc/uptime and returns whole seconds.
func ParseUptime(data []byte) (int64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("empty /proc/uptime")
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid uptime %q", fields[0])
	}
	return int64(value), nil
}

// NetCounters are cumulative interface byte counters.
type NetCounters struct {
	RxBytes uint64
	TxBytes uint64
}

// InterfaceFilter decides whether an interface counts toward host network
// I/O.
type InterfaceFilter func(name string) bool

// DefaultInterfaceFilter excludes loopback, container veth pairs, Docker and
// user-defined bridges, and the Tako mesh interfaces.
func DefaultInterfaceFilter(meshInterfaces ...string) InterfaceFilter {
	mesh := map[string]bool{}
	for _, name := range meshInterfaces {
		if name = strings.TrimSpace(name); name != "" {
			mesh[name] = true
		}
	}
	return func(name string) bool {
		switch {
		case name == "lo", mesh[name]:
			return false
		case strings.HasPrefix(name, "veth"), strings.HasPrefix(name, "docker"), strings.HasPrefix(name, "br-"):
			return false
		}
		return true
	}
}

// ParseNetDev sums receive and transmit bytes over the interfaces include
// accepts.
func ParseNetDev(data []byte, include InterfaceFilter) (NetCounters, error) {
	var counters NetCounters
	parsed := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		fields := strings.Fields(line[colon+1:])
		if name == "" || len(fields) < 9 {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
		tx, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			return NetCounters{}, fmt.Errorf("invalid counters for interface %s", name)
		}
		parsed = true
		if include != nil && !include(name) {
			continue
		}
		counters.RxBytes += rx
		counters.TxBytes += tx
	}
	if !parsed {
		return NetCounters{}, fmt.Errorf("no interfaces in /proc/net/dev")
	}
	return counters, nil
}

// DiskCounters are cumulative sector counters (512-byte units).
type DiskCounters struct {
	ReadSectors  uint64
	WriteSectors uint64
}

// IncludeBlockDevice excludes loop, RAM, zram and device-mapper devices,
// whose I/O is already counted on the underlying whole device.
func IncludeBlockDevice(name string) bool {
	for _, prefix := range []string{"loop", "ram", "zram", "dm-"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return name != ""
}

// ParseDiskstats sums read and written sectors of the devices in devices
// (whole devices from /sys/block) that IncludeBlockDevice accepts.
func ParseDiskstats(data []byte, devices map[string]bool) (DiskCounters, error) {
	var counters DiskCounters
	parsed := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		read, readErr := strconv.ParseUint(fields[5], 10, 64)
		written, writeErr := strconv.ParseUint(fields[9], 10, 64)
		if readErr != nil || writeErr != nil {
			return DiskCounters{}, fmt.Errorf("invalid counters for device %s", name)
		}
		parsed = true
		if !devices[name] || !IncludeBlockDevice(name) {
			continue
		}
		counters.ReadSectors += read
		counters.WriteSectors += written
	}
	if !parsed {
		return DiskCounters{}, fmt.Errorf("no devices in /proc/diskstats")
	}
	return counters, nil
}

// Delta returns cur-prev for a cumulative counter, or 0 when the counter
// wrapped or was reset, so deltas are never negative.
func Delta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}
