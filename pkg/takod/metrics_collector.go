package takod

// Native host metrics collector (add-server-status-history design D1). One
// goroutine samples every --metrics-interval, keeps the latest sample in
// memory for /v1/metrics, monitoring and node status, and appends it to the
// bounded history.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

// CapabilityNodeMetricsHistoryV1 means takod collects host metrics natively,
// fills the native /v1/metrics fields and serves GET /v1/metrics/history.
// It is advertised only when native collection is enabled.
const CapabilityNodeMetricsHistoryV1 = "node.metrics-history-v1"

const (
	metricsFirstSampleWindow = time.Second
	metricsCollectNowWindow  = time.Second
	// defaultMeshInterface is the default Tako WireGuard interface name;
	// its traffic is mesh overhead, not host network I/O.
	defaultMeshInterface = "tako"
)

// MetricsOptions configures native host metrics collection.
type MetricsOptions struct {
	// Interval is the sampling interval; 0 disables native collection and
	// keeps the legacy current.json read path.
	Interval        time.Duration
	Retention       time.Duration
	HistoryMaxBytes int64
	// TextfileDir, when set, receives tako_node.prom after each sample for
	// node_exporter's textfile collector.
	TextfileDir string
	// MeshInterfaces are excluded from network I/O in addition to "tako".
	MeshInterfaces []string
}

type hostSnapshotReader interface {
	Read(now time.Time) (hostmetrics.Snapshot, error)
}

type nodeMetricsCollector struct {
	reader   hostSnapshotReader
	interval time.Duration
	history  *metricsHistoryStore
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	// onSample runs after each periodic sample (node status, textfile).
	onSample func(hostmetrics.Sample)

	mu   sync.Mutex
	prev *hostmetrics.Snapshot
	// counterPrev is the newest snapshot folded into totals by either a
	// periodic tick or CollectNow, so exported *_total counters only ever
	// advance.
	counterPrev *hostmetrics.Snapshot
	totals      hostmetrics.Totals
	latest      *hostmetrics.Sample
	historyErr  error
	readErr     error
}

func newNodeMetricsCollector(reader hostSnapshotReader, interval time.Duration, history *metricsHistoryStore) *nodeMetricsCollector {
	return &nodeMetricsCollector{
		reader: reader, interval: interval, history: history,
		now:   func() time.Time { return time.Now().UTC() },
		sleep: hostmetrics.SleepContext,
	}
}

// Run takes the first sample over a 1s window, then one sample per tick.
// ticks may be nil to use a ticker at the collector interval.
func (c *nodeMetricsCollector) Run(ctx context.Context, ticks <-chan time.Time) {
	if c.history != nil {
		c.history.Prune(c.now())
	}
	if ticks == nil {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	c.First(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.Tick()
		}
	}
}

// First records the first sample, with CPU over a short window.
func (c *nodeMetricsCollector) First(ctx context.Context) {
	first, err := c.reader.Read(c.now())
	if err != nil {
		c.setReadError(err)
		return
	}
	c.mu.Lock()
	c.prev = &first
	c.advanceCountersLocked(first)
	c.mu.Unlock()
	if err := c.sleep(ctx, metricsFirstSampleWindow); err != nil {
		return
	}
	c.Tick()
}

// Tick reads one snapshot, computes the sample against the previous one,
// keeps it as the latest and appends it to history.
func (c *nodeMetricsCollector) Tick() {
	snapshot, err := c.reader.Read(c.now())
	if err != nil {
		c.setReadError(err)
		return
	}
	c.mu.Lock()
	sample := hostmetrics.Compute(c.prev, snapshot, nil)
	c.advanceCountersLocked(snapshot)
	applyTotals(&sample, c.totals)
	c.prev = &snapshot
	c.latest = &sample
	c.readErr = nil
	c.mu.Unlock()
	var historyErr error
	if c.history != nil {
		historyErr = c.history.Append(sample, sample.Time)
	}
	c.mu.Lock()
	c.historyErr = historyErr
	c.mu.Unlock()
	if c.onSample != nil {
		c.onSample(sample)
	}
}

