package hostmetrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "proc", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseCPUStatFixture(t *testing.T) {
	times, cpus, err := ParseCPUStat(readFixture(t, "stat"))
	if err != nil {
		t.Fatal(err)
	}
	if times.Total != 95500 || times.Busy != 14000 || cpus != 2 {
		t.Fatalf("times = %+v cpus = %d", times, cpus)
	}
	if _, _, err := ParseCPUStat([]byte("intr 1\n")); err == nil {
		t.Fatal("expected error without aggregate cpu line")
	}
	if _, _, err := ParseCPUStat([]byte("cpu 1 2 x 4 5\n")); err == nil {
		t.Fatal("expected error for invalid value")
	}
}

func TestParseMeminfoFixture(t *testing.T) {
	memory, err := ParseMeminfo(readFixture(t, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if memory.TotalKB != 4014080 || memory.AvailableKB != 3006464 || memory.SwapTotalKB != 2097148 || memory.SwapFreeKB != 1048572 {
		t.Fatalf("memory = %+v", memory)
	}
	if _, err := ParseMeminfo([]byte("MemTotal: 10 kB\n")); err == nil {
		t.Fatal("expected error without MemAvailable")
	}
}

func TestParseLoadavgAndUptimeFixtures(t *testing.T) {
	load, err := ParseLoadavg(readFixture(t, "loadavg"))
	if err != nil || load.One != 0.41 || load.Five != 0.38 || load.Fifteen != 0.30 || load.FifteenText != "0.30" {
		t.Fatalf("load = %+v err = %v", load, err)
	}
	uptime, err := ParseUptime(readFixture(t, "uptime"))
	if err != nil || uptime != 86400 {
		t.Fatalf("uptime = %d err = %v", uptime, err)
	}
	if _, err := ParseLoadavg([]byte("0.1 0.2\n")); err == nil {
		t.Fatal("expected short loadavg error")
	}
}

func TestParseNetDevExcludesVirtualInterfaces(t *testing.T) {
	counters, err := ParseNetDev(readFixture(t, "net/dev"), DefaultInterfaceFilter("tako"))
	if err != nil {
		t.Fatal(err)
	}
	// eth0 + ens5 only: lo, docker0, veth*, br-* and the mesh are excluded.
	if counters.RxBytes != 1200000 || counters.TxBytes != 600000 {
		t.Fatalf("counters = %+v", counters)
	}
	onlyEth0, err := ParseNetDev([]byte("Inter-|\n face |\n  lo: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n  eth0: 7 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n"), DefaultInterfaceFilter())
	if err != nil || onlyEth0.RxBytes != 7 || onlyEth0.TxBytes != 9 {
		t.Fatalf("eth0 counters = %+v err = %v", onlyEth0, err)
	}
}

func TestParseDiskstatsCountsWholeDevicesOnly(t *testing.T) {
	devices := map[string]bool{"sda": true, "nvme0n1": true, "loop0": true, "dm-0": true, "ram0": true}
	counters, err := ParseDiskstats(readFixture(t, "diskstats"), devices)
	if err != nil {
		t.Fatal(err)
	}
	if counters.ReadSectors != 50000 || counters.WriteSectors != 100000 {
		t.Fatalf("counters = %+v", counters)
	}
	for _, name := range []string{"loop0", "ram1", "zram0", "dm-3"} {
		if IncludeBlockDevice(name) {
			t.Fatalf("%s should be excluded", name)
		}
	}
}

func TestDeltaNeverNegative(t *testing.T) {
	if Delta(5, 10) != 0 || Delta(10, 5) != 5 {
		t.Fatal("unexpected delta")
	}
}

func fixtureSource(t *testing.T) (*Source, string) {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stat", "meminfo", "loadavg", "uptime", "net/dev", "diskstats"} {
		if err := os.WriteFile(filepath.Join(proc, filepath.FromSlash(name)), readFixture(t, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, device := range []string{"sda", "nvme0n1", "loop0", "dm-0", "ram0"} {
		if err := os.MkdirAll(filepath.Join(root, "sys", "block", device), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source := &Source{
		ProcRoot: proc, SysRoot: filepath.Join(root, "sys"), RootPath: "/", DockerRoot: "/var/lib/docker",
		Interfaces: DefaultInterfaceFilter("tako"),
		Statfs: func(path string) (FilesystemUsage, error) {
			return FilesystemUsage{ID: "root", TotalBytes: 100 << 30, FreeBytes: 40 << 30, AvailBytes: 35 << 30, Inodes: 1000, InodesFree: 900, HasInodeCount: true}, nil
		},
	}
	return source, proc
}

func TestSourceReadAndComputeFromFixtures(t *testing.T) {
	source, proc := fixtureSource(t)
	start := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	first, err := source.Read(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Warnings) != 0 {
		t.Fatalf("warnings = %v", first.Warnings)
	}
	if first.Docker != nil {
		t.Fatal("docker root on the same filesystem must be deduplicated")
	}
	// Advance counters: +1000 ticks with 250 busy, +60000 rx, +12 sectors.
	stat := "cpu  10200 500 3050 80700 1550 200 300 0 0 0\ncpu0 1 1 1 1\n"
	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	net := "Inter-|\n face |\n  eth0: 1060000 0 0 0 0 0 0 0 530000 0 0 0 0 0 0 0\n  ens5: 200000 0 0 0 0 0 0 0 100000 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "dev"), []byte(net), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := source.Read(start.Add(60 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	totals := &Totals{}
	sample := Compute(&first, second, totals)
	if sample.CPUPercent != 25 {
		t.Fatalf("cpu = %v", sample.CPUPercent)
	}
	if sample.MemTotalMB != 3920 || sample.MemUsedMB != 984 || sample.MemAvailMB != 2936 || sample.MemPercent != 25.1 || sample.SwapTotalMB != 2047 || sample.SwapUsedMB != 1024 {
		t.Fatalf("memory = %+v", sample)
	}
	if sample.Network == nil || sample.Network.InBytes != 60000 || sample.Network.OutBytes != 30000 || sample.Network.InRate != 1000 || sample.Network.InTotal != 60000 {
		t.Fatalf("network = %+v", sample.Network)
	}
	if sample.DiskIO == nil || sample.DiskIO.InBytes != 0 || sample.DiskIO.OutBytes != 0 {
		t.Fatalf("disk io = %+v", sample.DiskIO)
	}
	if sample.Root == nil || sample.Root.TotalMB != 102400 || sample.Root.UsedMB != 61440 || sample.Root.Percent != 63.16 || sample.Root.InodesPercent != 10 {
		t.Fatalf("root = %+v", sample.Root)
	}
	if sample.UptimeSeconds != 86400 || sample.CPUCount != 1 {
		t.Fatalf("uptime/cpus = %d/%d", sample.UptimeSeconds, sample.CPUCount)
	}
}

func TestComputeCounterResetProducesNoNegativeDeltas(t *testing.T) {
	prev := Snapshot{At: time.Unix(0, 0), CPU: CPUTimes{Total: 1000, Busy: 500}, Memory: Memory{TotalKB: 1024, AvailableKB: 512}, Net: &NetCounters{RxBytes: 1 << 40, TxBytes: 100}, Disk: &DiskCounters{ReadSectors: 50, WriteSectors: 50}}
	cur := Snapshot{At: time.Unix(60, 0), CPU: CPUTimes{Total: 10, Busy: 5}, Memory: Memory{TotalKB: 1024, AvailableKB: 512}, Net: &NetCounters{RxBytes: 10, TxBytes: 200}, Disk: &DiskCounters{ReadSectors: 1, WriteSectors: 60}}
	sample := Compute(&prev, cur, nil)
	if sample.CPUPercent != 0 || sample.Network.InBytes != 0 || sample.Network.OutBytes != 100 || sample.DiskIO.InBytes != 0 || sample.DiskIO.OutBytes != 10*512 {
		t.Fatalf("sample = %+v net=%+v disk=%+v", sample, sample.Network, sample.DiskIO)
	}
}

func TestSourceSeparateDockerFilesystem(t *testing.T) {
	source, _ := fixtureSource(t)
	source.Statfs = func(path string) (FilesystemUsage, error) {
		if path == "/var/lib/docker" {
			return FilesystemUsage{ID: "docker", TotalBytes: 200 << 30, FreeBytes: 20 << 30, AvailBytes: 20 << 30, Inodes: 100, InodesFree: 8, HasInodeCount: true}, nil
		}
		return FilesystemUsage{ID: "root", TotalBytes: 100 << 30, FreeBytes: 50 << 30, AvailBytes: 50 << 30}, nil
	}
	snapshot, err := source.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sample := Compute(nil, snapshot, nil)
	if sample.Docker == nil || sample.Docker.Path != "/var/lib/docker" || sample.Docker.InodesPercent != 92 || sample.Docker.Percent != 90 {
		t.Fatalf("docker = %+v", sample.Docker)
	}
	if sample.Root.HasInodes {
		t.Fatal("root without inode counts must not report inodes")
	}
	if sample.Network != nil || sample.CPUPercent != 0 {
		t.Fatal("first sample without previous snapshot has no I/O or CPU")
	}
}

func TestSourceSameFilesystemMatchesOnEitherIdentity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		root, docker FilesystemUsage
		separate     bool
	}{
		{"same fsid, other device", FilesystemUsage{ID: "fsid:1:2", Device: "dev:10"}, FilesystemUsage{ID: "fsid:1:2", Device: "dev:11"}, false},
		{"btrfs subvolumes share the mount source", FilesystemUsage{ID: "fsid:1:2", Device: "btrfs:/dev/sda2"}, FilesystemUsage{ID: "fsid:1:7", Device: "btrfs:/dev/sda2"}, false},
		{"different filesystems", FilesystemUsage{ID: "fsid:1:2", Device: "dev:10"}, FilesystemUsage{ID: "fsid:3:4", Device: "dev:11"}, true},
		{"no identity", FilesystemUsage{}, FilesystemUsage{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, _ := fixtureSource(t)
			source.Statfs = func(path string) (FilesystemUsage, error) {
				if path == "/var/lib/docker" {
					return tc.docker, nil
				}
				return tc.root, nil
			}
			snapshot, err := source.Read(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if (snapshot.Docker != nil) != tc.separate {
				t.Fatalf("docker reported separately = %v, want %v", snapshot.Docker != nil, tc.separate)
			}
		})
	}
}

func TestMountSourceFor(t *testing.T) {
	mountinfo := []byte(`22 1 0:21 / / rw,relatime shared:1 - btrfs /dev/sda2 rw,subvol=/@
23 22 0:22 / /proc rw - proc proc rw
30 22 0:35 /@docker /var/lib/docker rw,relatime shared:5 - btrfs /dev/sda2 rw,subvol=/@docker
31 22 0:40 / /mnt/my\040data rw - ext4 /dev/sdb1 rw
`)
	for path, want := range map[string]string{
		"/":                        "/dev/sda2",
		"/var/lib/docker":          "/dev/sda2",
		"/var/lib/docker/overlay2": "/dev/sda2",
		"/var/lib/dockerish":       "/dev/sda2",
		"/mnt/my data/x":           "/dev/sdb1",
		"/proc/self":               "proc",
	} {
		if got := mountSourceFor(mountinfo, path); got != want {
			t.Fatalf("mountSourceFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSourceMissingOptionalFilesWarn(t *testing.T) {
	source, proc := fixtureSource(t)
	_ = os.Remove(filepath.Join(proc, "diskstats"))
	source.SysRoot = filepath.Join(t.TempDir(), "missing")
	source.Statfs = func(string) (FilesystemUsage, error) { return FilesystemUsage{}, errors.New("denied") }
	snapshot, err := source.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Disk != nil || snapshot.Root != nil || len(snapshot.Warnings) < 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	_ = os.Remove(filepath.Join(proc, "meminfo"))
	if _, err := source.Read(time.Now()); err == nil {
		t.Fatal("missing meminfo must fail the read")
	}
}

func TestSampleOverRespectsContext(t *testing.T) {
	source, _ := fixtureSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := source.SampleOver(ctx, time.Second, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample, _, err := source.SampleOver(context.Background(), time.Second, func() time.Time { clock = clock.Add(time.Second); return clock }, func(context.Context, time.Duration) error { return nil })
	if err != nil || sample.Network == nil || sample.Network.WindowSeconds != 1 {
		t.Fatalf("sample = %+v err = %v", sample, err)
	}
}
