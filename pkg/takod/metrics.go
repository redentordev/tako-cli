package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	monitorCommandContext = exec.CommandContext
	monitorCommandPath    = "/usr/local/bin/tako-monitor.sh"
	metricsCurrentPath    = "/var/lib/tako/metrics/current.json"
)

type MetricsResponse struct {
	Collected bool            `json:"collected"`
	Metrics   json.RawMessage `json:"metrics"`
}

type PrometheusMetricsRequest struct {
	Collect     bool
	Project     string
	Environment string
	Node        string
	DataDir     string
	StartedAt   time.Time
	Now         time.Time
}

type nodeMetricsDocument struct {
	Timestamp     string                 `json:"timestamp"`
	CPUPercent    string                 `json:"cpu_percent"`
	Memory        nodeMemoryMetrics      `json:"memory"`
	Disk          nodeDiskMetrics        `json:"disk"`
	Network       nodeNetworkMetrics     `json:"network"`
	DiskIO        nodeDiskIOMetrics      `json:"disk_io"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	LoadAverage   nodeLoadAverageMetrics `json:"load_average"`
}

type nodeMemoryMetrics struct {
	TotalMB     int64  `json:"total_mb"`
	UsedMB      int64  `json:"used_mb"`
	AvailableMB int64  `json:"available_mb"`
	Percent     string `json:"percent"`
	SwapTotalMB int64  `json:"swap_total_mb"`
	SwapUsedMB  int64  `json:"swap_used_mb"`
}

type nodeDiskMetrics struct {
	TotalMB     int64  `json:"total_mb"`
	UsedMB      int64  `json:"used_mb"`
	AvailableMB int64  `json:"available_mb"`
	Percent     string `json:"percent"`
}

type nodeLoadAverageMetrics struct {
	OneMin     string `json:"1min"`
	FiveMin    string `json:"5min"`
	FifteenMin string `json:"15min"`
}

type nodeNetworkMetrics struct {
	RxBytes int64 `json:"rx_bytes"`
	TxBytes int64 `json:"tx_bytes"`
}

type nodeDiskIOMetrics struct {
	ReadSectors  int64 `json:"read_sectors"`
	WriteSectors int64 `json:"write_sectors"`
}

func ReadNodeMetrics(ctx context.Context, collect bool) (*MetricsResponse, error) {
	if collect {
		cmd := monitorCommandContext(ctx, monitorCommandPath, "once")
		output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to collect node metrics: %w, output: %s", err, output.String())
		}
	}

	data, err := os.ReadFile(metricsCurrentPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read node metrics: %w", err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("node metrics file is not valid JSON")
	}

	return &MetricsResponse{
		Collected: collect,
		Metrics:   append(json.RawMessage(nil), data...),
	}, nil
}

func RenderPrometheusMetrics(ctx context.Context, req PrometheusMetricsRequest) (string, error) {
	normalizePrometheusMetricsRequest(&req)
	if err := validatePrometheusMetricsRequest(req); err != nil {
		return "", err
	}

	response, err := ReadNodeMetrics(ctx, req.Collect)
	if err != nil {
		return "", err
	}
	var metrics nodeMetricsDocument
	if err := json.Unmarshal(response.Metrics, &metrics); err != nil {
		return "", fmt.Errorf("failed to parse node metrics: %w", err)
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	labels := []prometheusLabel{{Name: "node", Value: req.Node}}
	var b strings.Builder
	metadata := map[string]bool{}

	writePrometheusMetric(&b, metadata, "tako_node_up", "Whether takod can read the latest node metrics.", "gauge", labels, 1)
	if !req.StartedAt.IsZero() {
		writePrometheusMetric(&b, metadata, "tako_takod_uptime_seconds", "Seconds since takod started.", "gauge", labels, now.Sub(req.StartedAt).Seconds())
	}
	writePrometheusMetric(&b, metadata, "tako_node_cpu_percent", "Node CPU usage percent.", "gauge", labels, parseMetricFloat(metrics.CPUPercent))
	writePrometheusMetric(&b, metadata, "tako_node_memory_percent", "Node memory usage percent.", "gauge", labels, parseMetricFloat(metrics.Memory.Percent))
	writePrometheusMetric(&b, metadata, "tako_node_disk_percent", "Node disk usage percent.", "gauge", labels, parseMetricFloat(metrics.Disk.Percent))
	writePrometheusMetric(&b, metadata, "tako_node_uptime_seconds", "Node OS uptime in seconds.", "gauge", labels, float64(metrics.UptimeSeconds))
	writePrometheusMetric(&b, metadata, "tako_node_memory_bytes", "Node memory by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "total"}), mbToBytes(metrics.Memory.TotalMB))
	writePrometheusMetric(&b, metadata, "tako_node_memory_bytes", "Node memory by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "used"}), mbToBytes(metrics.Memory.UsedMB))
	writePrometheusMetric(&b, metadata, "tako_node_memory_bytes", "Node memory by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "available"}), mbToBytes(metrics.Memory.AvailableMB))
	writePrometheusMetric(&b, metadata, "tako_node_memory_bytes", "Node memory by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "swap_total"}), mbToBytes(metrics.Memory.SwapTotalMB))
	writePrometheusMetric(&b, metadata, "tako_node_memory_bytes", "Node memory by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "swap_used"}), mbToBytes(metrics.Memory.SwapUsedMB))
	writePrometheusMetric(&b, metadata, "tako_node_disk_bytes", "Node disk by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "total"}), mbToBytes(metrics.Disk.TotalMB))
	writePrometheusMetric(&b, metadata, "tako_node_disk_bytes", "Node disk by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "used"}), mbToBytes(metrics.Disk.UsedMB))
	writePrometheusMetric(&b, metadata, "tako_node_disk_bytes", "Node disk by kind in bytes.", "gauge", append(labels, prometheusLabel{Name: "kind", Value: "available"}), mbToBytes(metrics.Disk.AvailableMB))
	writePrometheusMetric(&b, metadata, "tako_node_load_average", "Node load average by window.", "gauge", append(labels, prometheusLabel{Name: "window", Value: "1m"}), parseMetricFloat(metrics.LoadAverage.OneMin))
	writePrometheusMetric(&b, metadata, "tako_node_load_average", "Node load average by window.", "gauge", append(labels, prometheusLabel{Name: "window", Value: "5m"}), parseMetricFloat(metrics.LoadAverage.FiveMin))
	writePrometheusMetric(&b, metadata, "tako_node_load_average", "Node load average by window.", "gauge", append(labels, prometheusLabel{Name: "window", Value: "15m"}), parseMetricFloat(metrics.LoadAverage.FifteenMin))
	writePrometheusMetric(&b, metadata, "tako_node_network_bytes_total", "Node network bytes by direction.", "counter", append(labels, prometheusLabel{Name: "direction", Value: "rx"}), float64(metrics.Network.RxBytes))
	writePrometheusMetric(&b, metadata, "tako_node_network_bytes_total", "Node network bytes by direction.", "counter", append(labels, prometheusLabel{Name: "direction", Value: "tx"}), float64(metrics.Network.TxBytes))
	writePrometheusMetric(&b, metadata, "tako_node_disk_io_sectors_total", "Node disk IO sectors by direction.", "counter", append(labels, prometheusLabel{Name: "direction", Value: "read"}), float64(metrics.DiskIO.ReadSectors))
	writePrometheusMetric(&b, metadata, "tako_node_disk_io_sectors_total", "Node disk IO sectors by direction.", "counter", append(labels, prometheusLabel{Name: "direction", Value: "write"}), float64(metrics.DiskIO.WriteSectors))
	if timestamp := parseMetricTimestamp(metrics.Timestamp); !timestamp.IsZero() {
		writePrometheusMetric(&b, metadata, "tako_node_metrics_timestamp_seconds", "Unix timestamp of the node metrics sample.", "gauge", labels, float64(timestamp.Unix()))
	}

	if req.Project != "" && req.Environment != "" {
		if err := appendProjectPrometheusMetrics(ctx, &b, metadata, req); err != nil {
			return "", err
		}
	}

	return b.String(), nil
}

func normalizePrometheusMetricsRequest(req *PrometheusMetricsRequest) {
	req.Project = strings.TrimSpace(req.Project)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Node = strings.TrimSpace(req.Node)
}

func validatePrometheusMetricsRequest(req PrometheusMetricsRequest) error {
	if (req.Project == "") != (req.Environment == "") {
		return fmt.Errorf("project and environment must be provided together")
	}
	if req.Project != "" && !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if req.Environment != "" && !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	return nil
}

func appendProjectPrometheusMetrics(ctx context.Context, b *strings.Builder, metadata map[string]bool, req PrometheusMetricsRequest) error {
	actual, err := GatherActualState(ctx, req.Project, req.Environment)
	if err != nil {
		return err
	}
	desired, err := readDesiredReplicasForPrometheus(ctx, req.DataDir, req.Project, req.Environment)
	if err != nil {
		return err
	}

	serviceNames := make([]string, 0, len(actual.Services)+len(desired))
	seen := map[string]bool{}
	for service := range actual.Services {
		seen[service] = true
		serviceNames = append(serviceNames, service)
	}
	for service := range desired {
		if !seen[service] {
			serviceNames = append(serviceNames, service)
		}
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		labels := []prometheusLabel{
			{Name: "node", Value: req.Node},
			{Name: "project", Value: req.Project},
			{Name: "environment", Value: req.Environment},
			{Name: "service", Value: serviceName},
		}
		running := 0
		if service := actual.Services[serviceName]; service != nil {
			running = service.Replicas
		}
		writePrometheusMetric(b, metadata, "tako_service_replicas", "Service replica count by state.", "gauge", append(labels, prometheusLabel{Name: "state", Value: "running"}), float64(running))
		if desiredReplicas, ok := desired[serviceName]; ok {
			writePrometheusMetric(b, metadata, "tako_service_replicas", "Service replica count by state.", "gauge", append(labels, prometheusLabel{Name: "state", Value: "desired"}), float64(desiredReplicas))
		}
	}

	appendLeasePrometheusMetrics(ctx, b, metadata, req)
	appendDeploymentPrometheusMetrics(ctx, b, metadata, req)
	return nil
}

func readDesiredReplicasForPrometheus(ctx context.Context, dataDir string, project string, environment string) (map[string]int, error) {
	response, err := ReadStateDocument(ctx, dataDir, StateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    stateDocumentDesired,
	})
	if err != nil || !response.Found {
		return nil, err
	}
	var desired struct {
		Services map[string]struct {
			Replicas int `json:"replicas"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(response.Content), &desired); err != nil {
		return nil, fmt.Errorf("failed to parse desired state for metrics: %w", err)
	}
	replicas := make(map[string]int, len(desired.Services))
	for serviceName, service := range desired.Services {
		if isSafeServiceName(serviceName) {
			replicas[serviceName] = service.Replicas
		}
	}
	return replicas, nil
}

