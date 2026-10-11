package takod

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

var historyBase = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

func historySample(at time.Time, cpu float64, uptime int64) hostmetrics.Sample {
	return hostmetrics.Sample{
		Time: at, CPUPercent: cpu, MemTotalMB: 1000, MemUsedMB: 250, MemPercent: 25, UptimeSeconds: uptime,
		Load:    hostmetrics.Load{One: cpu / 100},
		Root:    &hostmetrics.Filesystem{TotalMB: 1000, UsedMB: 400, Percent: 40, InodesPercent: 5, HasInodes: true},
		Network: &hostmetrics.IO{InBytes: 6000, OutBytes: 600, WindowSeconds: 60},
		DiskIO:  &hostmetrics.IO{InBytes: 60, OutBytes: 120, WindowSeconds: 60},
	}
}

func newTestHistory(t *testing.T) *metricsHistoryStore {
	t.Helper()
	return newMetricsHistoryStore(filepath.Join(t.TempDir(), "metrics", "history"), DefaultMetricsRetention, DefaultMetricsHistoryMaxBytes, time.Minute)
}

func appendMinutes(t *testing.T, store *metricsHistoryStore, from time.Time, minutes int, uptimeStart int64) {
	t.Helper()
	for index := 0; index < minutes; index++ {
		at := from.Add(time.Duration(index) * time.Minute)
		if err := store.Append(historySample(at, float64(index%100), uptimeStart+int64(index*60)), at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetricsHistoryAppendAndReopen(t *testing.T) {
	store := newTestHistory(t)
	appendMinutes(t, store, historyBase, 90, 1000)
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "20261012T00.jsonl" || entries[1].Name() != "20261012T01.jsonl" {
		t.Fatalf("segments = %v", entries)
	}
	info, _ := os.Stat(store.dir)
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode = %v", info.Mode().Perm())
	}
	if fileInfo, _ := os.Stat(filepath.Join(store.dir, entries[0].Name())); fileInfo.Mode().Perm() != 0o640 {
		t.Fatalf("file mode = %v", fileInfo.Mode().Perm())
	}
	reopened := newMetricsHistoryStore(store.dir, DefaultMetricsRetention, DefaultMetricsHistoryMaxBytes, time.Minute)
	records := reopened.readRange(historyBase, historyBase.Add(2*time.Hour))
	if len(records) != 90 || records[0].T != historyBase.Unix() || records[0].DI == nil || *records[0].DI != 5 {
		t.Fatalf("records = %d first = %+v", len(records), records[0])
	}
}

func TestMetricsHistoryTornLastLineIsSkipped(t *testing.T) {
	store := newTestHistory(t)
	appendMinutes(t, store, historyBase, 5, 1000)
	file, err := os.OpenFile(filepath.Join(store.dir, "20261012T00.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"t":1791763500,"cpu":4`)
	file.Close()
	records := store.readRange(historyBase, historyBase.Add(time.Hour))
	if len(records) != 5 {
		t.Fatalf("records = %d, want the 5 complete samples", len(records))
	}
	// The next append after the torn line still lands as its own record.
	appendMinutes(t, store, historyBase.Add(5*time.Minute), 2, 1300)
	records = store.readRange(historyBase, historyBase.Add(time.Hour))
	if len(records) != 7 || records[6].T != historyBase.Add(6*time.Minute).Unix() {
		t.Fatalf("records after torn line = %d, want 7", len(records))
	}
}

func TestMetricsHistoryRetentionPruning(t *testing.T) {
	store := newMetricsHistoryStore(filepath.Join(t.TempDir(), "history"), 2*time.Hour, DefaultMetricsHistoryMaxBytes, time.Minute)
	appendMinutes(t, store, historyBase, 4*60, 0)
	// The rollover into hour 4 pruned files whose hour ended 2h before.
	if err := store.Append(historySample(historyBase.Add(4*time.Hour), 1, 0), historyBase.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, segment := range store.segments() {
		names = append(names, segment.name)
	}
	if strings.Join(names, ",") != "20261012T02.jsonl,20261012T03.jsonl,20261012T04.jsonl" {
		t.Fatalf("segments after prune = %v", names)
	}
}

func TestMetricsHistorySizeCapDeletesOldestFirst(t *testing.T) {
	store := newTestHistory(t)
	appendMinutes(t, store, historyBase, 3*60, 0)
	segments := store.segments()
	store.maxBytes = segments[1].size + segments[2].size
	store.Prune(historyBase.Add(3 * time.Hour))
	remaining := store.segments()
	if len(remaining) != 2 || remaining[0].name != "20261012T01.jsonl" {
		t.Fatalf("remaining = %+v", remaining)
	}
}

func TestMetricsHistoryWriteFailureReturnsError(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "metrics")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newMetricsHistoryStore(filepath.Join(blocker, "history"), DefaultMetricsRetention, DefaultMetricsHistoryMaxBytes, time.Minute)
	if err := store.Append(historySample(historyBase, 1, 1), historyBase); err == nil {
		t.Fatal("expected append to fail")
	}
}

func TestMetricsHistoryQueryDefaultStep24h(t *testing.T) {
	store := newTestHistory(t)
	now := historyBase.Add(24 * time.Hour)
	appendMinutes(t, store, historyBase, 24*60, 0)
	response, err := store.Query(MetricsHistoryQuery{Since: "24h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if response.Step != "5m" || response.StepSeconds != 300 || len(response.Buckets) != 288 || response.Truncated {
		t.Fatalf("step = %s buckets = %d truncated = %v", response.Step, len(response.Buckets), response.Truncated)
	}
	first := response.Buckets[0]
	if first.N != 5 || first.CPU.Min != 0 || first.CPU.Max != 4 || first.CPU.Avg != 2 || first.Mem.Avg != 25 || first.Disk.Max != 40 {
		t.Fatalf("first bucket = %+v", first)
	}
	if first.RxRate == nil || first.RxRate.Avg != 100 || first.WriteRate == nil || first.WriteRate.Max != 2 {
		t.Fatalf("rates = %+v %+v", first.RxRate, first.WriteRate)
	}
	if response.Last == nil || response.Last.T != now.Add(-time.Minute) || response.Last.RxRate == nil || *response.Last.RxRate != 100 {
		t.Fatalf("last = %+v", response.Last)
	}
	if len(response.Gaps) != 0 || len(response.Boots) != 0 {
		t.Fatalf("gaps = %v boots = %v", response.Gaps, response.Boots)
	}
}

func TestMetricsHistoryQueryStepValidation(t *testing.T) {
	store := newTestHistory(t)
	now := historyBase.Add(24 * time.Hour)
	cases := []struct {
		query   MetricsHistoryQuery
		step    string
		buckets int
		wantErr bool
	}{
		{query: MetricsHistoryQuery{Since: "6h", Step: "15m"}, step: "15m"},
		{query: MetricsHistoryQuery{Since: "1h"}, step: "1m"},
		{query: MetricsHistoryQuery{Since: "720h"}, step: "30m"},
		{query: MetricsHistoryQuery{Since: "24h", Step: "10s"}, wantErr: true},
		{query: MetricsHistoryQuery{Since: "48h", Step: "1m"}, wantErr: true},
		{query: MetricsHistoryQuery{Since: "yesterday"}, wantErr: true},
		{query: MetricsHistoryQuery{}, wantErr: true},
		{query: MetricsHistoryQuery{Since: historyBase.Add(23 * time.Hour).Format(time.RFC3339), Until: historyBase.Add(22 * time.Hour).Format(time.RFC3339)}, wantErr: true},
		{query: MetricsHistoryQuery{Since: historyBase.Add(23 * time.Hour).Format(time.RFC3339)}, step: "1m"},
	}
	for _, testCase := range cases {
		response, err := store.Query(testCase.query, now)
		if testCase.wantErr {
			var requestErr *metricsHistoryRequestError
			if !errors.As(err, &requestErr) {
				t.Errorf("%+v: err = %v, want request error", testCase.query, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", testCase.query, err)
			continue
		}
		if response.Step != testCase.step || len(response.Buckets) != 0 {
			t.Errorf("%+v: step = %s buckets = %d", testCase.query, response.Step, len(response.Buckets))
		}
	}
}

func TestMetricsHistoryQueryClampsToRetention(t *testing.T) {
	store := newTestHistory(t)
	now := historyBase.Add(800 * time.Hour)
	response, err := store.Query(MetricsHistoryQuery{Since: "720h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Truncated || !response.From.Equal(now.Add(-168*time.Hour)) || response.RetentionSeconds != 168*3600 {
		t.Fatalf("from = %s truncated = %v", response.From, response.Truncated)
	}
}

func TestMetricsHistoryQueryGapAndBoot(t *testing.T) {
	store := newTestHistory(t)
	appendMinutes(t, store, historyBase, 30, 5000)
	// takod down for 20 minutes, then the node rebooted (uptime reset).
	appendMinutes(t, store, historyBase.Add(50*time.Minute), 10, 120)
	response, err := store.Query(MetricsHistoryQuery{Since: "1h"}, historyBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Gaps) != 1 || !response.Gaps[0].From.Equal(historyBase.Add(29*time.Minute)) || !response.Gaps[0].To.Equal(historyBase.Add(50*time.Minute)) {
		t.Fatalf("gaps = %+v", response.Gaps)
	}
	if len(response.Boots) != 1 || !response.Boots[0].Equal(historyBase.Add(48*time.Minute)) {
		t.Fatalf("boots = %+v", response.Boots)
	}
	for _, bucket := range response.Buckets {
		if bucket.T.After(historyBase.Add(29*time.Minute)) && bucket.T.Before(historyBase.Add(50*time.Minute)) {
			t.Fatalf("bucket inside the gap: %+v", bucket)
		}
	}
}

func TestMetricsHistoryBackwardClockJumpKeepsOverlap(t *testing.T) {
	store := newTestHistory(t)
	appendMinutes(t, store, historyBase, 10, 1000)
	// The clock jumped back 5 minutes: overlapping samples are kept and
	// averaged; an exact duplicate is dropped.
	appendMinutes(t, store, historyBase.Add(5*time.Minute), 5, 1600)
	if err := store.Append(historySample(historyBase.Add(9*time.Minute), 7, 1900), historyBase.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(historySample(historyBase.Add(9*time.Minute), 7, 1900), historyBase.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	response, err := store.Query(MetricsHistoryQuery{Since: "10m", Step: "5m"}, historyBase.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, bucket := range response.Buckets {
		total += bucket.N
	}
	if total != 16 {
		t.Fatalf("samples = %d, want 10 + 5 overlapping + 1 (duplicate dropped)", total)
	}
}
