package takod

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
)

// Monitor check types carried in MonitorSpec.CheckType.
const (
	MonitorCheckHTTP      = "http"
	MonitorCheckContainer = "container"
)

// MonitorSpec is one service health check declared by the deploying CLI.
// HTTP checks run only on CheckerNode (the node that serves the service's
// proxy route); container checks run on every node holding a replica.
type MonitorSpec struct {
	Service         string `json:"service"`
	CheckType       string `json:"checkType"`
	IntervalSeconds int64  `json:"intervalSeconds"`
	TimeoutSeconds  int64  `json:"timeoutSeconds"`
	AlertAfter      int    `json:"alertAfter"`
	RecoverAfter    int    `json:"recoverAfter"`
	Domain          string `json:"domain,omitempty"`
	HealthPath      string `json:"healthPath,omitempty"`
	CheckerNode     string `json:"checkerNode,omitempty"`
	BasicAuth       bool   `json:"basicAuth,omitempty"`
}

// HostAlertPolicy enables host threshold alerts. Zero percents disable the
// corresponding reason.
type HostAlertPolicy struct {
	DiskPercent   int     `json:"diskPercent,omitempty"`
	InodesPercent int     `json:"inodesPercent,omitempty"`
	MemoryPercent int     `json:"memoryPercent,omitempty"`
	CPUPercent    int     `json:"cpuPercent,omitempty"`
	CPUForSeconds int64   `json:"cpuForSeconds,omitempty"`
	LoadPerCore   float64 `json:"loadPerCore,omitempty"`
	Conditions    bool    `json:"conditions,omitempty"`
}

// ContainerAlertPolicy enables OOM-kill events and crash-loop alerts.
type ContainerAlertPolicy struct {
	OOM                    bool  `json:"oom,omitempty"`
	CrashLoopRestarts      int   `json:"crashLoopRestarts,omitempty"`
	CrashLoopWindowSeconds int64 `json:"crashLoopWindowSeconds,omitempty"`
}

// CertificateAlertPolicy enables certificate expiry alerts.
type CertificateAlertPolicy struct {
	WarnBeforeSeconds     int64 `json:"warnBeforeSeconds"`
	CriticalBeforeSeconds int64 `json:"criticalBeforeSeconds"`
}

func (request AlertConfigRequest) sourcePolicy() SourceAlertPolicy {
	return SourceAlertPolicy{Monitors: request.Monitors, Host: request.Host, Containers: request.Containers, Certificates: request.Certificates}
}

// validateSourceAlertRequest validates the additive node-side monitoring
// fields of an alert request.
func validateSourceAlertRequest(request AlertConfigRequest) error {
	if err := validateSourceAlertPolicy(request.sourcePolicy()); err != nil {
		return err
	}
	for service, endpoint := range request.ServiceTargets {
		if !scopeMonitorsService(request.sourcePolicy(), service) {
			return fmt.Errorf("service alert target for unmonitored service")
		}
		if normalized, err := alerttarget.NormalizeAndValidate("webhook", endpoint); err != nil || normalized != endpoint {
			return fmt.Errorf("invalid service alert target")
		}
	}
	if sourcesEnabled(request) && len(configuredAlertProviders(request.Targets)) == 0 && len(request.ServiceTargets) == 0 {
		return fmt.Errorf("enabled monitoring requires a target")
	}
	return nil
}

func normalizeServiceTargets(targets map[string]string) map[string]string {
	if len(targets) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(targets))
	for service, endpoint := range targets {
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			normalized[service] = endpoint
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
