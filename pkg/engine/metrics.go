package engine

import (
	"encoding/json"
	"time"
)

// KindMetricsResult identifies a serialized metrics result document.
const KindMetricsResult = "MetricsResult"

// MetricsNodeSample is one node's metrics read. Metrics carries the takod
// `/v1/metrics` document verbatim (the monitoring agent's schema, e.g.
// `cpu_percent`, `memory`, `disk`, `load_average`); it is empty when the
// read failed and Error says why.
type MetricsNodeSample struct {
	Server  string          `json:"server"`
	Host    string          `json:"host,omitempty"`
	Metrics json.RawMessage `json:"metrics,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// MetricsResult is the serializable outcome of a single-shot `tako metrics`.
// All nodes failing exits 1; a partial read exits 6; live mode is
// interactive-only and never emits this document.
type MetricsResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Server is the --server filter when one was requested.
	Server      string              `json:"server,omitempty"`
	CollectedAt time.Time           `json:"collectedAt"`
	Nodes       []MetricsNodeSample `json:"nodes"`
	Error       string              `json:"error,omitempty"`
}