func (c *nodeMetricsCollector) setReadError(err error) {
	c.mu.Lock()
	c.readErr = err
	c.mu.Unlock()
}

// Latest returns the newest periodic sample.
func (c *nodeMetricsCollector) Latest() (hostmetrics.Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil {
		return hostmetrics.Sample{}, false
	}
	return *c.latest, true
}

// Errors returns the last snapshot read error and history write error.
func (c *nodeMetricsCollector) Errors() (readErr, historyErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr, c.historyErr
}

// advanceCountersLocked folds the counter deltas from the newest snapshot
// already counted to snapshot into totals. Snapshots not newer than it
// (a CollectNow read that raced a tick) are ignored, so totals never count
// a window twice and never go backwards.
func (c *nodeMetricsCollector) advanceCountersLocked(snapshot hostmetrics.Snapshot) {
	if c.counterPrev != nil {
		if !snapshot.At.After(c.counterPrev.At) {
			return
		}
		hostmetrics.Compute(c.counterPrev, snapshot, &c.totals)
	}
	c.counterPrev = &snapshot
}

// applyTotals reports the collector's monotonic totals in sample.
func applyTotals(sample *hostmetrics.Sample, totals hostmetrics.Totals) {
	if sample.Network != nil {
		sample.Network.InTotal, sample.Network.OutTotal = totals.NetRx, totals.NetTx
	}
	if sample.DiskIO != nil {
		sample.DiskIO.InTotal, sample.DiskIO.OutTotal = totals.DiskRead, totals.DiskWrite
	}
}

// CollectNow takes a fresh sample over a 1s window without touching the
// periodic sample or history. Its rates cover the 1s window; its totals are
// the collector's totals advanced to the fresh read and kept in collector
// state, so *_total counters stay monotonic across collect=true requests and
// periodic samples.
func (c *nodeMetricsCollector) CollectNow(ctx context.Context) (hostmetrics.Sample, error) {
	first, err := c.reader.Read(c.now())
	if err != nil {
		return hostmetrics.Sample{}, err
	}
	if err := c.sleep(ctx, metricsCollectNowWindow); err != nil {
		return hostmetrics.Sample{}, err
	}
	if err := ctx.Err(); err != nil {
		return hostmetrics.Sample{}, err
	}
	second, err := c.reader.Read(c.now())
	if err != nil {
		return hostmetrics.Sample{}, err
	}
	sample := hostmetrics.Compute(&first, second, nil)
	c.mu.Lock()
	c.advanceCountersLocked(first)
	c.advanceCountersLocked(second)
	applyTotals(&sample, c.totals)
	c.mu.Unlock()
	return sample, nil
}

// nativeHostSampleSource implements monitoring's HostSampleSource over the
// collector's latest sample.
type nativeHostSampleSource struct {
	collector *nodeMetricsCollector
}

var errNoNativeSample = errors.New("no native host sample collected yet")

func hostFilesystemSample(fs *hostmetrics.Filesystem) HostFilesystemSample {
	if fs == nil {
		return HostFilesystemSample{}
	}
	return HostFilesystemSample{UsedPercent: fs.Percent, UsedMB: fs.UsedMB, TotalMB: fs.TotalMB, InodesPercent: fs.InodesPercent, HasInodes: fs.HasInodes}
}

func (s nativeHostSampleSource) Latest() (HostSample, time.Time, error) {
	sample, ok := s.collector.Latest()
	if !ok {
		return HostSample{}, time.Time{}, errNoNativeSample
	}
	host := HostSample{
		CPUPercent: sample.CPUPercent, MemoryPercent: sample.MemPercent, MemoryUsedMB: sample.MemUsedMB, MemoryTotalMB: sample.MemTotalMB,
		Load5: sample.Load.Five, CPUCount: sample.CPUCount, Root: hostFilesystemSample(sample.Root),
	}
	if sample.Docker != nil {
		docker := hostFilesystemSample(sample.Docker)
		host.Docker = &docker
	}
	return host, sample.Time, nil
}
