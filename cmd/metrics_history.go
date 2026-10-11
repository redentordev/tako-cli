package cmd

// `tako metrics --since` (add-server-status-history design D7): per-node
// history summaries read from takod's /v1/metrics/history, plus the OS line
// shared with point-in-time metrics.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// metricsSparklineWidth is the fixed width of every history sparkline.
const metricsSparklineWidth = 48

// metricsHistoryRequest is a validated --since/--step pair in takod's query
// syntax (day suffixes are converted to hours).
type metricsHistoryRequest struct {
	Since string
	Step  string
}

// parseMetricsHistoryFlags validates --since/--step. It returns nil when
// --since is absent; conflicting or malformed flags are invalid requests
// (exit 2).
func parseMetricsHistoryFlags(since, step string, live, once bool) (*metricsHistoryRequest, error) {
	since, step = strings.TrimSpace(since), strings.TrimSpace(step)
	if since == "" {
		if step != "" {
			return nil, &engine.InvalidRequestError{Err: fmt.Errorf("--step requires --since")}
		}
		return nil, nil
	}
	if live || once {
		return nil, &engine.InvalidRequestError{Err: fmt.Errorf("--since cannot be combined with --live or --once")}
	}
	request := &metricsHistoryRequest{}
	if duration, ok := parseHistoryDuration(since); ok {
		if duration <= 0 {
			return nil, &engine.InvalidRequestError{Err: fmt.Errorf("--since must be a positive duration")}
		}
		request.Since = formatQueryDuration(duration)
	} else if parsed, err := time.Parse(time.RFC3339, since); err == nil {
		request.Since = parsed.UTC().Format(time.RFC3339)
	} else {
		return nil, &engine.InvalidRequestError{Err: fmt.Errorf("--since must be a duration (24h, 7d) or an RFC3339 timestamp, got %q", since)}
	}
	if step != "" {
		duration, ok := parseHistoryDuration(step)
		if !ok || duration <= 0 {
			return nil, &engine.InvalidRequestError{Err: fmt.Errorf("--step must be a positive duration (e.g. 5m), got %q", step)}
		}
		request.Step = formatQueryDuration(duration)
	}
	return request, nil
}

// parseHistoryDuration accepts Go durations plus a whole-day suffix ("7d").
func parseHistoryDuration(raw string) (time.Duration, bool) {
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return 0, false
		}
		return time.Duration(days) * 24 * time.Hour, true
	}
	duration, err := time.ParseDuration(raw)
	return duration, err == nil
}

func formatQueryDuration(duration time.Duration) string {
	if duration%time.Second == 0 {
		return takod.FormatHistoryStep(duration)
	}
	return duration.String()
}

type metricsHistoryNodeResult struct {
	serverName string
	host       string
	raw        json.RawMessage
	history    *takod.MetricsHistoryResponse
	err        error
}

type metricsHistoryReadFunc func(serverName string, server config.ServerConfig) (json.RawMessage, error)

func showMetricsHistory(cfg *config.Config, envName string, servers []string, factory *nodeclient.Factory, request metricsHistoryRequest) error {
	socket := takodSocketFromConfig(cfg)
	results := collectMetricsHistoryWith(cfg.Servers, servers, func(serverName string, _ config.ServerConfig) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(context.Background(), takodclient.JSONRequestTimeout)
		defer cancel()
		client, _, err := factory.Client(ctx, serverName)
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		if err := takodclient.RequireCapability(ctx, client, socket, serverName, takodclient.CapabilityNodeMetricsHistoryV1, "metrics history (tako metrics --since)"); err != nil {
			return nil, err
		}
		output, err := takodclient.RequestJSONWithContext(ctx, client, socket, "GET", takodclient.MetricsHistoryEndpoint(request.Since, "", request.Step), nil)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(output), nil
	})
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}
	return reportMetricsHistory(out, cfg.Project.Name, envName, metricsServer, request, results)
}

// reportMetricsHistory renders every node and emits the MetricsHistoryResult;
// all nodes failing exits 1 and a partial read exits 6.
func reportMetricsHistory(out io.Writer, project, envName, serverFilter string, request metricsHistoryRequest, results []metricsHistoryNodeResult) error {
	result := engine.MetricsHistoryResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindMetricsHistoryResult,
		Project:     project,
		Environment: envName,
		Server:      serverFilter,
		Since:       request.Since,
		Step:        request.Step,
		Nodes:       []engine.MetricsHistoryNode{},
	}
	failures := 0
	for _, node := range results {
		entry := engine.MetricsHistoryNode{Server: node.serverName, Host: node.host}
		if node.err != nil {
			fmt.Fprintf(out, "❌ %s (%s): %v\n\n", node.serverName, node.host, node.err)
			entry.Error = node.err.Error()
			failures++
		} else {
			renderMetricsHistory(out, node.serverName, node.host, node.history)
			entry.History = node.raw
		}
		result.Nodes = append(result.Nodes, entry)
	}
	var err error
	switch {
	case failures == len(result.Nodes) && failures > 0:
		err = fmt.Errorf("failed to read metrics history from all %d node(s)", failures)
		result.Error = err.Error()
	case failures > 0:
		err = &engine.AttentionError{Err: fmt.Errorf("failed to read metrics history from %d of %d node(s)", failures, len(result.Nodes))}
		result.Error = err.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
		err = emitErr
	}
	return err
}

