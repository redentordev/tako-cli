package takod

// Host threshold alerts (design D6). The evaluator reads samples through
// HostSampleSource and never opens current.json itself, so the native
// collector from add-server-status-history can replace the legacy adapter
// by setting ServerOptions.HostSampleSource.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/notification"
)

const (
	hostEvaluateInterval   = 60 * time.Second
	hostMetricsStaleAfter  = 5 * time.Minute
	hostCriticalPercent    = 95
	hostSubjectRoot        = "root"
	hostSubjectDocker      = "docker"
	hostReasonDisk         = "disk_high"
	hostReasonInodes       = "inodes_high"
	hostReasonMemory       = "memory_high"
	hostReasonCPU          = "cpu_high"
	hostReasonLoad         = "load_high"
	hostReasonMetricsStale = "metrics_stale"
)

// HostFilesystemSample is one filesystem's usage.
type HostFilesystemSample struct {
	UsedPercent   float64
	UsedMB        int64
	TotalMB       int64
	InodesPercent float64
	// HasInodes is false when the source has no inode data.
	HasInodes bool
}

// HostSample is one host metrics sample.
type HostSample struct {
	CPUPercent    float64
	MemoryPercent float64
	MemoryUsedMB  int64
	MemoryTotalMB int64
	Load5         float64
	// CPUCount is the number of CPUs used for load per core; 0 means
	// runtime.NumCPU().
	CPUCount int
	Root     HostFilesystemSample
	// Docker is the Docker data-root filesystem when it is separate from
	// root (native source only).
	Docker *HostFilesystemSample
}

// HostSampleSource provides the newest host sample and its time.
type HostSampleSource interface {
	Latest() (HostSample, time.Time, error)
}

// legacyHostSampleSource adapts /var/lib/tako/metrics/current.json.
type legacyHostSampleSource struct {
	path func() string
}

func newLegacyHostSampleSource() HostSampleSource {
	return legacyHostSampleSource{path: func() string { return metricsCurrentPath }}
}

type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	text = strings.TrimSuffix(text, "%")
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("invalid number")
	}
	*f = flexibleFloat(value)
	return nil
}

type legacyMetricsDocument struct {
	Timestamp  string        `json:"timestamp"`
	CPUPercent flexibleFloat `json:"cpu_percent"`
	Memory     struct {
		TotalMB flexibleFloat `json:"total_mb"`
		UsedMB  flexibleFloat `json:"used_mb"`
		Percent flexibleFloat `json:"percent"`
	} `json:"memory"`
	Disk struct {
		TotalMB flexibleFloat `json:"total_mb"`
		UsedMB  flexibleFloat `json:"used_mb"`
		Percent flexibleFloat `json:"percent"`
	} `json:"disk"`
	LoadAverage struct {
		Five flexibleFloat `json:"5min"`
	} `json:"load_average"`
}

func (s legacyHostSampleSource) Latest() (HostSample, time.Time, error) {
	path := s.path()
	info, err := os.Stat(path)
	if err != nil {
		return HostSample{}, time.Time{}, err
	}
	if info.Size() > 1<<20 {
		return HostSample{}, time.Time{}, errors.New("metrics document too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return HostSample{}, time.Time{}, err
	}
	var document legacyMetricsDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return HostSample{}, time.Time{}, err
	}
	sampledAt := info.ModTime().UTC()
	if document.Timestamp != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, document.Timestamp); parseErr == nil {
			sampledAt = parsed.UTC()
		}
	}
	return HostSample{
		CPUPercent: float64(document.CPUPercent), MemoryPercent: float64(document.Memory.Percent),
		MemoryUsedMB: int64(document.Memory.UsedMB), MemoryTotalMB: int64(document.Memory.TotalMB),
		Load5: float64(document.LoadAverage.Five),
		Root: HostFilesystemSample{
			UsedPercent: float64(document.Disk.Percent), UsedMB: int64(document.Disk.UsedMB), TotalMB: int64(document.Disk.TotalMB),
		},
	}, sampledAt, nil
}

func hostAlertSourceSpec() AlertSourceSpec {
	normal := string(notification.EventResourceNormal)
	return AlertSourceSpec{
		Kind:       AlertKindHost,
		NodeScoped: true,
		Reasons: map[string]Severity{
			hostReasonDisk: SeverityWarning, hostReasonInodes: SeverityWarning, hostReasonMemory: SeverityWarning,
			hostReasonCPU: SeverityWarning, hostReasonLoad: SeverityWarning, hostReasonMetricsStale: SeverityWarning,
		},
		DetailKeys: []string{"percent", "threshold", "usedMb", "totalMb", "inodesPercent", "inodesThreshold", "memoryPercent", "memoryThreshold", "cpuPercent", "cpuThreshold", "loadPerCore", "loadThreshold"},
		Events: map[string]string{
			hostReasonDisk: string(notification.EventHighDisk), hostReasonInodes: string(notification.EventHighInodes),
			hostReasonMemory: string(notification.EventHighMemory), hostReasonCPU: string(notification.EventHighCPU),
			hostReasonLoad: string(notification.EventHighLoad), hostReasonMetricsStale: string(notification.EventMetricsStale),
		},
		Recoveries: map[string]string{
			hostReasonDisk: normal, hostReasonInodes: normal, hostReasonMemory: normal,
			hostReasonCPU: normal, hostReasonLoad: normal, hostReasonMetricsStale: normal,
		},
	}
}

// hostAlertEvaluator evaluates host thresholds every 60s for each scope
// with a host section, against that scope's thresholds.
type hostAlertEvaluator struct {
	alerts *SourceAlertManager
	source HostSampleSource
	now    func() time.Time

	mu sync.Mutex
	// cpuHighSince tracks sustained CPU per scope.
	cpuHighSince map[string]time.Time
}

