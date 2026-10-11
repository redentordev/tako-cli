package takod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

// fakeSnapshotReader returns counters that advance by a fixed amount on
// every read.
type fakeSnapshotReader struct {
	mu    sync.Mutex
	reads int
	err   error
}

func (r *fakeSnapshotReader) Read(now time.Time) (hostmetrics.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return hostmetrics.Snapshot{}, r.err
	}
	r.reads++
	step := uint64(r.reads)
	return hostmetrics.Snapshot{
		At: now, CPUCount: 2, Uptime: 3600 + int64(step)*60,
		CPU:    hostmetrics.CPUTimes{Total: step * 1000, Busy: step * 250},
		Memory: hostmetrics.Memory{TotalKB: 4 << 20, AvailableKB: 3 << 20, SwapTotalKB: 1 << 20, SwapFreeKB: 1 << 20},
		Load:   hostmetrics.Load{One: 0.5, Five: 0.4, Fifteen: 0.3, OneText: "0.50", FiveText: "0.40", FifteenText: "0.30"},
		Net:    &hostmetrics.NetCounters{RxBytes: step * 6000, TxBytes: step * 3000},
		Disk:   &hostmetrics.DiskCounters{ReadSectors: step * 10, WriteSectors: step * 20},
		Root:   &hostmetrics.FilesystemUsage{Path: "/", ID: "root", TotalBytes: 100 << 30, FreeBytes: 60 << 30, AvailBytes: 60 << 30, Inodes: 100, InodesFree: 90, HasInodeCount: true},
		Docker: &hostmetrics.FilesystemUsage{Path: "/var/lib/docker", ID: "docker", TotalBytes: 50 << 30, FreeBytes: 5 << 30, AvailBytes: 5 << 30, Inodes: 100, InodesFree: 8, HasInodeCount: true},
	}, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newFakeCollector(t *testing.T, reader *fakeSnapshotReader, clock *fakeClock) *nodeMetricsCollector {
	t.Helper()
	history := newMetricsHistoryStore(filepath.Join(t.TempDir(), "history"), DefaultMetricsRetention, DefaultMetricsHistoryMaxBytes, time.Minute)
	collector := newNodeMetricsCollector(reader, time.Minute, history)
	collector.now = clock.Now
	collector.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.Advance(d)
		return nil
	}
	return collector
}

func TestCollectorOneSamplePerTick(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
	reader := &fakeSnapshotReader{}
	collector := newFakeCollector(t, reader, clock)
	samples := make(chan hostmetrics.Sample, 8)
	collector.onSample = func(sample hostmetrics.Sample) { samples <- sample }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		collector.Run(ctx, ticks)
		close(done)
	}()
	first := <-samples
	if first.CPUPercent != 25 || first.Network == nil || first.Network.WindowSeconds != 1 {
		t.Fatalf("first sample = %+v network = %+v", first, first.Network)
	}
	for index := 0; index < 2; index++ {
		clock.Advance(time.Minute)
		ticks <- clock.Now()
		sample := <-samples
		latest, ok := collector.Latest()
		if !ok || !latest.Time.Equal(sample.Time) || !latest.Time.Equal(clock.Now()) {
			t.Fatalf("latest = %v, sample = %v", latest.Time, sample.Time)
		}
		if sample.Network.InBytes != 6000 || sample.Network.InRate != 100 || sample.DiskIO.OutBytes != 20*512 {
			t.Fatalf("tick sample = %+v %+v", sample.Network, sample.DiskIO)
		}
	}
	cancel()
	<-done
	if reader.reads != 4 {
		t.Fatalf("reads = %d, want 2 for the first sample + 1 per tick", reader.reads)
	}
	latest, _ := collector.Latest()
	if latest.Network.InTotal != 18000 {
		t.Fatalf("network total = %d", latest.Network.InTotal)
	}
	records := collector.history.readRange(clock.Now().Add(-time.Hour), clock.Now().Add(time.Second))
	if len(records) != 3 {
		t.Fatalf("history records = %d, want 3", len(records))
	}
}

