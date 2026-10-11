package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
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
	// ReleaseCommand surfaces the service's deploy.release command that
	// will run before cutover when this change applies.
	ReleaseCommand []string `json:"releaseCommand,omitempty"`
	RunCommand     []string `json:"runCommand,omitempty"`
	// Strategy is the effective deploy strategy (recreate, rolling,
	// blue_green) and StrategySource says whether tako.yaml set it
	// ("explicit") or Tako selected it ("default"). Both are empty for runs,
	// jobs and removed services.
	Strategy       string `json:"strategy,omitempty"`
	StrategySource string `json:"strategySource,omitempty"`
}

type PlanSharedBuild struct {
	Key       string   `json:"key"`
	Image     string   `json:"image"`
	Consumers []string `json:"consumers"`
}

// DeployPlan is the serializable outcome of PlanDeploy: what would change,
// whether that needs confirmation, and the identity of what would deploy.
// It feeds confirmation screens and the --plan-only machine output.
type DeployPlan struct {
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	Project      string            `json:"project"`
	Environment  string            `json:"environment"`
	Revision     string            `json:"revision"`
	Source       string            `json:"source"`
	Git          *GitInfo          `json:"git,omitempty"`
	Servers      []string          `json:"servers"`
	Services     []string          `json:"services"`
	Changes      []PlanChange      `json:"changes"`
	SharedBuilds []PlanSharedBuild `json:"sharedBuilds,omitempty"`
	Destructive  bool              `json:"destructive"`
	Empty        bool              `json:"empty"`

	// HumanText is reconcile plan text exactly as the CLI displays it.
	// Excluded from the plan hash.
	HumanText string `json:"humanText,omitempty"`
}

func planSharedBuilds(cfg *config.Config, environment string, revision string, services map[string]config.ServiceConfig) []PlanSharedBuild {
	consumers := make(map[string][]string)
	for serviceName, service := range services {
		if service.SharedBuildHash != "" {
			consumers[service.ImageFrom] = append(consumers[service.ImageFrom], serviceName)
		}
	}
	keys := make([]string, 0, len(consumers))
	for key := range consumers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]PlanSharedBuild, 0, len(keys))
	for _, key := range keys {
		sort.Strings(consumers[key])
		out = append(out, PlanSharedBuild{Key: key, Image: deployplan.SharedBuildImageRef(cfg, environment, key, revision), Consumers: consumers[key]})
	}
	return out
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

// PlanHash is Hash under the name every reviewed plan document shares
// (RollbackPlan stores its hash in a field named Hash).
func (p *DeployPlan) PlanHash() string {
	return p.Hash()
}

// ServiceOutcome reports what happened to one service during apply.
type ServiceOutcome struct {
	Name     string          `json:"name"`
	Image    string          `json:"image,omitempty"`
	Action   string          `json:"action"`
	Replicas int             `json:"replicas,omitempty"`
	Release  *ReleaseOutcome `json:"release,omitempty"`
	Run      *RunOutcome     `json:"run,omitempty"`
	Error    string          `json:"error,omitempty"`
	// Strategy is the effective deploy strategy of a long-running service.
	Strategy string `json:"strategy,omitempty"`
	// Recovery reports how a failed rollout was recovered.
	Recovery *RecoveryOutcome `json:"recovery,omitempty"`
}

type RunOutcome struct {
	Command    []string `json:"command"`
	Server     string   `json:"server"`
	Image      string   `json:"image"`
	ExitCode   int      `json:"exitCode"`
	DurationMs int64    `json:"durationMs"`
}

// ReleaseOutcome reports the service's release command run: executed once
// per applied deploy from the new image, before traffic cutover.
type ReleaseOutcome struct {
	Command    []string `json:"command"`
	Server     string   `json:"server,omitempty"`
	Image      string   `json:"image,omitempty"`
	ExitCode   int      `json:"exitCode"`
	DurationMs int64    `json:"durationMs"`
}

