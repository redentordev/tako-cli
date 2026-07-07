package engine

import (
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

// KindStatsResult identifies a serialized container-stats result document.
const KindStatsResult = "StatsResult"

// StatsNodeSample is one node's point-in-time container stats. Containers
// reuses the takod stats schema (`name`, `cpuPercent`, `memUsage`,
// `memPercent`, `netIO`, `blockIO`, `pids`); it is empty when the read
// failed and Error says why.
type StatsNodeSample struct {
	Server     string                `json:"server"`
	Host       string                `json:"host,omitempty"`
	Containers []takod.ContainerStat `json:"containers,omitempty"`
	Error      string                `json:"error,omitempty"`
}

// StatsResult is the serializable outcome of a point-in-time `tako stats`.
// All nodes failing exits 1; a partial read exits 6. `--follow` streams the
// same node samples as `stats.sample` events instead; `--live` is
// interactive-only and never emits this document.
type StatsResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Service is the --service filter when one was requested; All mirrors
	// --all (include containers beyond this project).
	Service     string            `json:"service,omitempty"`
	All         bool              `json:"all,omitempty"`
	CollectedAt time.Time         `json:"collectedAt"`
	Nodes       []StatsNodeSample `json:"nodes"`
	Error       string            `json:"error,omitempty"`
}
