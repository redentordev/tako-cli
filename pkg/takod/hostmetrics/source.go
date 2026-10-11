package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// FilesystemUsage is one statfs result.
type FilesystemUsage struct {
	Path string
	// ID is the statfs filesystem ID and Device the device identity (st_dev,
	// or the mount source on btrfs). Two paths matching on either are the
	// same filesystem and reported once.
	ID            string
	Device        string
	TotalBytes    uint64
	FreeBytes     uint64
	AvailBytes    uint64
	Inodes        uint64
	InodesFree    uint64
	HasInodeCount bool
}

// StatfsFunc returns filesystem usage for a path.
type StatfsFunc func(path string) (FilesystemUsage, error)

// Source reads raw counters and gauges. ProcRoot and SysRoot are injectable
// so tests can use fixture trees.
type Source struct {
	ProcRoot   string
	SysRoot    string
	RootPath   string
	DockerRoot string
	Interfaces InterfaceFilter
	Statfs     StatfsFunc
}

// NewSource returns a Source over the real /proc and /sys.
func NewSource(dockerRoot string, meshInterfaces ...string) *Source {
	return &Source{
		ProcRoot: "/proc", SysRoot: "/sys", RootPath: "/", DockerRoot: dockerRoot,
		Interfaces: DefaultInterfaceFilter(meshInterfaces...), Statfs: Statfs,
	}
}

// Snapshot is one raw read: cumulative counters plus point-in-time gauges.
type Snapshot struct {
	At       time.Time
	CPU      CPUTimes
	CPUCount int
	Memory   Memory
	Load     Load
	Uptime   int64
	Net      *NetCounters
	Disk     *DiskCounters
	Root     *FilesystemUsage
	Docker   *FilesystemUsage
	Warnings []string
}

// Read takes one snapshot. CPU and memory are required; every other source
// is optional and recorded as a warning when unreadable.
func (s *Source) Read(now time.Time) (Snapshot, error) {
	snapshot := Snapshot{At: now}
	data, err := os.ReadFile(filepath.Join(s.ProcRoot, "stat"))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	if snapshot.CPU, snapshot.CPUCount, err = ParseCPUStat(data); err != nil {
		return Snapshot{}, err
	}
	if data, err = os.ReadFile(filepath.Join(s.ProcRoot, "meminfo")); err != nil {
		return Snapshot{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	if snapshot.Memory, err = ParseMeminfo(data); err != nil {
		return Snapshot{}, err
	}
	warn := func(source string, err error) {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("%s: %v", source, err))
	}
	if data, err = os.ReadFile(filepath.Join(s.ProcRoot, "loadavg")); err != nil {
		warn("loadavg", err)
	} else if snapshot.Load, err = ParseLoadavg(data); err != nil {
		warn("loadavg", err)
	}
	if data, err = os.ReadFile(filepath.Join(s.ProcRoot, "uptime")); err != nil {
		warn("uptime", err)
	} else if snapshot.Uptime, err = ParseUptime(data); err != nil {
		warn("uptime", err)
	}
	if data, err = os.ReadFile(filepath.Join(s.ProcRoot, "net", "dev")); err != nil {
		warn("net", err)
	} else if counters, parseErr := ParseNetDev(data, s.Interfaces); parseErr != nil {
		warn("net", parseErr)
	} else {
		snapshot.Net = &counters
	}
	if devices, listErr := s.blockDevices(); listErr != nil {
		warn("diskstats", listErr)
	} else if data, err = os.ReadFile(filepath.Join(s.ProcRoot, "diskstats")); err != nil {
		warn("diskstats", err)
	} else if counters, parseErr := ParseDiskstats(data, devices); parseErr != nil {
		warn("diskstats", parseErr)
	} else {
		snapshot.Disk = &counters
	}
	if s.Statfs != nil {
		rootPath := s.RootPath
		if rootPath == "" {
			rootPath = "/"
		}
		if usage, statErr := s.Statfs(rootPath); statErr != nil {
			warn("statfs "+rootPath, statErr)
		} else {
			usage.Path = rootPath
			snapshot.Root = &usage
		}
		if s.DockerRoot != "" {
			if usage, statErr := s.Statfs(s.DockerRoot); statErr != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					warn("statfs "+s.DockerRoot, statErr)
				}
			} else if snapshot.Root == nil || !usage.SameFilesystem(*snapshot.Root) {
				usage.Path = s.DockerRoot
				snapshot.Docker = &usage
			}
		}
	}
	return snapshot, nil
}

func (s *Source) blockDevices() (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(s.SysRoot, "block"))
	if err != nil {
		return nil, err
	}
	devices := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if IncludeBlockDevice(entry.Name()) {
			devices[entry.Name()] = true
		}
	}
	return devices, nil
}

// Filesystem is a filesystem's usage in a sample.
type Filesystem struct {
	Path          string
	TotalMB       int64
	UsedMB        int64
	AvailableMB   int64
	Percent       float64
	InodesPercent float64
	HasInodes     bool
}

// IO is one direction pair of byte counters: deltas over the sample
// window, per-second rates, and totals accumulated since the collector
// started.
type IO struct {
	InBytes, OutBytes     uint64
	InRate, OutRate       float64
	InTotal, OutTotal     uint64
	WindowSeconds         float64
	InSectors, OutSectors uint64
}

