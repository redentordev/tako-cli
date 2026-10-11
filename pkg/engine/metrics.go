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
	// Status is takod's node status document (`GET /v1/node/status`,
	// conditions plus OS updates and reboot state) verbatim. It is absent
	// for nodes whose takod predates `node.status-v1`.
	Status json.RawMessage `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
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

// KindMetricsHistoryResult identifies a serialized metrics history document.
const KindMetricsHistoryResult = "MetricsHistoryResult"

// MetricsHistoryNode is one node's history read. History carries the takod
// `/v1/metrics/history` document verbatim; it is empty when the read failed
// and Error says why (for example a takod without
// `node.metrics-history-v1`, which names `tako upgrade servers`).
type MetricsHistoryNode struct {
	Server  string          `json:"server"`
	Host    string          `json:"host,omitempty"`
	History json.RawMessage `json:"history,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// MetricsHistoryResult is the serializable outcome of
// `tako metrics --since`. Since, Until and Step echo the request (Until and
// Step are empty when defaulted). All nodes failing exits 1; a partial read
// exits 6.
type MetricsHistoryResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Server is the --server filter when one was requested.
	Server string               `json:"server,omitempty"`
	Since  string               `json:"since"`
	Until  string               `json:"until,omitempty"`
	Step   string               `json:"step,omitempty"`
	Nodes  []MetricsHistoryNode `json:"nodes"`
	Error  string               `json:"error,omitempty"`
}