// collectMetricsHistoryWith reads every node in parallel and keeps the
// configured order. Each document is decoded for rendering and compacted
// for the machine result.
func collectMetricsHistoryWith(configuredServers map[string]config.ServerConfig, serverNames []string, read metricsHistoryReadFunc) []metricsHistoryNodeResult {
	results := make([]metricsHistoryNodeResult, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := configuredServers[serverName]
		if !ok {
			results[index] = metricsHistoryNodeResult{serverName: serverName, err: fmt.Errorf("server not found in configuration")}
			continue
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			result := metricsHistoryNodeResult{serverName: serverName, host: server.Host}
			raw, err := read(serverName, server)
			if err == nil {
				var history takod.MetricsHistoryResponse
				var compact bytes.Buffer
				if err = json.Unmarshal(raw, &history); err != nil {
					err = fmt.Errorf("failed to parse metrics history: %w", err)
				} else if err = json.Compact(&compact, raw); err == nil {
					result.history, result.raw = &history, compact.Bytes()
				}
			}
			result.err = err
			results[index] = result
		}(index, serverName, server)
	}
	wg.Wait()
	return results
}

// historyRow is one rendered metric: per-bucket averages for the sparkline
// plus the summary columns.
type historyRow struct {
	label            string
	min, avg, max    float64
	last             *float64
	points           []historyPoint
	format           func(float64) string
	hasData          bool
	weightedAvgTotal float64
	weight           int
}

type historyPoint struct {
	t     time.Time
	value float64
}

func (r *historyRow) add(t time.Time, n int, min, avg, max float64) {
	if !r.hasData {
		r.min, r.max, r.hasData = min, max, true
	}
	r.min, r.max = math.Min(r.min, min), math.Max(r.max, max)
	if n < 1 {
		n = 1
	}
	r.weightedAvgTotal += avg * float64(n)
	r.weight += n
	r.points = append(r.points, historyPoint{t: t, value: avg})
}

