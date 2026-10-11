package takod

// Native /v1/metrics document (add-server-status-history design D2). The
// legacy fields keep the bash agent's names and types (percentages and load
// averages are strings, sizes are integer MB) so CLIs built before native
// collection decode it unchanged; everything else is additive.

import (
	"strconv"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

// NodeMetricsSource marks a document produced by the native collector.
const NodeMetricsSource = "takod"

type NodeMetricsMemory struct {
	TotalMB     int64  `json:"total_mb"`
	UsedMB      int64  `json:"used_mb"`
	AvailableMB int64  `json:"available_mb"`
	Percent     string `json:"percent"`
	SwapTotalMB int64  `json:"swap_total_mb"`
	SwapUsedMB  int64  `json:"swap_used_mb"`
}

type NodeMetricsDisk struct {
	Path          string `json:"path,omitempty"`
	TotalMB       int64  `json:"total_mb"`
	UsedMB        int64  `json:"used_mb"`
	AvailableMB   int64  `json:"available_mb"`
	Percent       string `json:"percent"`
	InodesPercent string `json:"inodes_percent,omitempty"`
}

type NodeMetricsLoad struct {
	OneMin     string `json:"1min"`
	FiveMin    string `json:"5min"`
	FifteenMin string `json:"15min"`
}

// NodeMetricsNetwork holds deltas since the previous sample plus rates and
// totals accumulated since takod started.
type NodeMetricsNetwork struct {
	RxBytes       int64   `json:"rx_bytes"`
	TxBytes       int64   `json:"tx_bytes"`
	RxMB          string  `json:"rx_mb"`
	TxMB          string  `json:"tx_mb"`
	RxBytesPerSec float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec"`
	RxBytesTotal  int64   `json:"rx_bytes_total"`
	TxBytesTotal  int64   `json:"tx_bytes_total"`
}

type NodeMetricsDiskIO struct {
	ReadSectors       int64   `json:"read_sectors"`
	WriteSectors      int64   `json:"write_sectors"`
	ReadMB            string  `json:"read_mb"`
	WriteMB           string  `json:"write_mb"`
	ReadBytesPerSec   float64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec  float64 `json:"write_bytes_per_sec"`
	ReadBytesTotal    int64   `json:"read_bytes_total"`
	WrittenBytesTotal int64   `json:"written_bytes_total"`
}

// NodeMetricsDocument is the `metrics` object of GET /v1/metrics when
// native collection is enabled.
type NodeMetricsDocument struct {
	Timestamp       string              `json:"timestamp"`
	CPUPercent      string              `json:"cpu_percent"`
	Memory          NodeMetricsMemory   `json:"memory"`
	Disk            NodeMetricsDisk     `json:"disk"`
	UptimeSeconds   int64               `json:"uptime_seconds"`
	LoadAverage     NodeMetricsLoad     `json:"load_average"`
	Network         *NodeMetricsNetwork `json:"network,omitempty"`
	DiskIO          *NodeMetricsDiskIO  `json:"disk_io,omitempty"`
	Source          string              `json:"source"`
	IntervalSeconds int64               `json:"interval_seconds"`
	CPUCount        int                 `json:"cpu_count,omitempty"`
	DockerDisk      *NodeMetricsDisk    `json:"docker_disk,omitempty"`
	Warnings        []string            `json:"warnings,omitempty"`
}

func formatPercent(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func formatMB(bytes uint64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 2, 64)
}

func clampInt64(value uint64) int64 {
	if value > 1<<62 {
		return 1 << 62
	}
	return int64(value)
}

func nodeMetricsDisk(fs *hostmetrics.Filesystem) NodeMetricsDisk {
	if fs == nil {
		return NodeMetricsDisk{Percent: "0.00"}
	}
	disk := NodeMetricsDisk{Path: fs.Path, TotalMB: fs.TotalMB, UsedMB: fs.UsedMB, AvailableMB: fs.AvailableMB, Percent: formatPercent(fs.Percent)}
	if fs.HasInodes {
		disk.InodesPercent = formatPercent(fs.InodesPercent)
	}
	return disk
}

// NativeMetricsDocument converts a native sample to the /v1/metrics schema.
func NativeMetricsDocument(sample hostmetrics.Sample, interval time.Duration) NodeMetricsDocument {
	loadText := func(text string, value float64) string {
		if text != "" {
			return text
		}
		return strconv.FormatFloat(value, 'f', 2, 64)
	}
	document := NodeMetricsDocument{
		Timestamp:  sample.Time.UTC().Format(time.RFC3339),
		CPUPercent: formatPercent(sample.CPUPercent),
		Memory: NodeMetricsMemory{
			TotalMB: sample.MemTotalMB, UsedMB: sample.MemUsedMB, AvailableMB: sample.MemAvailMB,
			Percent: formatPercent(sample.MemPercent), SwapTotalMB: sample.SwapTotalMB, SwapUsedMB: sample.SwapUsedMB,
		},
		Disk:          nodeMetricsDisk(sample.Root),
		UptimeSeconds: sample.UptimeSeconds,
		LoadAverage: NodeMetricsLoad{
			OneMin: loadText(sample.Load.OneText, sample.Load.One), FiveMin: loadText(sample.Load.FiveText, sample.Load.Five),
			FifteenMin: loadText(sample.Load.FifteenText, sample.Load.Fifteen),
		},
		Source:          NodeMetricsSource,
		IntervalSeconds: int64(interval / time.Second),
		CPUCount:        sample.CPUCount,
		Warnings:        append([]string(nil), sample.Warnings...),
	}
	if sample.Docker != nil {
		docker := nodeMetricsDisk(sample.Docker)
		document.DockerDisk = &docker
	}
	if io := sample.Network; io != nil {
		document.Network = &NodeMetricsNetwork{
			RxBytes: clampInt64(io.InBytes), TxBytes: clampInt64(io.OutBytes), RxMB: formatMB(io.InBytes), TxMB: formatMB(io.OutBytes),
			RxBytesPerSec: io.InRate, TxBytesPerSec: io.OutRate, RxBytesTotal: clampInt64(io.InTotal), TxBytesTotal: clampInt64(io.OutTotal),
		}
	}
	if io := sample.DiskIO; io != nil {
		document.DiskIO = &NodeMetricsDiskIO{
			ReadSectors: clampInt64(io.InSectors), WriteSectors: clampInt64(io.OutSectors), ReadMB: formatMB(io.InBytes), WriteMB: formatMB(io.OutBytes),
			ReadBytesPerSec: io.InRate, WriteBytesPerSec: io.OutRate, ReadBytesTotal: clampInt64(io.InTotal), WrittenBytesTotal: clampInt64(io.OutTotal),
		}
	}
	return document
}