func appendLeasePrometheusMetrics(ctx context.Context, b *strings.Builder, metadata map[string]bool, req PrometheusMetricsRequest) {
	labels := []prometheusLabel{
		{Name: "node", Value: req.Node},
		{Name: "project", Value: req.Project},
		{Name: "environment", Value: req.Environment},
	}
	response, err := ReadLease(ctx, req.DataDir, LeaseRequest{Project: req.Project, Environment: req.Environment})
	if err != nil || response == nil || !response.Found || response.Lease == nil {
		writePrometheusMetric(b, metadata, "tako_lease_held", "Whether a project environment lease is currently held.", "gauge", labels, 0)
		return
	}
	writePrometheusMetric(b, metadata, "tako_lease_held", "Whether a project environment lease is currently held.", "gauge", append(labels, prometheusLabel{Name: "operation", Value: response.Lease.Operation}), 1)
	writePrometheusMetric(b, metadata, "tako_lease_expires_timestamp_seconds", "Unix timestamp when the current lease expires.", "gauge", labels, float64(response.Lease.ExpiresAt.Unix()))
}

func appendDeploymentPrometheusMetrics(ctx context.Context, b *strings.Builder, metadata map[string]bool, req PrometheusMetricsRequest) {
	deployment, ok := latestDeploymentForPrometheus(ctx, req.DataDir, req.Project, req.Environment)
	if !ok {
		return
	}
	labels := []prometheusLabel{
		{Name: "node", Value: req.Node},
		{Name: "project", Value: req.Project},
		{Name: "environment", Value: req.Environment},
	}
	statuses := []string{"success", "failed", "rolled_back", "in_progress"}
	for _, status := range statuses {
		value := 0.0
		if deployment.Status == status {
			value = 1
		}
		writePrometheusMetric(b, metadata, "tako_deployment_last_status", "Latest deployment status by state.", "gauge", append(labels, prometheusLabel{Name: "status", Value: status}), value)
	}
	if !deployment.Timestamp.IsZero() {
		writePrometheusMetric(b, metadata, "tako_deployment_last_timestamp_seconds", "Unix timestamp of the latest deployment.", "gauge", labels, float64(deployment.Timestamp.Unix()))
	}
	if deployment.Duration > 0 {
		writePrometheusMetric(b, metadata, "tako_deployment_last_duration_seconds", "Latest deployment duration in seconds.", "gauge", labels, deployment.Duration.Seconds())
	}
}