// Sample is one computed host metrics sample.
type Sample struct {
	Time          time.Time
	CPUPercent    float64
	CPUCount      int
	MemTotalMB    int64
	MemUsedMB     int64
	MemAvailMB    int64
	MemPercent    float64
	SwapTotalMB   int64
	SwapUsedMB    int64
	Load          Load
	UptimeSeconds int64
	Root          *Filesystem
	Docker        *Filesystem
	// Network is in (receive) and out (transmit).
	Network *IO
	// DiskIO is in (read) and out (written).
	DiskIO   *IO
	Warnings []string
}

// Totals accumulates counter deltas across samples.
type Totals struct {
	NetRx, NetTx, DiskRead, DiskWrite uint64
}

// Compute derives a sample from two snapshots. prev may be nil, in which
// case CPU is 0 and I/O is absent. totals, when non-nil, is advanced by the
// I/O deltas and reported in the sample.
func Compute(prev *Snapshot, cur Snapshot, totals *Totals) Sample {
	sample := Sample{
		Time: cur.At, CPUCount: cur.CPUCount, Load: cur.Load, UptimeSeconds: cur.Uptime,
		Warnings: append([]string(nil), cur.Warnings...),
	}
	memory := cur.Memory
	used := memory.TotalKB - memory.AvailableKB
	sample.MemTotalMB = int64(memory.TotalKB / 1024)
	sample.MemUsedMB = int64(used / 1024)
	sample.MemAvailMB = int64(memory.AvailableKB / 1024)
	sample.MemPercent = round2(float64(used) * 100 / float64(memory.TotalKB))
	sample.SwapTotalMB = int64(memory.SwapTotalKB / 1024)
	sample.SwapUsedMB = int64((memory.SwapTotalKB - memory.SwapFreeKB) / 1024)
	sample.Root = filesystem(cur.Root)
	sample.Docker = filesystem(cur.Docker)
	if prev == nil {
		return sample
	}
	window := cur.At.Sub(prev.At).Seconds()
	totalDelta := Delta(cur.CPU.Total, prev.CPU.Total)
	busyDelta := Delta(cur.CPU.Busy, prev.CPU.Busy)
	if totalDelta > 0 {
		if busyDelta > totalDelta {
			busyDelta = totalDelta
		}
		sample.CPUPercent = round2(float64(busyDelta) * 100 / float64(totalDelta))
	}
	if window <= 0 {
		return sample
	}
	if totals == nil {
		totals = &Totals{}
	}
	if cur.Net != nil && prev.Net != nil {
		rx, tx := Delta(cur.Net.RxBytes, prev.Net.RxBytes), Delta(cur.Net.TxBytes, prev.Net.TxBytes)
		totals.NetRx += rx
		totals.NetTx += tx
		sample.Network = &IO{InBytes: rx, OutBytes: tx, InRate: round2(float64(rx) / window), OutRate: round2(float64(tx) / window), InTotal: totals.NetRx, OutTotal: totals.NetTx, WindowSeconds: window}
	}
	if cur.Disk != nil && prev.Disk != nil {
		readSectors, writeSectors := Delta(cur.Disk.ReadSectors, prev.Disk.ReadSectors), Delta(cur.Disk.WriteSectors, prev.Disk.WriteSectors)
		read, written := readSectors*512, writeSectors*512
		totals.DiskRead += read
		totals.DiskWrite += written
		sample.DiskIO = &IO{InBytes: read, OutBytes: written, InRate: round2(float64(read) / window), OutRate: round2(float64(written) / window), InTotal: totals.DiskRead, OutTotal: totals.DiskWrite, WindowSeconds: window, InSectors: readSectors, OutSectors: writeSectors}
	}
	return sample
}

func filesystem(usage *FilesystemUsage) *Filesystem {
	if usage == nil || usage.TotalBytes == 0 {
		return nil
	}
	free := usage.FreeBytes
	if free > usage.TotalBytes {
		free = usage.TotalBytes
	}
	used := usage.TotalBytes - free
	fs := &Filesystem{
		Path: usage.Path, TotalMB: int64(usage.TotalBytes >> 20), UsedMB: int64(used >> 20), AvailableMB: int64(usage.AvailBytes >> 20),
	}
	// df semantics: used / (used + available to unprivileged users).
	if denominator := used + usage.AvailBytes; denominator > 0 {
		fs.Percent = round2(float64(used) * 100 / float64(denominator))
	}
	if usage.HasInodeCount && usage.Inodes > 0 {
		inodesFree := usage.InodesFree
		if inodesFree > usage.Inodes {
			inodesFree = usage.Inodes
		}
		fs.InodesPercent = round2(float64(usage.Inodes-inodesFree) * 100 / float64(usage.Inodes))
		fs.HasInodes = true
	}
	return fs
}

func round2(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

// SampleOver reads two snapshots window apart and computes a sample over
// that window (used for the first sample and for collect=true). sleep is
// injectable for tests; nil uses a context-aware timer.
func (s *Source) SampleOver(ctx context.Context, window time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) (Sample, Snapshot, error) {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = SleepContext
	}
	first, err := s.Read(now().UTC())
	if err != nil {
		return Sample{}, Snapshot{}, err
	}
	if err := sleep(ctx, window); err != nil {
		return Sample{}, Snapshot{}, err
	}
	second, err := s.Read(now().UTC())
	if err != nil {
		return Sample{}, Snapshot{}, err
	}
	return Compute(&first, second, nil), second, nil
}

// SleepContext sleeps for d or until ctx is done.
func SleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