// releaseOutcomeFor converts the deployer's recorded release run for the
// result document; nil when the service declares no release command or it
// never ran.
func releaseOutcomeFor(d *deployer.Deployer, serviceName string) *ReleaseOutcome {
	if d == nil {
		return nil
	}
	run := d.ReleaseRunFor(serviceName)
	if run == nil {
		return nil
	}
	return &ReleaseOutcome{
		Command:    append([]string(nil), run.Command...),
		Server:     run.Server,
		Image:      run.Image,
		ExitCode:   run.ExitCode,
		DurationMs: run.DurationMs,
	}
}

// Service outcome actions.
const (
	OutcomeDeployed = "deployed"
	OutcomeWarmed   = "warmed"
	OutcomeRemoved  = "removed"
	OutcomeUpToDate = "up_to_date"
	OutcomeFailed   = "failed"
	OutcomeRan      = "ran"
	// OutcomeRestored marks a service whose rollout failed but whose
	// previous revision serves again; the deploy still fails (exit 1).
	OutcomeRestored = "restored"
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
	// DeploymentID is the deployment history record ID, set once the
	// in-progress record has been written.
	DeploymentID string `json:"deploymentId,omitempty"`
	// Hooks lists the local lifecycle hooks considered, in run order.
	Hooks []HookOutcome `json:"hooks,omitempty"`
	// Summary is the end-of-operation summary (additive).
	Summary *OperationSummary `json:"summary,omitempty"`
}

// Hook outcome statuses carried in HookOutcome.Status.
const (
	HookCompleted = "completed"
	HookFailed    = "failed"
	HookTimedOut  = "timed_out"
	HookSkipped   = "skipped"
)

// HookOutcome reports one local lifecycle hook in a result document.
// ExitCode and DurationMs are set when the hook process ran.
type HookOutcome struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	DurationMs *int64 `json:"durationMs,omitempty"`
}

// MutationInfo describes the operation a BeforeMutation callback is about to
// let proceed: the resolved target, before the first mutation.
type MutationInfo struct {
	// Operation is "rollback" or "promote".
	Operation string
	Services  []string
	Servers   []string
	// TargetDeploymentID is the rollback target deployment.
	TargetDeploymentID string
	// Revision is the promoted revision, or the rollback target's commit.
	Revision string
	Image    string
}

// BeforeMutationFunc runs after an operation has taken its lock and leases
// and resolved its target, and before any mutation. A non-nil error aborts
// the operation with that error; the lock and leases are released.
type BeforeMutationFunc func(ctx context.Context, info MutationInfo) error

func callBeforeMutation(ctx context.Context, fn BeforeMutationFunc, info MutationInfo) error {
	if fn == nil {
		return nil
	}
	return fn(ctx, info)
}

func newDeployPlanDocument(project string, environment string, plan *reconcile.ReconciliationPlan, services map[string]config.ServiceConfig) DeployPlan {
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
		planChange := PlanChange{
			Type:    string(change.Type),
			Service: change.ServiceName,
			Reasons: append([]string(nil), change.Reasons...),
		}
		if service, ok := services[change.ServiceName]; ok && service.Deploy.Release != nil {
			planChange.ReleaseCommand = append([]string(nil), service.Deploy.Release.Command...)
		}
		if service, ok := services[change.ServiceName]; ok && service.IsRun() {
			planChange.RunCommand = service.Command.Arguments()
		}
		if service, ok := services[change.ServiceName]; ok && change.Type != reconcile.ChangeRemove {
			planChange.Strategy, planChange.StrategySource = serviceStrategyReport(&service)
		}
		doc.Changes = append(doc.Changes, planChange)
	}
	sort.Slice(doc.Changes, func(i, j int) bool {
		if doc.Changes[i].Service != doc.Changes[j].Service {
			return doc.Changes[i].Service < doc.Changes[j].Service
		}
		return doc.Changes[i].Type < doc.Changes[j].Type
	})
	return doc
}

// serviceStrategyReport returns the effective deploy strategy and its source
// for plan and result documents; both are empty for runs and jobs, which
// have no rollout strategy.
func serviceStrategyReport(service *config.ServiceConfig) (string, string) {
	if service == nil || service.IsRun() || service.IsJob() {
		return "", ""
	}
	return deployplan.EffectiveDeployStrategy(service), service.DeployStrategySource()
}

func sortedServiceNames(services map[string]config.ServiceConfig) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
