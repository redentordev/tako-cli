package engine

import (
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

const KindCertsResult = "CertsResult"

type CertsNodeResult struct {
	Server       string                           `json:"server"`
	Host         string                           `json:"host,omitempty"`
	Certificates []takod.ProxyCertificateMetadata `json:"certificates"`
	Error        string                           `json:"error,omitempty"`
}

type CertsResult struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Action      string            `json:"action"`
	Domain      string            `json:"domain,omitempty"`
	Nodes       []CertsNodeResult `json:"nodes"`
	StartedAt   time.Time         `json:"startedAt"`
	Duration    float64           `json:"durationSeconds"`
	Error       string            `json:"error,omitempty"`
}
