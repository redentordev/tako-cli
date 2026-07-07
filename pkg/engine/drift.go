package engine

import "time"

// KindDriftResult identifies a serialized drift result document.
const KindDriftResult = "DriftResult"

// DriftEntry is one detected desired-vs-actual difference.
type DriftEntry struct {
	Service  string            `json:"service"`
	Type     string            `json:"type"`
	Severity string            `json:"severity"`
	Expected string            `json:"expected"`
	Actual   string            `json:"actual"`
	Details  map[string]string `json:"details,omitempty"`
}

// DriftResult is the serializable outcome of a single-shot `tako drift`
// check. Drifted mirrors exit code 6; watch mode is interactive-only and
// never emits this document.
type DriftResult struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Drifted     bool         `json:"drifted"`
	Drifts      []DriftEntry `json:"drifts,omitempty"`
	ServicesOK  []string     `json:"servicesOk,omitempty"`
	CheckedAt   time.Time    `json:"checkedAt"`
	Duration    float64      `json:"durationSeconds"`
}