func newHostAlertEvaluator(alerts *SourceAlertManager, source HostSampleSource) *hostAlertEvaluator {
	if source == nil {
		source = newLegacyHostSampleSource()
	}
	return &hostAlertEvaluator{alerts: alerts, source: source, now: func() time.Time { return time.Now().UTC() }, cpuHighSince: map[string]time.Time{}}
}

func (e *hostAlertEvaluator) Run(ctx context.Context) {
	ticker := time.NewTicker(hostEvaluateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.EvaluateOnce()
		}
	}
}

func formatMetric(value float64) string {
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64)
}

// EvaluateOnce reads one sample and reports the root (and docker) subject
// for every scope subscribing to host alerts.
func (e *hostAlertEvaluator) EvaluateOnce() {
	if !e.alerts.AnySubscribed(AlertKindHost) {
		return
	}
	now := e.now()
	sample, sampledAt, err := e.source.Latest()
	stale := err != nil || sampledAt.IsZero() || now.Sub(sampledAt) > hostMetricsStaleAfter
	for _, scope := range e.alerts.ScopePolicies() {
		policy := scope.Policy.Host
		if policy == nil || !e.alerts.Subscribed(scope.Project, scope.Environment, AlertKindHost) {
			continue
		}
		base := LevelObservation{
			Project: scope.Project, Environment: scope.Environment, Kind: AlertKindHost,
			Interval: hostEvaluateInterval, ObservedAt: now,
		}
		if stale {
			root := base
			root.Subject = hostSubjectRoot
			root.Failing = []string{hostReasonMetricsStale}
			_ = e.alerts.ReportLevel(root)
			continue
		}
		root := base
		root.Subject = hostSubjectRoot
		root.Details = map[string]string{}
		root.Severities = map[string]Severity{}
		evaluateFilesystem(&root, sample.Root, policy)
		if policy.MemoryPercent > 0 {
			root.Details["memoryPercent"] = formatMetric(sample.MemoryPercent)
			root.Details["memoryThreshold"] = strconv.Itoa(policy.MemoryPercent)
			if sample.MemoryPercent >= float64(policy.MemoryPercent) {
				root.Failing = append(root.Failing, hostReasonMemory)
			}
		}
		if policy.CPUPercent > 0 {
			key := scope.Project + "/" + scope.Environment
			root.Details["cpuPercent"] = formatMetric(sample.CPUPercent)
			root.Details["cpuThreshold"] = strconv.Itoa(policy.CPUPercent)
			e.mu.Lock()
			if sample.CPUPercent >= float64(policy.CPUPercent) {
				since, ok := e.cpuHighSince[key]
				if !ok {
					since = now
					e.cpuHighSince[key] = now
				}
				if now.Sub(since) >= time.Duration(policy.CPUForSeconds)*time.Second {
					root.Failing = append(root.Failing, hostReasonCPU)
				}
			} else {
				delete(e.cpuHighSince, key)
			}
			e.mu.Unlock()
		}
		if policy.LoadPerCore > 0 {
			cpus := sample.CPUCount
			if cpus <= 0 {
				cpus = runtime.NumCPU()
			}
			perCore := sample.Load5 / float64(cpus)
			root.Details["loadPerCore"] = strconv.FormatFloat(math.Round(perCore*100)/100, 'f', -1, 64)
			root.Details["loadThreshold"] = strconv.FormatFloat(policy.LoadPerCore, 'f', -1, 64)
			if perCore >= policy.LoadPerCore {
				root.Failing = append(root.Failing, hostReasonLoad)
			}
		}
		_ = e.alerts.ReportLevel(root)
		if sample.Docker != nil {
			docker := base
			docker.Subject = hostSubjectDocker
			docker.Details = map[string]string{}
			docker.Severities = map[string]Severity{}
			evaluateFilesystem(&docker, *sample.Docker, policy)
			_ = e.alerts.ReportLevel(docker)
		}
	}
}

func evaluateFilesystem(observation *LevelObservation, filesystem HostFilesystemSample, policy *HostAlertPolicy) {
	observation.Details["percent"] = formatMetric(filesystem.UsedPercent)
	if filesystem.TotalMB > 0 {
		observation.Details["usedMb"] = strconv.FormatInt(filesystem.UsedMB, 10)
		observation.Details["totalMb"] = strconv.FormatInt(filesystem.TotalMB, 10)
	}
	if policy.DiskPercent > 0 {
		observation.Details["threshold"] = strconv.Itoa(policy.DiskPercent)
		if filesystem.UsedPercent >= float64(policy.DiskPercent) {
			observation.Failing = append(observation.Failing, hostReasonDisk)
			if filesystem.UsedPercent >= hostCriticalPercent {
				observation.Severities[hostReasonDisk] = SeverityCritical
			}
		}
	}
	if policy.InodesPercent > 0 && filesystem.HasInodes {
		observation.Details["inodesPercent"] = formatMetric(filesystem.InodesPercent)
		observation.Details["inodesThreshold"] = strconv.Itoa(policy.InodesPercent)
		if filesystem.InodesPercent >= float64(policy.InodesPercent) {
			observation.Failing = append(observation.Failing, hostReasonInodes)
			if filesystem.InodesPercent >= hostCriticalPercent {
				observation.Severities[hostReasonInodes] = SeverityCritical
			}
		}
	}
}

// HostMetricsStale reports whether the host sample source is stale; used by
// the Prometheus status.
func hostMetricsStale(source HostSampleSource, now time.Time) bool {
	_, sampledAt, err := source.Latest()
	return err != nil || sampledAt.IsZero() || now.Sub(sampledAt) > hostMetricsStaleAfter
}
