package takod

// Bounded on-node metrics history (add-server-status-history design D3/D4):
// hourly UTC JSON-lines segments under <data-dir>/metrics/history, pruned by
// retention and a byte cap, and queried into min/avg/max buckets.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod/hostmetrics"
)

const (
	DefaultMetricsInterval         = 60 * time.Second
	DefaultMetricsRetention        = 168 * time.Hour
	MinimumMetricsRetention        = time.Hour
	MaximumMetricsRetention        = 744 * time.Hour
	DefaultMetricsHistoryMaxBytes  = 64 << 20
	metricsHistorySegmentLayout    = "20060102T15"
	metricsHistorySegmentExt       = ".jsonl"
	metricsHistoryDefaultBuckets   = 360
	metricsHistoryMaximumBuckets   = 1440
	metricsHistoryGapIntervals     = 3
	metricsHistoryMaxLineBytes     = 64 << 10
	metricsHistoryDirectoryMode    = 0o750
	metricsHistoryFileMode         = 0o640
	metricsHistoryReasonWriteError = "history-write-failed"
)

// metricsHistoryNiceSteps are the standard bucket widths for a default step.
var metricsHistoryNiceSteps = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 6 * time.Hour,
}

// metricsHistoryRecord is one compact history line. Sizes are MB, rates are
// derived from byte deltas over the window w (seconds).
type metricsHistoryRecord struct {
	T   int64    `json:"t"`
	CPU float64  `json:"cpu"`
	MU  int64    `json:"mu"`
	MT  int64    `json:"mt"`
	MP  float64  `json:"mp"`
	SU  int64    `json:"su"`
	ST  int64    `json:"st"`
	DU  int64    `json:"du"`
	DT  int64    `json:"dt"`
	DP  float64  `json:"dp"`
	DI  *float64 `json:"di,omitempty"`
	DDU *int64   `json:"ddu,omitempty"`
	DDT *int64   `json:"ddt,omitempty"`
	DDP *float64 `json:"ddp,omitempty"`
	DDI *float64 `json:"ddi,omitempty"`
	L1  float64  `json:"l1"`
	L5  float64  `json:"l5"`
	L15 float64  `json:"l15"`
	RX  *uint64  `json:"rx,omitempty"`
	TX  *uint64  `json:"tx,omitempty"`
	RD  *uint64  `json:"rd,omitempty"`
	WR  *uint64  `json:"wr,omitempty"`
	W   float64  `json:"w,omitempty"`
	UP  int64    `json:"up"`
}

func historyRecordFromSample(sample hostmetrics.Sample) metricsHistoryRecord {
	record := metricsHistoryRecord{
		T: sample.Time.UTC().Unix(), CPU: sample.CPUPercent, MU: sample.MemUsedMB, MT: sample.MemTotalMB, MP: sample.MemPercent,
		SU: sample.SwapUsedMB, ST: sample.SwapTotalMB, L1: sample.Load.One, L5: sample.Load.Five, L15: sample.Load.Fifteen, UP: sample.UptimeSeconds,
	}
	if root := sample.Root; root != nil {
		record.DU, record.DT, record.DP = root.UsedMB, root.TotalMB, root.Percent
		if root.HasInodes {
			value := root.InodesPercent
			record.DI = &value
		}
	}
	if docker := sample.Docker; docker != nil {
		used, total, percent := docker.UsedMB, docker.TotalMB, docker.Percent
		record.DDU, record.DDT, record.DDP = &used, &total, &percent
		if docker.HasInodes {
			value := docker.InodesPercent
			record.DDI = &value
		}
	}
	if io := sample.Network; io != nil {
		rx, tx := io.InBytes, io.OutBytes
		record.RX, record.TX, record.W = &rx, &tx, io.WindowSeconds
	}
	if io := sample.DiskIO; io != nil {
		read, written := io.InBytes, io.OutBytes
		record.RD, record.WR, record.W = &read, &written, io.WindowSeconds
	}
	return record
}

// metricsHistoryStore appends samples and answers range queries.
type metricsHistoryStore struct {
	dir       string
	retention time.Duration
	maxBytes  int64
	interval  time.Duration

	mu       sync.Mutex
	lastHour int64
}

func newMetricsHistoryStore(dir string, retention time.Duration, maxBytes int64, interval time.Duration) *metricsHistoryStore {
	if retention <= 0 {
		retention = DefaultMetricsRetention
	}
	if retention < MinimumMetricsRetention {
		retention = MinimumMetricsRetention
	}
	if retention > MaximumMetricsRetention {
		retention = MaximumMetricsRetention
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMetricsHistoryMaxBytes
	}
	if interval <= 0 {
		interval = DefaultMetricsInterval
	}
	return &metricsHistoryStore{dir: dir, retention: retention, maxBytes: maxBytes, interval: interval, lastHour: -1}
}