func TestCollectorCollectNowDoesNotTouchHistoryAndRespectsContext(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
	reader := &fakeSnapshotReader{}
	collector := newFakeCollector(t, reader, clock)
	sample, err := collector.CollectNow(context.Background())
	if err != nil || sample.CPUPercent != 25 {
		t.Fatalf("sample = %+v err = %v", sample, err)
	}
	if _, ok := collector.Latest(); ok {
		t.Fatal("CollectNow must not replace the periodic sample")
	}
	if segments := collector.history.segments(); len(segments) != 0 {
		t.Fatalf("CollectNow appended history: %+v", segments)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collector.CollectNow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// scriptedSnapshotReader returns snapshots whose network receive counter
// follows rx, one value per read.
type scriptedSnapshotReader struct {
	mu sync.Mutex
	rx []uint64
}

func (r *scriptedSnapshotReader) Read(now time.Time) (hostmetrics.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.rx[0]
	if len(r.rx) > 1 {
		r.rx = r.rx[1:]
	}
	return hostmetrics.Snapshot{
		At: now, CPUCount: 1, CPU: hostmetrics.CPUTimes{Total: 1000, Busy: 10},
		Memory: hostmetrics.Memory{TotalKB: 1 << 20, AvailableKB: 1 << 19},
		Net:    &hostmetrics.NetCounters{RxBytes: value, TxBytes: value},
		Disk:   &hostmetrics.DiskCounters{ReadSectors: value, WriteSectors: value},
	}, nil
}

func TestCollectorTotalsMonotonicAcrossCollectNow(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
	// First (0 -> 100), CollectNow A (100 -> 1100), CollectNow B
	// (1100 -> 1110), Tick (1200).
	reader := &scriptedSnapshotReader{rx: []uint64{0, 100, 100, 1100, 1100, 1110, 1200}}
	collector := newNodeMetricsCollector(reader, time.Minute, nil)
	collector.now = clock.Now
	collector.sleep = func(_ context.Context, d time.Duration) error { clock.Advance(d); return nil }
	collector.First(context.Background())
	var totals []uint64
	latest, _ := collector.Latest()
	totals = append(totals, latest.Network.InTotal)
	for i := 0; i < 2; i++ {
		clock.Advance(10 * time.Second)
		sample, err := collector.CollectNow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		totals = append(totals, sample.Network.InTotal)
	}
	clock.Advance(time.Minute)
	collector.Tick()
	latest, _ = collector.Latest()
	totals = append(totals, latest.Network.InTotal)
	if !slices.Equal(totals, []uint64{100, 1100, 1110, 1200}) {
		t.Fatalf("network totals = %v, want monotonic [100 1100 1110 1200]", totals)
	}
	if latest.Network.InBytes != 1100 {
		t.Fatalf("periodic window bytes = %d, want 1100 since the previous tick", latest.Network.InBytes)
	}
	if latest.DiskIO.InTotal != 1200*512 {
		t.Fatalf("disk read total = %d", latest.DiskIO.InTotal)
	}
}

func TestCollectorHistoryWriteFailureKeepsLatest(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
	collector := newFakeCollector(t, &fakeSnapshotReader{}, clock)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	collector.history.dir = filepath.Join(blocker, "history")
	collector.Tick()
	if _, ok := collector.Latest(); !ok {
		t.Fatal("latest sample must be served from memory")
	}
	if _, historyErr := collector.Errors(); historyErr == nil {
		t.Fatal("expected a history error")
	}
	status := newNodeStatusManager(t.TempDir(), nil, nil, func() (time.Time, bool, error, error, []string) {
		latest, ok := collector.Latest()
		readErr, historyErr := collector.Errors()
		return latest.Time, ok, readErr, historyErr, nil
	}, time.Minute)
	status.now = clock.Now
	status.UpdateCollector()
	condition := status.doc.Conditions[NodeConditionMetricsCollector]
	if condition.Status != NodeConditionWarn || condition.Reason != metricsHistoryReasonWriteError {
		t.Fatalf("condition = %+v", condition)
	}
}

func TestNativeHostSampleSourceFeedsMonitoring(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
	collector := newFakeCollector(t, &fakeSnapshotReader{}, clock)
	source := nativeHostSampleSource{collector: collector}
	if _, _, err := source.Latest(); err == nil {
		t.Fatal("expected an error before the first sample")
	}
	if !hostMetricsStale(source, clock.Now()) {
		t.Fatal("no sample must be stale")
	}
	collector.Tick()
	sample, at, err := source.Latest()
	if err != nil || !at.Equal(clock.Now()) {
		t.Fatalf("at = %v err = %v", at, err)
	}
	if sample.Docker == nil || sample.Docker.InodesPercent != 92 || !sample.Docker.HasInodes || sample.Root.InodesPercent != 10 || sample.CPUCount != 2 || sample.MemoryPercent != 25 {
		t.Fatalf("sample = %+v docker = %+v", sample, sample.Docker)
	}
	clock.Advance(10 * time.Minute)
	if !hostMetricsStale(source, clock.Now()) {
		t.Fatal("metrics_stale must follow the native sample time")
	}
}

func TestServerUsesNativeSourceOnlyWhenEnabled(t *testing.T) {
	native := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{Metrics: MetricsOptions{Interval: time.Minute}})
	if _, ok := native.hostSamples.(nativeHostSampleSource); !ok {
		t.Fatalf("native collection must feed monitoring, got %T", native.hostSamples)
	}
	if !slices.Contains(native.Status().Capabilities, CapabilityNodeMetricsHistoryV1) || !slices.Contains(native.Status().Capabilities, CapabilityNodeStatusV1) {
		t.Fatalf("capabilities = %v", native.Status().Capabilities)
	}
	legacy := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	if _, ok := legacy.hostSamples.(legacyHostSampleSource); !ok {
		t.Fatalf("--metrics-interval 0 must keep the legacy adapter, got %T", legacy.hostSamples)
	}
	if slices.Contains(legacy.Status().Capabilities, CapabilityNodeMetricsHistoryV1) || !slices.Contains(legacy.Status().Capabilities, CapabilityNodeStatusV1) {
		t.Fatalf("legacy capabilities = %v", legacy.Status().Capabilities)
	}
}

func nativeTestServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{NodeName: "app-1", Metrics: MetricsOptions{Interval: time.Minute}})
	clock := &fakeClock{now: time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)}
	collector := newFakeCollector(t, &fakeSnapshotReader{}, clock)
	server.statusHistory.collector = collector
	server.statusHistory.history = collector.history
	return server, clock
}