type prometheusDeployment struct {
	Status    string        `json:"status"`
	Timestamp time.Time     `json:"timestamp"`
	Duration  time.Duration `json:"duration"`
}

func latestDeploymentForPrometheus(ctx context.Context, dataDir string, project string, environment string) (prometheusDeployment, bool) {
	response, err := ReadStateDocument(ctx, dataDir, StateDocumentRequest{
		Project:     project,
		Environment: environment,
		Document:    stateDocumentHistory,
	})
	if err != nil || !response.Found {
		return prometheusDeployment{}, false
	}
	var history struct {
		Deployments []prometheusDeployment `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(response.Content), &history); err != nil {
		return prometheusDeployment{}, false
	}
	var latest prometheusDeployment
	for _, deployment := range history.Deployments {
		if latest.Timestamp.IsZero() || deployment.Timestamp.After(latest.Timestamp) {
			latest = deployment
		}
	}
	return latest, !latest.Timestamp.IsZero()
}

type prometheusLabel struct {
	Name  string
	Value string
}

func writePrometheusMetric(b *strings.Builder, metadata map[string]bool, name string, help string, metricType string, labels []prometheusLabel, value float64) {
	if !metadata[name] {
		fmt.Fprintf(b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(b, "# TYPE %s %s\n", name, metricType)
		metadata[name] = true
	}
	fmt.Fprintf(b, "%s%s %s\n", name, prometheusLabels(labels), strconv.FormatFloat(value, 'f', -1, 64))
}

func prometheusLabels(labels []prometheusLabel) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for index, label := range labels {
		if index > 0 {
			b.WriteByte(',')
		}
		b.WriteString(label.Name)
		b.WriteString("=\"")
		b.WriteString(escapePrometheusLabel(label.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapePrometheusLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}

func parseMetricFloat(value string) float64 {
	value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseMetricTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func mbToBytes(value int64) float64 {
	return float64(value * 1024 * 1024)
}
