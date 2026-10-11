package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

var historyTestStart = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)

func floatPointer(value float64) *float64 { return &value }

func testHistoryResponse() *takod.MetricsHistoryResponse {
	response := &takod.MetricsHistoryResponse{
		Node: "app-1", IntervalSeconds: 60, RetentionSeconds: 604800,
		From: historyTestStart, To: historyTestStart.Add(time.Hour), Step: "15m", StepSeconds: 900,
		Gaps:  []takod.MetricsHistoryGap{{From: historyTestStart.Add(30 * time.Minute), To: historyTestStart.Add(45 * time.Minute)}},
		Boots: []time.Time{historyTestStart.Add(45 * time.Minute)},
	}
	for index, cpu := range []float64{10, 40, 20} {
		start := historyTestStart.Add(time.Duration(index) * 15 * time.Minute)
		if index == 2 {
			start = historyTestStart.Add(45 * time.Minute)
		}
		response.Buckets = append(response.Buckets, takod.MetricsHistoryBucket{
			T: start, N: 15,
			CPU:    takod.MetricsHistoryStat{Min: cpu - 5, Avg: cpu, Max: cpu + 30},
			Mem:    takod.MetricsHistoryStat{Min: 50, Avg: 55, Max: 60},
			Disk:   takod.MetricsHistoryStat{Min: 70, Avg: 70, Max: 70},
			Load1:  takod.MetricsHistoryStat{Min: 0.1, Avg: 0.5, Max: 1.5},
			RxRate: &takod.MetricsHistoryRate{Avg: 2048 * float64(index+1), Max: 8192},
			TxRate: &takod.MetricsHistoryRate{Avg: 512, Max: 1024},
		})
	}
	response.Last = &takod.MetricsHistoryPoint{T: historyTestStart.Add(59 * time.Minute), CPU: 21, Mem: 56, Disk: 70, Load1: 0.4, RxRate: floatPointer(6000), TxRate: floatPointer(500)}
	return response
}