func metricsHistorySegmentName(t time.Time) string {
	return t.UTC().Format(metricsHistorySegmentLayout) + metricsHistorySegmentExt
}

func parseMetricsHistorySegment(name string) (time.Time, bool) {
	if !strings.HasSuffix(name, metricsHistorySegmentExt) {
		return time.Time{}, false
	}
	hour, err := time.Parse(metricsHistorySegmentLayout, strings.TrimSuffix(name, metricsHistorySegmentExt))
	if err != nil {
		return time.Time{}, false
	}
	return hour.UTC(), true
}

// Append writes one record with a single O_APPEND write, pruning on the
// first append and whenever the hour rolls over.
func (s *metricsHistoryStore) Append(sample hostmetrics.Sample, now time.Time) error {
	record := historyRecordFromSample(sample)
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, metricsHistoryDirectoryMode); err != nil {
		return err
	}
	hour := now.UTC().Unix() / 3600
	if hour != s.lastHour {
		s.lastHour = hour
		s.pruneLocked(now)
	}
	file, err := os.OpenFile(filepath.Join(s.dir, metricsHistorySegmentName(time.Unix(record.T, 0))), os.O_WRONLY|os.O_APPEND|os.O_CREATE, metricsHistoryFileMode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// Prune deletes segments older than the retention, then the oldest
// segments until the directory is under the byte cap.
func (s *metricsHistoryStore) Prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
}

type metricsHistorySegment struct {
	name string
	hour time.Time
	size int64
}

func (s *metricsHistoryStore) segments() []metricsHistorySegment {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	segments := make([]metricsHistorySegment, 0, len(entries))
	for _, entry := range entries {
		hour, ok := parseMetricsHistorySegment(entry.Name())
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		segments = append(segments, metricsHistorySegment{name: entry.Name(), hour: hour, size: info.Size()})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].hour.Before(segments[j].hour) })
	return segments
}

func (s *metricsHistoryStore) pruneLocked(now time.Time) {
	cutoff := now.UTC().Add(-s.retention)
	var kept []metricsHistorySegment
	var total int64
	for _, segment := range s.segments() {
		if segment.hour.Add(time.Hour).Before(cutoff) || segment.hour.Add(time.Hour).Equal(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, segment.name))
			continue
		}
		kept = append(kept, segment)
		total += segment.size
	}
	for len(kept) > 0 && total > s.maxBytes {
		_ = os.Remove(filepath.Join(s.dir, kept[0].name))
		total -= kept[0].size
		kept = kept[1:]
	}
}

// readRange returns the records with from <= t < until from the segments
// overlapping the range, sorted by t with exact duplicates dropped. Lines
// that do not parse (a torn last line) are skipped.
func (s *metricsHistoryStore) readRange(from, until time.Time) []metricsHistoryRecord {
	s.mu.Lock()
	segments := s.segments()
	s.mu.Unlock()
	fromHour := from.UTC().Truncate(time.Hour)
	var records []metricsHistoryRecord
	for _, segment := range segments {
		if segment.hour.Before(fromHour) || !segment.hour.Before(until) {
			continue
		}
		file, err := os.Open(filepath.Join(s.dir, segment.name))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 4096), metricsHistoryMaxLineBytes)
		for scanner.Scan() {
			var record metricsHistoryRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				continue
			}
			if record.T < from.Unix() || record.T >= until.Unix() {
				continue
			}
			records = append(records, record)
		}
		file.Close()
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].T < records[j].T })
	unique := records[:0]
	var previous []byte
	for _, record := range records {
		encoded, _ := json.Marshal(record)
		if previous != nil && string(encoded) == string(previous) {
			continue
		}
		previous = encoded
		unique = append(unique, record)
	}
	return unique
}

