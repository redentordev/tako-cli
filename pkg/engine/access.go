package engine

import (
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// KindAccessResult identifies a serialized proxy access-log result document.
const KindAccessResult = "AccessResult"

// AccessNodeResult is the serializable outcome for one streamed proxy node.
type AccessNodeResult struct {
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// AccessResult is the serializable outcome of streaming proxy access logs;
// the entries themselves stream as access.line events.
type AccessResult struct {
	APIVersion  string             `json:"apiVersion"`
	Kind        string             `json:"kind"`
	Project     string             `json:"project"`
	Environment string             `json:"environment"`
	Service     string             `json:"service,omitempty"`
	Tail        int                `json:"tail"`
	Follow      bool               `json:"follow"`
	Status      string             `json:"status"`
	Nodes       []AccessNodeResult `json:"nodes"`
	StartedAt   time.Time          `json:"startedAt"`
	Duration    float64            `json:"durationSeconds"`
	Message     string             `json:"message,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// NewAccessResult assembles the access result document from per-node stream
// outcomes and the aggregate summary error, mirroring LogsResult semantics.
func NewAccessResult(project string, environment string, service string, tail int, follow bool, startedAt time.Time, nodes []AccessNodeResult, summaryErr error) AccessResult {
	result := AccessResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindAccessResult,
		Project:     project,
		Environment: environment,
		Service:     service,
		Tail:        tail,
		Follow:      follow,
		Status:      logsStatusSuccess,
		Nodes:       nodes,
		StartedAt:   startedAt,
		Duration:    time.Since(startedAt).Seconds(),
	}
	if summaryErr != nil {
		result.Status = logsStatusFailed
		result.Error = summaryErr.Error()
		return result
	}
	result.Message = fmt.Sprintf("streamed access logs from %d node(s)", len(nodes))
	return result
}