func TestParseMetricsHistoryFlags(t *testing.T) {
	for _, tc := range []struct {
		name, since, step string
		live, once        bool
		want              *metricsHistoryRequest
		invalid           bool
	}{
		{name: "absent", want: nil},
		{name: "duration", since: "24h", want: &metricsHistoryRequest{Since: "24h"}},
		{name: "days", since: "7d", step: "1h", want: &metricsHistoryRequest{Since: "168h", Step: "1h"}},
		{name: "rfc3339", since: "2026-10-11T02:00:00+02:00", want: &metricsHistoryRequest{Since: "2026-10-11T00:00:00Z"}},
		{name: "live conflict", since: "1h", live: true, invalid: true},
		{name: "once conflict", since: "1h", once: true, invalid: true},
		{name: "step without since", step: "5m", invalid: true},
		{name: "bad since", since: "yesterday", invalid: true},
		{name: "negative since", since: "-1h", invalid: true},
		{name: "bad step", since: "1h", step: "0s", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMetricsHistoryFlags(tc.since, tc.step, tc.live, tc.once)
			if tc.invalid {
				var invalid *engine.InvalidRequestError
				if !errors.As(err, &invalid) || exitCodeForError(err) != 2 {
					t.Fatalf("err = %v, want invalid request (exit 2)", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("request = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRenderMetricsHistoryGolden(t *testing.T) {
	var out bytes.Buffer
	renderMetricsHistory(&out, "app-1", "10.0.0.1", testHistoryResponse())
	want := "📈 app-1 (10.0.0.1)\n" +
		"2026-10-11 00:00 → 2026-10-11 01:00 UTC, step 15m, 3 bucket(s)\n" +
		"─────────────────────────────────────────────────\n" +
		"                     min         avg         max        last  trend\n" +
		"CPU                 5.0%       23.3%       70.0%       21.0%  ▁▁▁▁▁▁▁▁▁▁▁▁████████████            ▃▃▃▃▃▃▃▃▃▃▃▃\n" +
		"Memory             50.0%       55.0%       60.0%       56.0%  ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁            ▁▁▁▁▁▁▁▁▁▁▁▁\n" +
		"Disk /             70.0%       70.0%       70.0%       70.0%  ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁            ▁▁▁▁▁▁▁▁▁▁▁▁\n" +
		"Load 1m             0.10        0.50        1.50        0.40  ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁            ▁▁▁▁▁▁▁▁▁▁▁▁\n" +
		"Net rx         2.0 KiB/s   4.0 KiB/s   8.0 KiB/s   5.9 KiB/s  ▁▁▁▁▁▁▁▁▁▁▁▁▄▄▄▄▄▄▄▄▄▄▄▄            ████████████\n" +
		"Net tx           512 B/s     512 B/s   1.0 KiB/s     500 B/s  ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁            ▁▁▁▁▁▁▁▁▁▁▁▁\n" +
		"Gap:  2026-10-11 00:30 → 2026-10-11 00:45 UTC (15m without samples)\n" +
		"Boot: 2026-10-11 00:45 UTC\n\n"
	if out.String() != want {
		t.Fatalf("history rendering =\n%s\nwant\n%s", out.String(), want)
	}
	for _, line := range strings.Split(out.String(), "\n")[4:10] {
		if width := len([]rune(line[62:])); width != metricsSparklineWidth {
			t.Fatalf("sparkline width = %d in %q", width, line)
		}
	}
}

func TestRenderMetricsHistoryEmptyWindow(t *testing.T) {
	response := &takod.MetricsHistoryResponse{From: historyTestStart, To: historyTestStart.Add(time.Hour), Step: "1m", Truncated: true, RetentionSeconds: 3600}
	var out bytes.Buffer
	renderMetricsHistory(&out, "app-1", "10.0.0.1", response)
	if !strings.Contains(out.String(), "No samples in this window.") || !strings.Contains(out.String(), "clamped to 1h retention") {
		t.Fatalf("output =\n%s", out.String())
	}
}

func historyReadFixture(t *testing.T) metricsHistoryReadFunc {
	t.Helper()
	payload, err := json.MarshalIndent(testHistoryResponse(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return func(serverName string, _ config.ServerConfig) (json.RawMessage, error) {
		switch serverName {
		case "old":
			return nil, &takodclient.CapabilityRequiredError{Server: serverName, Capability: takodclient.CapabilityNodeMetricsHistoryV1, Feature: "metrics history (tako metrics --since)"}
		case "down":
			return nil, fmt.Errorf("connect: dial tcp: refused")
		}
		return payload, nil
	}
}

func TestMetricsHistoryMixedVersionsExitsSix(t *testing.T) {
	servers := map[string]config.ServerConfig{"app-1": {Host: "10.0.0.1"}, "old": {Host: "10.0.0.2"}}
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	var human bytes.Buffer
	stdout := captureStdout(t, func() {
		results := collectMetricsHistoryWith(servers, []string{"app-1", "old"}, historyReadFixture(t))
		runErr = reportMetricsHistory(&human, "demo", "production", "", metricsHistoryRequest{Since: "1h"}, results)
	})
	var attention *engine.AttentionError
	if !errors.As(runErr, &attention) || exitCodeForError(runErr) != 6 {
		t.Fatalf("err = %v (exit %d), want attention exit 6", runErr, exitCodeForError(runErr))
	}
	var result engine.MetricsHistoryResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Kind != engine.KindMetricsHistoryResult || len(result.Nodes) != 2 || len(result.Nodes[0].History) == 0 || result.Nodes[0].Error != "" {
		t.Fatalf("result = %+v", result)
	}
	if result.Nodes[1].History != nil || !strings.Contains(result.Nodes[1].Error, "tako upgrade servers") {
		t.Fatalf("old node = %+v, want upgrade hint", result.Nodes[1])
	}
	var history takod.MetricsHistoryResponse
	if err := json.Unmarshal(result.Nodes[0].History, &history); err != nil || history.Step != "15m" || len(history.Buckets) != 3 {
		t.Fatalf("history = %+v (%v)", history, err)
	}
	if !strings.Contains(human.String(), "📈 app-1") || !strings.Contains(human.String(), "❌ old") {
		t.Fatalf("human output =\n%s", human.String())
	}
}

func TestMetricsHistoryAllFailedExitsOne(t *testing.T) {
	servers := map[string]config.ServerConfig{"old": {Host: "10.0.0.2"}, "down": {Host: "10.0.0.3"}}
	results := collectMetricsHistoryWith(servers, []string{"old", "down", "missing"}, historyReadFixture(t))
	var human bytes.Buffer
	err := reportMetricsHistory(&human, "demo", "production", "", metricsHistoryRequest{Since: "1h"}, results)
	if err == nil || exitCodeForError(err) != 1 {
		t.Fatalf("err = %v (exit %d), want exit 1", err, exitCodeForError(err))
	}
	if results[2].err == nil || !strings.Contains(results[2].err.Error(), "not found in configuration") {
		t.Fatalf("missing server result = %+v", results[2])
	}
}

func TestMetricsHistoryResultDocumentGolden(t *testing.T) {
	result := engine.MetricsHistoryResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindMetricsHistoryResult,
		Project:     "demo",
		Environment: "production",
		Since:       "6h",
		Step:        "5m",
		Nodes: []engine.MetricsHistoryNode{
			{Server: "node-a", Host: "10.0.0.1", History: json.RawMessage(`{"step":"5m","buckets":[]}`)},
			{Server: "node-b", Host: "10.0.0.2", Error: "takod on node-b does not support metrics history (tako metrics --since); run 'tako upgrade servers'"},
		},
		Error: "failed to read metrics history from 1 of 2 node(s)",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "MetricsHistoryResult",
  "project": "demo",
  "environment": "production",
  "since": "6h",
  "step": "5m",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "history": {
        "step": "5m",
        "buckets": []
      }
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "error": "takod on node-b does not support metrics history (tako metrics --since); run 'tako upgrade servers'"
    }
  ],
  "error": "failed to read metrics history from 1 of 2 node(s)"
}`
	if string(payload) != want {
		t.Fatalf("metrics history result drifted:\n%s", payload)
	}
}

func testMetricsStatus() *takod.NodeStatusResponse {
	since := time.Date(2026, 10, 9, 3, 12, 0, 0, time.UTC)
	return &takod.NodeStatusResponse{
		Node: "app-1",
		OS:   &takod.NodeOSStatus{PackageManager: "apt", PendingUpdates: 12, SecurityUpdates: 3, RebootRequired: true, RebootRequiredSince: &since, CheckedAt: since},
		Conditions: []takod.NodeCondition{
			{Type: takod.NodeConditionOSUpdates, Status: takod.NodeConditionWarn, Since: since},
			{Type: takod.NodeConditionRebootRequired, Status: takod.NodeConditionWarn, Since: since},
			{Type: takod.NodeConditionClockSync, Status: takod.NodeConditionOK},
		},
	}
}

func TestFormatOSStatusLine(t *testing.T) {
	if got := formatOSStatusLine(testMetricsStatus()); got != "OS: 12 updates (3 security), reboot required since 2026-10-09" {
		t.Fatalf("line = %q", got)
	}
	if got := formatOSStatusLine(nil); got != "" {
		t.Fatalf("nil status line = %q", got)
	}
	unknown := &takod.NodeStatusResponse{Conditions: []takod.NodeCondition{
		{Type: takod.NodeConditionOSUpdates, Status: takod.NodeConditionUnknown},
		{Type: takod.NodeConditionRebootRequired, Status: takod.NodeConditionOK},
	}}
	if got := formatOSStatusLine(unknown); got != "OS: updates unknown, no reboot required" {
		t.Fatalf("unknown line = %q", got)
	}
}

// nativeMetricsResponse builds a /v1/metrics response the way takod's native
// collector does.
func nativeMetricsResponse(t *testing.T, status *takod.NodeStatusResponse) *takod.MetricsResponse {
	t.Helper()
	document := takod.NodeMetricsDocument{
		Timestamp: "2026-10-11T12:00:00Z", CPUPercent: "12.50",
		Memory:        takod.NodeMetricsMemory{TotalMB: 4096, UsedMB: 1024, AvailableMB: 3072, Percent: "25.00"},
		Disk:          takod.NodeMetricsDisk{Path: "/", TotalMB: 100000, UsedMB: 40000, AvailableMB: 60000, Percent: "40.00", InodesPercent: "3.00"},
		UptimeSeconds: 90061,
		LoadAverage:   takod.NodeMetricsLoad{OneMin: "0.50", FiveMin: "0.40", FifteenMin: "0.30"},
		Network:       &takod.NodeMetricsNetwork{RxBytes: 1 << 20, TxBytes: 1 << 19, RxMB: "1.00", TxMB: "0.50", RxBytesPerSec: 17476.27, TxBytesPerSec: 8738.13, RxBytesTotal: 5 << 20, TxBytesTotal: 3 << 20},
		DiskIO:        &takod.NodeMetricsDiskIO{ReadSectors: 2048, WriteSectors: 4096, ReadMB: "1.00", WriteMB: "2.00", ReadBytesPerSec: 17476.27, WriteBytesPerSec: 34952.53, ReadBytesTotal: 7 << 20, WrittenBytesTotal: 9 << 20},
		Source:        takod.NodeMetricsSource, IntervalSeconds: 60, CPUCount: 2,
		DockerDisk: &takod.NodeMetricsDisk{Path: "/var/lib/docker", TotalMB: 200000, UsedMB: 50000, AvailableMB: 150000, Percent: "25.00"},
	}
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return &takod.MetricsResponse{Metrics: payload, Status: status}
}

func TestDecodeNativeMetricsKeepsLegacyFields(t *testing.T) {
	metrics, err := decodeMetricsResponse(nativeMetricsResponse(t, testMetricsStatus()))
	if err != nil {
		t.Fatal(err)
	}
	if metrics.CPUPercent != "12.50" || metrics.Memory.Percent != "25.00" || metrics.Disk.UsedMB != 40000 || metrics.Network.RxBytes != 1<<20 || metrics.DiskIO.WriteSectors != 4096 || metrics.LoadAverage.FiveMin != "0.40" || metrics.UptimeSeconds != 90061 {
		t.Fatalf("legacy fields = %+v", metrics)
	}
	if !metrics.native() || metrics.nativeDocument == nil || metrics.Status == nil || metrics.DockerDisk == nil {
		t.Fatalf("native fields = %+v", metrics)
	}
	legacy, err := decodeMetricsResponse(&takod.MetricsResponse{Metrics: json.RawMessage(`{"cpu_percent":"3.0","network":{"rx_mb":"0.10","tx_mb":"0.20"}}`)})
	if err != nil || legacy.native() || legacy.nativeDocument != nil || legacy.Status != nil {
		t.Fatalf("legacy decode = %+v (%v)", legacy, err)
	}
}

func TestDisplayServerMetricsGoldenWithStatus(t *testing.T) {
	metrics, err := decodeMetricsResponse(nativeMetricsResponse(t, testMetricsStatus()))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	displayServerMetrics(&out, "app-1", "10.0.0.1", metrics)
	want := "📊 app-1 (10.0.0.1)\n" +
		"─────────────────────────────────────────────────\n" +
		"CPU:    12.50% [██░░░░░░░░░░░░░░░░░░]\n" +
		"Memory: 25.00% (1024 MB / 4096 MB) [█████░░░░░░░░░░░░░░░]\n" +
		"Disk:   40.00% (40000 MB / 100000 MB) [████████░░░░░░░░░░░░]\n" +
		"Docker: 25.00% (50000 MB / 200000 MB) [█████░░░░░░░░░░░░░░░]\n" +
		"Net I/O: ↓ 17.1 KiB/s  ↑ 8.5 KiB/s\n" +
		"Disk I/O: read 17.1 KiB/s  write 34.1 KiB/s\n" +
		"Load:   0.50 (1m) / 0.40 (5m) / 0.30 (15m)\n" +
		"Uptime: 1d 1h 1m\n" +
		"OS: 12 updates (3 security), reboot required since 2026-10-09\n" +
		"Updated: 2026-10-11T12:00:00Z\n\n"
	if out.String() != want {
		t.Fatalf("metrics rendering =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestDisplayServerMetricsLegacyWithoutStatus(t *testing.T) {
	metrics, err := decodeMetricsResponse(&takod.MetricsResponse{Metrics: json.RawMessage(`{"timestamp":"2026-10-11T12:00:00Z","cpu_percent":"3.0","memory":{"percent":"10"},"disk":{"percent":"20"},"network":{"rx_mb":"0.10","tx_mb":"0.20"},"disk_io":{"read_mb":"1.00","write_mb":"2.00"},"uptime_seconds":60,"load_average":{"1min":"0.1","5min":"0.2","15min":"0.3"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	displayServerMetrics(&out, "legacy", "10.0.0.9", metrics)
	text := out.String()
	if !strings.Contains(text, "Net I/O: ↓ 0.10 MB  ↑ 0.20 MB (since last check)") || !strings.Contains(text, "Disk I/O: ⬇ 1.00 MB  ⬆ 2.00 MB (since last check)") {
		t.Fatalf("legacy I/O lines missing:\n%s", text)
	}
	if strings.Contains(text, "OS:") || strings.Contains(text, "Docker:") {
		t.Fatalf("legacy node rendered native lines:\n%s", text)
	}
}

func TestMetricsResultCarriesNodeStatus(t *testing.T) {
	status, err := json.Marshal(testMetricsStatus())
	if err != nil {
		t.Fatal(err)
	}
	sample := engine.MetricsNodeSample{Server: "app-1", Metrics: json.RawMessage(`{"cpu_percent":"1"}`), Status: status}
	payload, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"status":{"node":"app-1"`) || !strings.Contains(string(payload), `"rebootRequired":true`) {
		t.Fatalf("sample = %s", payload)
	}
	payload, _ = json.Marshal(engine.MetricsNodeSample{Server: "legacy", Metrics: json.RawMessage(`{}`)})
	if strings.Contains(string(payload), "status") {
		t.Fatalf("legacy sample carries status: %s", payload)
	}
}

func TestExportNodeMetricsNativeAndLegacy(t *testing.T) {
	metrics, err := decodeMetricsResponse(nativeMetricsResponse(t, testMetricsStatus()))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if !exportNodeMetrics(&out, "app-1", metrics, true) {
		t.Fatal("native node exported nothing")
	}
	for _, want := range []string{
		`tako_network_receive_bytes_total{server="app-1"} 5242880`,
		`tako_disk_written_bytes_total{server="app-1"} 9437184`,
		`tako_docker_filesystem_used_percent{server="app-1"} 25`,
		`tako_node_condition{server="app-1",type="reboot-required",status="warn"} 1`,
		`tako_node_pending_updates{server="app-1",kind="security"} 3`,
		`tako_node_reboot_required{server="app-1"} 1`,
		`tako_node_clock_synchronized{server="app-1"} 1`,
		"# TYPE tako_node_reboot_required gauge",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("exposition missing %q:\n%s", want, out.String())
		}
	}
	legacy, err := decodeMetricsResponse(&takod.MetricsResponse{Metrics: json.RawMessage(`{"cpu_percent":"3.0","network":{"rx_bytes":10}}`)})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if exportNodeMetrics(&out, "legacy", legacy, true) || out.Len() != 0 {
		t.Fatalf("legacy node exported node series:\n%s", out.String())
	}
}

func TestNodeCapabilityConstantsMatchTakod(t *testing.T) {
	if takodclient.CapabilityNodeMetricsHistoryV1 != takod.CapabilityNodeMetricsHistoryV1 || takodclient.CapabilityNodeStatusV1 != takod.CapabilityNodeStatusV1 {
		t.Fatal("takodclient and takod node capability names diverged")
	}
	if got := takodclient.MetricsHistoryEndpoint("24h", "", "5m"); got != "/v1/metrics/history?since=24h&step=5m" {
		t.Fatalf("endpoint = %q", got)
	}
}