// MetricsHistoryStat is a min/avg/max aggregate.
type MetricsHistoryStat struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// MetricsHistoryRate is an avg/max aggregate of a per-second rate.
type MetricsHistoryRate struct {
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// MetricsHistoryBucket aggregates the samples in [t, t+step).
type MetricsHistoryBucket struct {
	T          time.Time           `json:"t"`
	N          int                 `json:"n"`
	CPU        MetricsHistoryStat  `json:"cpu"`
	Mem        MetricsHistoryStat  `json:"mem"`
	Disk       MetricsHistoryStat  `json:"disk"`
	DockerDisk *MetricsHistoryStat `json:"dockerDisk,omitempty"`
	Load1      MetricsHistoryStat  `json:"load1"`
	RxRate     *MetricsHistoryRate `json:"rxRate,omitempty"`
	TxRate     *MetricsHistoryRate `json:"txRate,omitempty"`
	ReadRate   *MetricsHistoryRate `json:"readRate,omitempty"`
	WriteRate  *MetricsHistoryRate `json:"writeRate,omitempty"`
}

// MetricsHistoryPoint is the newest raw sample as percentages and rates.
type MetricsHistoryPoint struct {
	T          time.Time `json:"t"`
	CPU        float64   `json:"cpu"`
	Mem        float64   `json:"mem"`
	Disk       float64   `json:"disk"`
	DockerDisk *float64  `json:"dockerDisk,omitempty"`
	Load1      float64   `json:"load1"`
	RxRate     *float64  `json:"rxRate,omitempty"`
	TxRate     *float64  `json:"txRate,omitempty"`
	ReadRate   *float64  `json:"readRate,omitempty"`
	WriteRate  *float64  `json:"writeRate,omitempty"`
}

// MetricsHistoryGap is a period longer than three intervals without samples.
type MetricsHistoryGap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// MetricsHistoryResponse is the GET /v1/metrics/history document.
type MetricsHistoryResponse struct {
	Node             string                 `json:"node,omitempty"`
	IntervalSeconds  int64                  `json:"intervalSeconds"`
	RetentionSeconds int64                  `json:"retentionSeconds"`
	From             time.Time              `json:"from"`
	To               time.Time              `json:"to"`
	Step             string                 `json:"step"`
	StepSeconds      int64                  `json:"stepSeconds"`
	Truncated        bool                   `json:"truncated,omitempty"`
	Buckets          []MetricsHistoryBucket `json:"buckets"`
	Last             *MetricsHistoryPoint   `json:"last,omitempty"`
	Gaps             []MetricsHistoryGap    `json:"gaps,omitempty"`
	Boots            []time.Time            `json:"boots,omitempty"`
}

// MetricsHistoryQuery is a parsed history request.
type MetricsHistoryQuery struct {
	Since string
	Until string
	Step  string
}

// metricsHistoryRequestError is a client error (HTTP 400).
type metricsHistoryRequestError struct{ msg string }

func (e *metricsHistoryRequestError) Error() string { return e.msg }

func historyRequestErrorf(format string, args ...any) error {
	return &metricsHistoryRequestError{msg: fmt.Sprintf(format, args...)}
}

// FormatHistoryStep renders a step as a compact duration ("5m", "2h", "90s").
func FormatHistoryStep(step time.Duration) string {
	switch {
	case step%time.Hour == 0:
		return fmt.Sprintf("%dh", step/time.Hour)
	case step%time.Minute == 0:
		return fmt.Sprintf("%dm", step/time.Minute)
	default:
		return fmt.Sprintf("%ds", step/time.Second)
	}
}

func bucketCount(span, step time.Duration) int64 {
	return int64((span + step - 1) / step)
}

// Query answers a history request at time now.
func (s *metricsHistoryStore) Query(query MetricsHistoryQuery, now time.Time) (*MetricsHistoryResponse, error) {
	now = now.UTC().Truncate(time.Second)
	until := now
	if raw := strings.TrimSpace(query.Until); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, historyRequestErrorf("until must be an RFC3339 timestamp")
		}
		until = parsed.UTC()
		if until.After(now) {
			until = now
		}
	}
	raw := strings.TrimSpace(query.Since)
	if raw == "" {
		return nil, historyRequestErrorf("since is required")
	}
	var from time.Time
	if duration, err := time.ParseDuration(raw); err == nil {
		if duration <= 0 {
			return nil, historyRequestErrorf("since must be a positive duration")
		}
		from = until.Add(-duration)
	} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		from = parsed.UTC()
	} else {
		return nil, historyRequestErrorf("since must be a duration (e.g. 24h) or an RFC3339 timestamp")
	}
	response := &MetricsHistoryResponse{
		IntervalSeconds: int64(s.interval / time.Second), RetentionSeconds: int64(s.retention / time.Second), Buckets: []MetricsHistoryBucket{},
	}
	if earliest := now.Add(-s.retention); from.Before(earliest) {
		from = earliest
		response.Truncated = true
	}
	if !from.Before(until) {
		return nil, historyRequestErrorf("since must be before until")
	}
	span := until.Sub(from)
	var step time.Duration
	if rawStep := strings.TrimSpace(query.Step); rawStep != "" {
		parsed, err := time.ParseDuration(rawStep)
		if err != nil || parsed <= 0 {
			return nil, historyRequestErrorf("step must be a positive duration")
		}
		if parsed < s.interval {
			return nil, historyRequestErrorf("step %s is below the collection interval %s", rawStep, s.interval)
		}
		if bucketCount(span, parsed) > metricsHistoryMaximumBuckets {
			return nil, historyRequestErrorf("step %s yields more than %d buckets; use a larger step", rawStep, metricsHistoryMaximumBuckets)
		}
		step = parsed
	} else {
		for _, candidate := range metricsHistoryNiceSteps {
			if candidate < s.interval {
				continue
			}
			if bucketCount(span, candidate) <= metricsHistoryDefaultBuckets {
				step = candidate
				break
			}
		}
		if step == 0 {
			step = metricsHistoryNiceSteps[len(metricsHistoryNiceSteps)-1]
			for bucketCount(span, step) > metricsHistoryDefaultBuckets {
				step *= 2
			}
		}
	}
	response.From, response.To = from, until
	response.Step, response.StepSeconds = FormatHistoryStep(step), int64(step/time.Second)
	records := s.readRange(from, until)
	fillHistoryResponse(response, records, from, step, s.interval)
	return response, nil
}

