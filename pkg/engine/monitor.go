package engine

import "time"

// KindMonitorResult is the machine document of `tako monitor`.
const KindMonitorResult = "MonitorResult"

// MonitorResult statuses.
const (
	MonitorStatusOK          = "ok"
	MonitorStatusAttention   = "attention"
	MonitorStatusUnreachable = "unreachable"
	MonitorStatusUnsupported = "unsupported"
)

// MonitorSubject is one alert subject as seen by one node. Every field comes
// from a closed vocabulary, a public name, a timestamp or a number.
type MonitorSubject struct {
	Kind          string            `json:"kind"`
	Subject       string            `json:"subject"`
	State         string            `json:"state"`
	Severity      string            `json:"severity,omitempty"`
	Reasons       []string          `json:"reasons,omitempty"`
	Since         *time.Time        `json:"since,omitempty"`
	LastCheckedAt *time.Time        `json:"lastCheckedAt,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
}

// MonitorNode is one node's monitoring status.
type MonitorNode struct {
	Node                string           `json:"node"`
	Reachable           bool             `json:"reachable"`
	Supported           bool             `json:"supported"`
	Error               string           `json:"error,omitempty"`
	Subjects            []MonitorSubject `json:"subjects"`
	PendingDeliveries   int              `json:"pendingDeliveries"`
	ExhaustedDeliveries int              `json:"exhaustedDeliveries"`
}

// MonitorCounts counts subjects by state group. Flapping and stale count as
// alerting and unknown respectively; maintenance counts as paused.
type MonitorCounts struct {
	OK       int `json:"ok"`
	Alerting int `json:"alerting"`
	Paused   int `json:"paused"`
	Unknown  int `json:"unknown"`
}

// MonitorResult is the `tako monitor` result document.
type MonitorResult struct {
	APIVersion  string        `json:"apiVersion"`
	Kind        string        `json:"kind"`
	Project     string        `json:"project"`
	Environment string        `json:"environment"`
	Service     string        `json:"service,omitempty"`
	Status      string        `json:"status"`
	Counts      MonitorCounts `json:"counts"`
	Nodes       []MonitorNode `json:"nodes"`
	// NotDeployed lists services with monitoring enabled in the local
	// config that no node reports (state unknown, reason not_deployed).
	NotDeployed []MonitorSubject `json:"notDeployed,omitempty"`
	Error       string           `json:"error,omitempty"`
}
