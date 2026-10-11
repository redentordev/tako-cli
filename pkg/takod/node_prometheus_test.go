package takod

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNodeStatusResponse() *NodeStatusResponse {
	since := time.Date(2026, 10, 9, 3, 12, 0, 0, time.UTC)
	return &NodeStatusResponse{
		Node: "app-1",
		OS:   &NodeOSStatus{PackageManager: "apt", PendingUpdates: 12, SecurityUpdates: 3, RebootRequired: true, RebootRequiredSince: &since, CheckedAt: since},
		Conditions: []NodeCondition{
			{Type: NodeConditionClockSync, Status: NodeConditionOK},
			{Type: NodeConditionOSUpdates, Status: NodeConditionWarn},
			{Type: NodeConditionRebootRequired, Status: NodeConditionWarn, Since: since},
		},
	}
}

func TestWriteNodePrometheusGolden(t *testing.T) {
	document := NativeMetricsDocument(historySample(historyBase, 10, 100), time.Minute)
	document.Network.RxBytesTotal, document.Network.TxBytesTotal = 1000, 2000
	document.DiskIO.ReadBytesTotal, document.DiskIO.WrittenBytesTotal = 3000, 4000
	document.DockerDisk = &NodeMetricsDisk{Percent: "42.50"}
	var out bytes.Buffer
	WriteNodePrometheus(&out, NodePrometheusInputFromDocument("app-1", &document, testNodeStatusResponse()), false)
	want := `tako_network_receive_bytes_total{server="app-1"} 1000
tako_network_transmit_bytes_total{server="app-1"} 2000
tako_disk_read_bytes_total{server="app-1"} 3000
tako_disk_written_bytes_total{server="app-1"} 4000
tako_docker_filesystem_used_percent{server="app-1"} 42.5
tako_node_condition{server="app-1",type="clock-sync",status="ok"} 1
tako_node_condition{server="app-1",type="clock-sync",status="warn"} 0
tako_node_condition{server="app-1",type="clock-sync",status="fail"} 0
tako_node_condition{server="app-1",type="clock-sync",status="unknown"} 0
tako_node_condition{server="app-1",type="os-updates",status="ok"} 0
tako_node_condition{server="app-1",type="os-updates",status="warn"} 1
tako_node_condition{server="app-1",type="os-updates",status="fail"} 0
tako_node_condition{server="app-1",type="os-updates",status="unknown"} 0
tako_node_condition{server="app-1",type="reboot-required",status="ok"} 0
tako_node_condition{server="app-1",type="reboot-required",status="warn"} 1
tako_node_condition{server="app-1",type="reboot-required",status="fail"} 0
tako_node_condition{server="app-1",type="reboot-required",status="unknown"} 0
tako_node_pending_updates{server="app-1",kind="all"} 12
tako_node_pending_updates{server="app-1",kind="security"} 3
tako_node_reboot_required{server="app-1"} 1
tako_node_clock_synchronized{server="app-1"} 1
`
	if out.String() != want {
		t.Fatalf("exposition =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestWriteNodePrometheusOmitsSeriesForLegacyNodes(t *testing.T) {
	var out bytes.Buffer
	WriteNodePrometheus(&out, NodePrometheusInputFromDocument("legacy", nil, nil), true)
	if out.Len() != 0 {
		t.Fatalf("legacy node exported series:\n%s", out.String())
	}
	legacyDocument := &NodeMetricsDocument{CPUPercent: "1"}
	WriteNodePrometheus(&out, NodePrometheusInputFromDocument("legacy", legacyDocument, nil), true)
	if out.Len() != 0 {
		t.Fatalf("document without source exported series:\n%s", out.String())
	}
}

func TestWriteNodePrometheusTextfileReplacesAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "textfile")
	input := NodePrometheusInput{Server: "app-1", Status: testNodeStatusResponse()}
	if err := writeNodePrometheusTextfile(dir, input); err != nil {
		t.Fatal(err)
	}
	input.Status.Conditions = input.Status.Conditions[:1]
	input.Status.OS = nil
	if err := writeNodePrometheusTextfile(dir, input); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != NodePrometheusFileName {
		t.Fatalf("entries = %v (temporary files must be renamed away)", entries)
	}
	data, err := os.ReadFile(filepath.Join(dir, NodePrometheusFileName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "# TYPE tako_node_clock_synchronized gauge") || strings.Contains(text, "tako_node_reboot_required") {
		t.Fatalf("textfile =\n%s", text)
	}
}

func TestCollectorWritesTextfileWhenConfigured(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "textfile")
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{NodeName: "app-1", Metrics: MetricsOptions{Interval: time.Minute, TextfileDir: dir}})
	collector := server.statusHistory.collector
	collector.reader = &fakeSnapshotReader{}
	collector.history.dir = filepath.Join(t.TempDir(), "history")
	collector.Tick()
	collector.Tick()
	data, err := os.ReadFile(filepath.Join(dir, NodePrometheusFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `tako_network_receive_bytes_total{server="app-1"} 6000`) || !strings.Contains(string(data), "tako_docker_filesystem_used_percent") {
		t.Fatalf("textfile =\n%s", data)
	}
}