func TestHandleMetricsNativeDocument(t *testing.T) {
	server, clock := nativeTestServer(t)
	server.statusHistory.collector.First(context.Background())
	clock.Advance(time.Minute)
	server.statusHistory.collector.Tick()
	recorder := httptest.NewRecorder()
	server.handleMetrics(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response MetricsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(response.Metrics, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics["source"] != "takod" || metrics["cpu_percent"] != "25.00" || response.Collected {
		t.Fatalf("metrics = %v", metrics)
	}
	network := metrics["network"].(map[string]any)
	if network["rx_bytes"] != float64(6000) || network["rx_mb"] != "0.01" {
		t.Fatalf("network = %v", network)
	}
	recorder = httptest.NewRecorder()
	server.handleMetrics(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics?collect=true", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.Collected {
		t.Fatalf("collect response = %s", recorder.Body.String())
	}
	if records := server.statusHistory.history.readRange(clock.Now().Add(-time.Hour), clock.Now().Add(time.Hour)); len(records) != 2 {
		t.Fatalf("collect=true appended history: %d records", len(records))
	}
}

func TestHandleMetricsLegacyModeReadsCurrentJSON(t *testing.T) {
	restore := useTempMetricsFile(t, `{"cpu_percent":"12.5","uptime_seconds":123}`+"\n")
	defer restore()
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	recorder := httptest.NewRecorder()
	server.handleMetrics(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"cpu_percent": "12.5"`) || strings.Contains(recorder.Body.String(), `"source"`) {
		t.Fatalf("legacy response = %d %s", recorder.Code, recorder.Body.String())
	}
	history := httptest.NewRecorder()
	server.handleMetricsHistory(history, httptest.NewRequest(http.MethodGet, "/v1/metrics/history?since=1h", nil))
	if history.Code != http.StatusNotFound {
		t.Fatalf("history in legacy mode = %d", history.Code)
	}
}

func TestHandleMetricsHistory(t *testing.T) {
	server, clock := nativeTestServer(t)
	server.statusHistory.collector.First(context.Background())
	for index := 0; index < 5; index++ {
		clock.Advance(time.Minute)
		server.statusHistory.collector.Tick()
	}
	recorder := httptest.NewRecorder()
	server.handleMetricsHistory(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/history?since=1h", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response MetricsHistoryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Node != "app-1" || response.Step != "1m" || len(response.Buckets) == 0 || response.Last == nil {
		t.Fatalf("response = %+v", response)
	}
	bad := httptest.NewRecorder()
	server.handleMetricsHistory(bad, httptest.NewRequest(http.MethodGet, "/v1/metrics/history?since=24h&step=10s", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("step below interval = %d", bad.Code)
	}
	post := httptest.NewRecorder()
	server.handleMetricsHistory(post, httptest.NewRequest(http.MethodPost, "/v1/metrics/history?since=1h", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", post.Code)
	}
}

func TestNativeMetricsDocumentGolden(t *testing.T) {
	sample := hostmetrics.Sample{
		Time: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), CPUPercent: 12.4, CPUCount: 2,
		MemTotalMB: 3920, MemUsedMB: 812, MemAvailMB: 3108, MemPercent: 20.71, SwapTotalMB: 2047, SwapUsedMB: 0,
		Load:          hostmetrics.Load{One: 0.41, Five: 0.38, Fifteen: 0.3, OneText: "0.41", FiveText: "0.38", FifteenText: "0.30"},
		UptimeSeconds: 86400,
		Root:          &hostmetrics.Filesystem{Path: "/", TotalMB: 81920, UsedMB: 20480, AvailableMB: 61440, Percent: 25, InodesPercent: 3.1, HasInodes: true},
		Network:       &hostmetrics.IO{InBytes: 1048576, OutBytes: 524288, InRate: 17476.27, OutRate: 8738.13, InTotal: 2097152, OutTotal: 1048576, WindowSeconds: 60},
		DiskIO:        &hostmetrics.IO{InBytes: 4096, OutBytes: 81920, InRate: 68.27, OutRate: 1365.33, InTotal: 4096, OutTotal: 81920, InSectors: 8, OutSectors: 160, WindowSeconds: 60},
	}
	data, err := json.Marshal(NativeMetricsDocument(sample, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"timestamp":"2026-10-12T10:00:00Z","cpu_percent":"12.40","memory":{"total_mb":3920,"used_mb":812,"available_mb":3108,"percent":"20.71","swap_total_mb":2047,"swap_used_mb":0},"disk":{"path":"/","total_mb":81920,"used_mb":20480,"available_mb":61440,"percent":"25.00","inodes_percent":"3.10"},"uptime_seconds":86400,"load_average":{"1min":"0.41","5min":"0.38","15min":"0.30"},"network":{"rx_bytes":1048576,"tx_bytes":524288,"rx_mb":"1.00","tx_mb":"0.50","rx_bytes_per_sec":17476.27,"tx_bytes_per_sec":8738.13,"rx_bytes_total":2097152,"tx_bytes_total":1048576},"disk_io":{"read_sectors":8,"write_sectors":160,"read_mb":"0.00","write_mb":"0.08","read_bytes_per_sec":68.27,"write_bytes_per_sec":1365.33,"read_bytes_total":4096,"written_bytes_total":81920},"source":"takod","interval_seconds":60,"cpu_count":2}`
	if string(data) != want {
		t.Fatalf("document =\n%s\nwant\n%s", data, want)
	}
	// Legacy fields keep their names and types for CLIs built before
	// native collection.
	var legacy struct {
		CPUPercent string `json:"cpu_percent"`
		Memory     struct {
			TotalMB int    `json:"total_mb"`
			Percent string `json:"percent"`
		} `json:"memory"`
		Disk struct {
			UsedMB  int    `json:"used_mb"`
			Percent string `json:"percent"`
		} `json:"disk"`
		UptimeSeconds int `json:"uptime_seconds"`
		LoadAverage   struct {
			OneMin string `json:"1min"`
		} `json:"load_average"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil || legacy.Memory.TotalMB != 3920 || legacy.Disk.Percent != "25.00" || legacy.LoadAverage.OneMin != "0.41" {
		t.Fatalf("legacy decode = %+v err = %v", legacy, err)
	}
}

func TestMetricsHistoryRouteIsRegisteredReadOnly(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	found := map[string]bool{}
	for _, route := range server.registeredRoutes() {
		found[route.path] = true
	}
	if !found["/v1/metrics/history"] || !found["/v1/node/status"] {
		t.Fatalf("routes missing: %v", found)
	}
	if !lifecycleReadOnlyRequest(httptest.NewRequest(http.MethodGet, "/v1/metrics/history?since=1h", nil)) {
		t.Fatal("history GET must be admitted on cordoned nodes")
	}
}

func TestTakodOpensNoTCPListener(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "net.Listen(") && !strings.Contains(line, `net.Listen("unix"`) {
				t.Fatalf("%s opens a non-unix listener: %s", name, strings.TrimSpace(line))
			}
			if strings.Contains(line, "ListenAndServe") {
				t.Fatalf("%s starts an HTTP listener: %s", name, strings.TrimSpace(line))
			}
		}
	}
}
