package takod

// Node-local Prometheus series (add-server-status-history design D7/D10),
// shared by `tako prometheus` and the opt-in --metrics-textfile-dir export.
// takod never opens a network listener for these series.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// NodePrometheusFileName is the textfile written under --metrics-textfile-dir.
const NodePrometheusFileName = "tako_node.prom"

// NodePrometheusInput holds the node-local values behind the series. Nil
// fields are omitted, so legacy-mode nodes emit none of these series.
type NodePrometheusInput struct {
	Server                      string
	NetworkReceiveBytesTotal    *int64
	NetworkTransmitBytesTotal   *int64
	DiskReadBytesTotal          *int64
	DiskWrittenBytesTotal       *int64
	DockerFilesystemUsedPercent *float64
	Status                      *NodeStatusResponse
}

// NodePrometheusInputFromDocument builds the input from a native metrics
// document and node status.
func NodePrometheusInputFromDocument(server string, document *NodeMetricsDocument, status *NodeStatusResponse) NodePrometheusInput {
	input := NodePrometheusInput{Server: server, Status: status}
	if document == nil || document.Source != NodeMetricsSource {
		return input
	}
	if network := document.Network; network != nil {
		rx, tx := network.RxBytesTotal, network.TxBytesTotal
		input.NetworkReceiveBytesTotal, input.NetworkTransmitBytesTotal = &rx, &tx
	}
	if diskIO := document.DiskIO; diskIO != nil {
		read, written := diskIO.ReadBytesTotal, diskIO.WrittenBytesTotal
		input.DiskReadBytesTotal, input.DiskWrittenBytesTotal = &read, &written
	}
	if document.DockerDisk != nil {
		if percent, err := strconv.ParseFloat(document.DockerDisk.Percent, 64); err == nil {
			input.DockerFilesystemUsedPercent = &percent
		}
	}
	return input
}

// WriteNodePrometheus writes the node-local series in the text exposition
// format. includeMetadata controls HELP/TYPE lines (emit once per scrape).
func WriteNodePrometheus(out io.Writer, input NodePrometheusInput, includeMetadata bool) {
	server := strconv.Quote(input.Server)
	series := func(name, kind, help string) {
		if includeMetadata {
			fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		}
	}
	counter := func(name, help string, value *int64) {
		if value == nil {
			return
		}
		series(name, "counter", help)
		fmt.Fprintf(out, "%s{server=%s} %d\n", name, server, *value)
	}
	counter("tako_network_receive_bytes_total", "Bytes received on host interfaces since takod started", input.NetworkReceiveBytesTotal)
	counter("tako_network_transmit_bytes_total", "Bytes transmitted on host interfaces since takod started", input.NetworkTransmitBytesTotal)
	counter("tako_disk_read_bytes_total", "Bytes read from whole disk devices since takod started", input.DiskReadBytesTotal)
	counter("tako_disk_written_bytes_total", "Bytes written to whole disk devices since takod started", input.DiskWrittenBytesTotal)
	if input.DockerFilesystemUsedPercent != nil {
		series("tako_docker_filesystem_used_percent", "gauge", "Docker data-root filesystem usage percentage when separate from /")
		fmt.Fprintf(out, "tako_docker_filesystem_used_percent{server=%s} %s\n", server, strconv.FormatFloat(*input.DockerFilesystemUsedPercent, 'f', -1, 64))
	}
	status := input.Status
	if status == nil {
		return
	}
	if len(status.Conditions) > 0 {
		series("tako_node_condition", "gauge", "Node condition status (1 for the current status)")
		for _, condition := range status.Conditions {
			for _, value := range []string{NodeConditionOK, NodeConditionWarn, NodeConditionFail, NodeConditionUnknown} {
				current := 0
				if condition.Status == value {
					current = 1
				}
				fmt.Fprintf(out, "tako_node_condition{server=%s,type=%s,status=%s} %d\n", server, strconv.Quote(condition.Type), strconv.Quote(value), current)
			}
		}
	}
	if osStatus := status.OS; osStatus != nil && osStatus.PackageManager != "" {
		if osUpdates, ok := status.Condition(NodeConditionOSUpdates); ok && osUpdates.Status != NodeConditionUnknown && !osStatus.CheckedAt.IsZero() {
			series("tako_node_pending_updates", "gauge", "Pending OS package updates from cached package lists")
			fmt.Fprintf(out, "tako_node_pending_updates{server=%s,kind=\"all\"} %d\n", server, osStatus.PendingUpdates)
			fmt.Fprintf(out, "tako_node_pending_updates{server=%s,kind=\"security\"} %d\n", server, osStatus.SecurityUpdates)
		}
	}
	if reboot, ok := status.Condition(NodeConditionRebootRequired); ok && reboot.Status != NodeConditionUnknown {
		value := 0
		if reboot.Status == NodeConditionWarn {
			value = 1
		}
		series("tako_node_reboot_required", "gauge", "Whether the node requires a reboot")
		fmt.Fprintf(out, "tako_node_reboot_required{server=%s} %d\n", server, value)
	}
	if clock, ok := status.Condition(NodeConditionClockSync); ok && clock.Status != NodeConditionUnknown {
		value := 1
		if clock.Status == NodeConditionFail {
			value = 0
		}
		series("tako_node_clock_synchronized", "gauge", "Whether the node clock is NTP-synchronized")
		fmt.Fprintf(out, "tako_node_clock_synchronized{server=%s} %d\n", server, value)
	}
}

// writeNodePrometheusTextfile atomically replaces <dir>/tako_node.prom.
func writeNodePrometheusTextfile(dir string, input NodePrometheusInput) error {
	var buffer bytes.Buffer
	WriteNodePrometheus(&buffer, input, true)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+NodePrometheusFileName+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(buffer.Bytes()); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, filepath.Join(dir, NodePrometheusFileName))
}