func renderMetricsHistory(out io.Writer, serverName, host string, history *takod.MetricsHistoryResponse) {
	fmt.Fprintf(out, "📈 %s (%s)\n", serverName, host)
	fmt.Fprintf(out, "%s → %s UTC, step %s, %d bucket(s)", history.From.UTC().Format("2006-01-02 15:04"), history.To.UTC().Format("2006-01-02 15:04"), history.Step, len(history.Buckets))
	if history.Truncated {
		fmt.Fprintf(out, " (clamped to %s retention)", formatQueryDuration(time.Duration(history.RetentionSeconds)*time.Second))
	}
	fmt.Fprintf(out, "\n─────────────────────────────────────────────────\n")
	if len(history.Buckets) == 0 {
		fmt.Fprintf(out, "No samples in this window.\n\n")
		return
	}
	percent := func(value float64) string { return strconv.FormatFloat(value, 'f', 1, 64) + "%" }
	load := func(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }
	rows := []*historyRow{
		{label: "CPU", format: percent},
		{label: "Memory", format: percent},
		{label: "Disk /", format: percent},
		{label: "Docker disk", format: percent},
		{label: "Load 1m", format: load},
		{label: "Net rx", format: formatByteRate},
		{label: "Net tx", format: formatByteRate},
		{label: "Disk read", format: formatByteRate},
		{label: "Disk write", format: formatByteRate},
	}
	stat := func(row *historyRow, bucket takod.MetricsHistoryBucket, value *takod.MetricsHistoryStat) {
		if value != nil {
			row.add(bucket.T, bucket.N, value.Min, value.Avg, value.Max)
		}
	}
	rate := func(row *historyRow, bucket takod.MetricsHistoryBucket, value *takod.MetricsHistoryRate) {
		if value != nil {
			// Rate buckets carry avg/max; the row minimum is the lowest bucket average.
			row.add(bucket.T, bucket.N, value.Avg, value.Avg, value.Max)
		}
	}
	for _, bucket := range history.Buckets {
		stat(rows[0], bucket, &bucket.CPU)
		stat(rows[1], bucket, &bucket.Mem)
		stat(rows[2], bucket, &bucket.Disk)
		stat(rows[3], bucket, bucket.DockerDisk)
		stat(rows[4], bucket, &bucket.Load1)
		rate(rows[5], bucket, bucket.RxRate)
		rate(rows[6], bucket, bucket.TxRate)
		rate(rows[7], bucket, bucket.ReadRate)
		rate(rows[8], bucket, bucket.WriteRate)
	}
	if last := history.Last; last != nil {
		rows[0].last, rows[1].last, rows[2].last = &last.CPU, &last.Mem, &last.Disk
		rows[3].last, rows[4].last = last.DockerDisk, &last.Load1
		rows[5].last, rows[6].last, rows[7].last, rows[8].last = last.RxRate, last.TxRate, last.ReadRate, last.WriteRate
	}
	fmt.Fprintf(out, "%-12s %11s %11s %11s %11s  %s\n", "", "min", "avg", "max", "last", "trend")
	for _, row := range rows {
		if !row.hasData {
			continue
		}
		row.avg = row.weightedAvgTotal / float64(row.weight)
		last := "-"
		if row.last != nil {
			last = row.format(*row.last)
		}
		fmt.Fprintf(out, "%-12s %11s %11s %11s %11s  %s\n", row.label, row.format(row.min), row.format(row.avg), row.format(row.max), last, historySparkline(row.points, history.From, history.To, time.Duration(history.StepSeconds)*time.Second, metricsSparklineWidth))
	}
	for _, gap := range history.Gaps {
		fmt.Fprintf(out, "Gap:  %s → %s UTC (%s without samples)\n", gap.From.UTC().Format("2006-01-02 15:04"), gap.To.UTC().Format("2006-01-02 15:04"), formatQueryDuration(gap.To.Sub(gap.From).Truncate(time.Second)))
	}
	for _, boot := range history.Boots {
		fmt.Fprintf(out, "Boot: %s UTC\n", boot.UTC().Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(out)
}

var sparklineLevels = []rune("▁▂▃▄▅▆▇█")

// historySparkline maps bucket averages ([t, t+step)) onto width time slots
// between from and to. A slot averages the buckets starting inside it, or
// else the bucket covering its midpoint; slots without samples are blank so
// gaps stay visible.
func historySparkline(points []historyPoint, from, to time.Time, step time.Duration, width int) string {
	sums := make([]float64, width)
	counts := make([]int, width)
	span := to.Sub(from)
	slotWidth := span / time.Duration(width)
	for slot := 0; slot < width && span > 0; slot++ {
		start := from.Add(time.Duration(float64(span) * float64(slot) / float64(width)))
		end := from.Add(time.Duration(float64(span) * float64(slot+1) / float64(width)))
		middle := start.Add(slotWidth / 2)
		for _, point := range points {
			if !point.t.Before(start) && point.t.Before(end) {
				sums[slot] += point.value
				counts[slot]++
			}
		}
		if counts[slot] > 0 {
			continue
		}
		for _, point := range points {
			if !middle.Before(point.t) && middle.Before(point.t.Add(step)) {
				sums[slot] += point.value
				counts[slot]++
			}
		}
	}
	low, high, seen := 0.0, 0.0, false
	for slot := range sums {
		if counts[slot] == 0 {
			continue
		}
		value := sums[slot] / float64(counts[slot])
		if !seen {
			low, high, seen = value, value, true
		}
		low, high = math.Min(low, value), math.Max(high, value)
	}
	var line strings.Builder
	for slot := range sums {
		if counts[slot] == 0 {
			line.WriteRune(' ')
			continue
		}
		level := 0
		if high > low {
			level = int((sums[slot]/float64(counts[slot]) - low) / (high - low) * float64(len(sparklineLevels)-1))
		}
		line.WriteRune(sparklineLevels[level])
	}
	return line.String()
}

// formatByteRate renders bytes per second with binary units.
func formatByteRate(value float64) string {
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s", "TiB/s"}
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return strconv.FormatFloat(value, 'f', 0, 64) + " " + units[unit]
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + units[unit]
}

// formatOSStatusLine renders node OS status, e.g.
// "OS: 12 updates (3 security), reboot required since 2026-10-09". It is
// empty when the node reports no status (takod without node.status-v1).
func formatOSStatusLine(status *takod.NodeStatusResponse) string {
	if status == nil {
		return ""
	}
	var parts []string
	if updates, ok := status.Condition(takod.NodeConditionOSUpdates); ok {
		osStatus := status.OS
		if updates.Status == takod.NodeConditionUnknown || osStatus == nil || osStatus.PackageManager == "" || osStatus.CheckedAt.IsZero() {
			parts = append(parts, "updates unknown")
		} else {
			part := fmt.Sprintf("%d updates (%d security)", osStatus.PendingUpdates, osStatus.SecurityUpdates)
			if osStatus.Stale || updates.Stale {
				part += " (stale)"
			}
			parts = append(parts, part)
		}
	}
	if reboot, ok := status.Condition(takod.NodeConditionRebootRequired); ok {
		switch reboot.Status {
		case takod.NodeConditionWarn, takod.NodeConditionFail:
			part := "reboot required"
			since := reboot.Since
			if status.OS != nil && status.OS.RebootRequiredSince != nil {
				since = *status.OS.RebootRequiredSince
			}
			if !since.IsZero() {
				part += " since " + since.UTC().Format("2006-01-02")
			}
			parts = append(parts, part)
		case takod.NodeConditionOK:
			parts = append(parts, "no reboot required")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "OS: " + strings.Join(parts, ", ")
}