type historyAccumulator struct {
	n                    int
	cpu, mem, disk, load statAccumulator
	docker               statAccumulator
	rx, tx, read, write  rateAccumulator
	hasDocker            bool
}

type statAccumulator struct {
	n             int
	min, max, sum float64
}

func (a *statAccumulator) add(value float64) {
	if a.n == 0 || value < a.min {
		a.min = value
	}
	if a.n == 0 || value > a.max {
		a.max = value
	}
	a.sum += value
	a.n++
}

func (a statAccumulator) stat() MetricsHistoryStat {
	if a.n == 0 {
		return MetricsHistoryStat{}
	}
	return MetricsHistoryStat{Min: roundHistory(a.min), Avg: roundHistory(a.sum / float64(a.n)), Max: roundHistory(a.max)}
}

type rateAccumulator struct{ statAccumulator }

func (a rateAccumulator) rate() *MetricsHistoryRate {
	if a.n == 0 {
		return nil
	}
	return &MetricsHistoryRate{Avg: roundHistory(a.sum / float64(a.n)), Max: roundHistory(a.max)}
}

func roundHistory(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

func recordRate(bytes *uint64, window float64) (float64, bool) {
	if bytes == nil || window <= 0 {
		return 0, false
	}
	return float64(*bytes) / window, true
}

func fillHistoryResponse(response *MetricsHistoryResponse, records []metricsHistoryRecord, from time.Time, step, interval time.Duration) {
	buckets := map[int64]*historyAccumulator{}
	var order []int64
	gapThreshold := int64(metricsHistoryGapIntervals * interval / time.Second)
	for index, record := range records {
		key := (record.T - from.Unix()) / int64(step/time.Second)
		acc := buckets[key]
		if acc == nil {
			acc = &historyAccumulator{}
			buckets[key] = acc
			order = append(order, key)
		}
		acc.n++
		acc.cpu.add(record.CPU)
		acc.mem.add(record.MP)
		acc.disk.add(record.DP)
		acc.load.add(record.L1)
		if record.DDP != nil {
			acc.hasDocker = true
			acc.docker.add(*record.DDP)
		}
		if value, ok := recordRate(record.RX, record.W); ok {
			acc.rx.add(value)
		}
		if value, ok := recordRate(record.TX, record.W); ok {
			acc.tx.add(value)
		}
		if value, ok := recordRate(record.RD, record.W); ok {
			acc.read.add(value)
		}
		if value, ok := recordRate(record.WR, record.W); ok {
			acc.write.add(value)
		}
		if index > 0 {
			previous := records[index-1]
			if record.T-previous.T > gapThreshold {
				response.Gaps = append(response.Gaps, MetricsHistoryGap{From: time.Unix(previous.T, 0).UTC(), To: time.Unix(record.T, 0).UTC()})
			}
			if record.UP < previous.UP {
				response.Boots = append(response.Boots, time.Unix(record.T-record.UP, 0).UTC())
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, key := range order {
		acc := buckets[key]
		bucket := MetricsHistoryBucket{
			T: from.Add(time.Duration(key) * step).UTC(), N: acc.n,
			CPU: acc.cpu.stat(), Mem: acc.mem.stat(), Disk: acc.disk.stat(), Load1: acc.load.stat(),
			RxRate: acc.rx.rate(), TxRate: acc.tx.rate(), ReadRate: acc.read.rate(), WriteRate: acc.write.rate(),
		}
		if acc.hasDocker {
			docker := acc.docker.stat()
			bucket.DockerDisk = &docker
		}
		response.Buckets = append(response.Buckets, bucket)
	}
	if len(records) > 0 {
		last := records[len(records)-1]
		point := &MetricsHistoryPoint{T: time.Unix(last.T, 0).UTC(), CPU: last.CPU, Mem: last.MP, Disk: last.DP, Load1: last.L1, DockerDisk: last.DDP}
		rate := func(bytes *uint64) *float64 {
			if value, ok := recordRate(bytes, last.W); ok {
				value = roundHistory(value)
				return &value
			}
			return nil
		}
		point.RxRate, point.TxRate, point.ReadRate, point.WriteRate = rate(last.RX), rate(last.TX), rate(last.RD), rate(last.WR)
		response.Last = point
	}
}
