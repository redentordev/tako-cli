package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

const (
	// KindDeployPlan identifies a serialized deployment plan document.
	KindDeployPlan = "DeployPlan"
	// KindDeployResult identifies a serialized deployment result document.
	KindDeployResult = "DeployResult"
)

// DeployRequest describes one deploy operation. Config must be loaded and
// validated; Environment must be resolved. All fields mirror the CLI flags.
type DeployRequest struct {
	Config      *config.Config
	Environment string
	// WorkDir is the project directory for git metadata and local state.
	// Empty means the current directory.
	WorkDir string

	Service  string
	Image    string
	Source   string
	Archive  string
	Revision string

	BuildStrategy string
	SkipBuild     bool
	AllowDirty    bool
	Force         bool
	// Verbose enables detailed progress from the deployer and dependency
	// resolver; debug-level events are emitted regardless and filtered by
	// renderers.
	Verbose bool

	SkipDomainCheck bool
	StrictDomains   bool
	DomainTimeout   time.Duration
	DomainTargets   []string
}

// GitInfo captures the source commit recorded with a deployment.
type GitInfo struct {
	Commit      string `json:"commit,omitempty"`
	CommitShort string `json:"commitShort,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
}

// PlanChange is one service-level change in a deployment plan.
type PlanChange struct {
	Type    string   `json:"type"`
	Service string   `json:"service"`
	Reasons []string `json:"reasons,omitempty"`
}

// DeployPlan is the serializable outcome of PlanDeploy: what would change,
// whether that needs confirmation, and the identity of what would deploy.
// It feeds confirmation screens and the --plan-only machine output.
type DeployPlan struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Revision    string       `json:"revision"`
	Source      string       `json:"source"`
	Git         *GitInfo     `json:"git,omitempty"`
	Servers     []string     `json:"servers"`
	Services    []string     `json:"services"`
	Changes     []PlanChange `json:"changes"`
	Destructive bool         `json:"destructive"`
	Empty       bool         `json:"empty"`

	// HumanText is reconcile plan text exactly as the CLI displays it.
	// Excluded from the plan hash.
	HumanText string `json:"humanText,omitempty"`
}

// Hash returns a stable fingerprint of the plan's decision-relevant fields,
// used to detect drift between a reviewed plan and a later apply.
func (p *DeployPlan) Hash() string {
	if p == nil {
		return ""
	}
	hashable := *p
	hashable.HumanText = ""
	payload, err := json.Marshal(hashable)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ServiceOutcome reports what happened to one service during apply.
type ServiceOutcome struct {
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
	Action   string `json:"action"`
	Replicas int    `json:"replicas,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Service outcome actions.
const (
	OutcomeDeployed = "deployed"
	OutcomeWarmed   = "warmed"
	OutcomeRemoved  = "removed"
	OutcomeUpToDate = "up_to_date"
	OutcomeFailed   = "failed"
)

// DeployResult is the serializable outcome of ApplyDeploy.
type DeployResult struct {
	APIVersion  string                   `json:"apiVersion"`
	Kind        string                   `json:"kind"`
	Project     string                   `json:"project"`
	Environment string                   `json:"environment"`
	Status      takoapi.DeploymentStatus `json:"status"`
	Revision    string                   `json:"revision,omitempty"`
	Git         *GitInfo                 `json:"git,omitempty"`
	Services    []ServiceOutcome         `json:"services"`
	// ManualPending lists services warmed for manual promotion.
	ManualPending []string  `json:"manualPending,omitempty"`
	URLs          []string  `json:"urls,omitempty"`
	InternalURLs  []string  `json:"internalUrls,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	Duration      float64   `json:"durationSeconds"`
	PlanHash      string    `json:"planHash,omitempty"`
	Message       string    `json:"message,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func newDeployPlanDocument(project string, environment string, plan *reconcile.ReconciliationPlan) DeployPlan {
	doc := DeployPlan{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindDeployPlan,
		Project:     project,
		Environment: environment,
		Changes:     make([]PlanChange, 0, len(plan.Changes)),
	}
	for _, change := range plan.Changes {
		if change.Type == reconcile.ChangeNone {
			continue
		}
		doc.Changes = append(doc.Changes, PlanChange{
			Type:    string(change.Type),
			Service: change.ServiceName,
			Reasons: append([]string(nil), change.Reasons...),
		})
	}
	sort.Slice(doc.Changes, func(i, j int) bool {
		if doc.Changes[i].Service != doc.Changes[j].Service {
			return doc.Changes[i].Service < doc.Changes[j].Service
		}
		return doc.Changes[i].Type < doc.Changes[j].Type
	})
	return doc
}

func sortedServiceNames(services map[string]config.ServiceConfig) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
