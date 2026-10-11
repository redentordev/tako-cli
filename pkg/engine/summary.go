package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
)

// OperationSummary is the end-of-operation summary shared by deploy,
// rollback, promote and scale: what changed, whether it is healthy, where it
// is served, and how to undo it. The same object is carried in the result
// document's `summary` field, the `operation.summary` event, the human text
// block and the GitHub Actions step summary. Every free-text field is
// redacted before the summary leaves the engine.
type OperationSummary struct {
	// Operation is deploy, rollback, promote or scale.
	Operation   string             `json:"operation"`
	Project     string             `json:"project"`
	Environment SummaryEnvironment `json:"environment"`
	// Outcome is succeeded, warmed, up_to_date, partial or failed.
	Outcome string `json:"outcome"`
	// DeploymentID and DisplayID identify the history record the operation
	// wrote, when it wrote one.
	DeploymentID    string             `json:"deploymentId,omitempty"`
	DisplayID       string             `json:"displayId,omitempty"`
	Revision        string             `json:"revision,omitempty"`
	Commit          string             `json:"commit,omitempty"`
	StartedAt       time.Time          `json:"startedAt"`
	DurationSeconds float64            `json:"durationSeconds"`
	Services        []SummaryService   `json:"services"`
	URLs            []SummaryURL       `json:"urls,omitempty"`
	Undo            *SummaryUndo       `json:"undo,omitempty"`
	NextSteps       []SummaryNextStep  `json:"nextSteps,omitempty"`
	Error           string             `json:"error,omitempty"`
	Notes           []string           `json:"notes,omitempty"`
}

// SummaryEnvironment names the environment an operation ran against. Kind
// is "standard" for declared environments; consumers treat other values as
// opaque.
type SummaryEnvironment struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// SummaryService is one service row of an OperationSummary.
type SummaryService struct {
	Name string `json:"name"`
	// Change is one of the SummaryChange* values.
	Change       string           `json:"change"`
	Strategy     string           `json:"strategy,omitempty"`
	FromImage    string           `json:"fromImage,omitempty"`
	ToImage      string           `json:"toImage,omitempty"`
	FromRevision string           `json:"fromRevision,omitempty"`
	ToRevision   string           `json:"toRevision,omitempty"`
	Replicas     *SummaryReplicas `json:"replicas,omitempty"`
	// Health is one of the SummaryHealth* values.
	Health string   `json:"health"`
	Nodes  []string `json:"nodes,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// SummaryReplicas reports a service's desired and running replica counts
// after the operation.
type SummaryReplicas struct {
	Desired int `json:"desired"`
	Running int `json:"running"`
}

// SummaryURL is one URL served by a service in scope.
type SummaryURL struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	// Kind is public or internal; consumers treat other values as opaque.
	Kind string `json:"kind"`
	// Status is ready, pending, failed or not_checked.
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// SummaryUndo is the command that reverses the operation, pinned to an
// explicit target, or the reason no safe undo exists.
type SummaryUndo struct {
	// Command is Argv joined with POSIX shell quoting, ready to paste.
	Command string   `json:"command,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	// TargetDeploymentID is the full ID of the history record the undo
	// restores, when it targets one.
	TargetDeploymentID string   `json:"targetDeploymentId,omitempty"`
	Unavailable        string   `json:"unavailable,omitempty"`
	Notes              []string `json:"notes,omitempty"`
}

// SummaryNextStep is a suggested follow-up command.
type SummaryNextStep struct {
	Reason  string `json:"reason"`
	Command string `json:"command"`
}

// Summary operations.
const (
	SummaryOperationDeploy   = "deploy"
	SummaryOperationRollback = "rollback"
	SummaryOperationPromote  = "promote"
	SummaryOperationScale    = "scale"
)

// Summary outcomes.
const (
	SummaryOutcomeSucceeded = "succeeded"
	SummaryOutcomeWarmed    = "warmed"
	SummaryOutcomeUpToDate  = "up_to_date"
	SummaryOutcomePartial   = "partial"
	SummaryOutcomeFailed    = "failed"
)

// Normalized per-service change values.
const (
	SummaryChangeAdded        = "added"
	SummaryChangeUpdated      = "updated"
	SummaryChangeUnchanged    = "unchanged"
	SummaryChangeWarmed       = "warmed"
	SummaryChangeRestored     = "restored"
	SummaryChangeFailed       = "failed"
	SummaryChangeNotAttempted = "not_attempted"
	SummaryChangeRan          = "ran"
	SummaryChangeRemoved      = "removed"
	SummaryChangeRolledBack   = "rolled_back"
	SummaryChangeSkipped      = "skipped"
	SummaryChangePromoted     = "promoted"
	SummaryChangeScaled       = "scaled"
)

// Post-operation health values.
const (
	SummaryHealthHealthy   = "healthy"
	SummaryHealthUnhealthy = "unhealthy"
	SummaryHealthStarting  = "starting"
	SummaryHealthDegraded  = "degraded"
	SummaryHealthStopped   = "stopped"
	SummaryHealthUnknown   = "unknown"
	SummaryHealthNA        = "n/a"
)

// URL kinds and readiness statuses.
const (
	SummaryURLKindPublic   = "public"
	SummaryURLKindInternal = "internal"

	SummaryURLReady      = "ready"
	SummaryURLPending    = "pending"
	SummaryURLFailed     = "failed"
	SummaryURLNotChecked = "not_checked"
)

// SummaryEnvironmentKindStandard is the kind of an environment declared
// under `environments:`.
const SummaryEnvironmentKindStandard = "standard"

// Undo unavailability reasons.
const (
	UndoUnavailableNothingChanged = "nothing changed"
	UndoUnavailableNotMutated     = "nothing was changed"
	UndoUnavailableWarmed         = "warmed revision is not serving; nothing to undo"
	UndoUnavailableFirstDeploy    = "first deployment; nothing to roll back to"
	UndoUnavailableHistory        = "deployment history unreadable"
	UndoUnavailableRestored       = "every service is back at its previous state; nothing to undo"
)

// summaryNextStepLimit caps NextSteps.
const summaryNextStepLimit = 5

// SummaryHealthTimeout bounds the post-operation health read.
const SummaryHealthTimeout = 10 * time.Second

// summaryChangeKept lists the changes that leave a service in a state that
// differs from where the operation started.
var summaryChangeKept = map[string]bool{
	SummaryChangeAdded:      true,
	SummaryChangeUpdated:    true,
	SummaryChangeRan:        true,
	SummaryChangeRemoved:    true,
	SummaryChangeRolledBack: true,
	SummaryChangePromoted:   true,
	SummaryChangeScaled:     true,
}

// summaryBuilder assembles an OperationSummary. It sorts rows and URLs,
// derives the outcome, caps next steps and redacts every free-text field.
type summaryBuilder struct {
	redact     func(string) string
	summary    OperationSummary
	unrestored map[string]bool
	failed     bool
	extraSteps []SummaryNextStep
}

func (e *Engine) newSummaryBuilder(operation string, project string, environment string, startedAt time.Time) *summaryBuilder {
	var redact func(string) string
	if e != nil {
		redact = e.RedactSensitive
	}
	return newSummaryBuilder(redact, operation, project, environment, startedAt)
}

func newSummaryBuilder(redact func(string) string, operation string, project string, environment string, startedAt time.Time) *summaryBuilder {
	if redact == nil {
		redact = func(value string) string { return value }
	}
	return &summaryBuilder{
		redact: redact,
		summary: OperationSummary{
			Operation:   operation,
			Project:     project,
			Environment: SummaryEnvironment{Name: environment, Kind: SummaryEnvironmentKindStandard},
			StartedAt:   startedAt,
			Services:    []SummaryService{},
		},
		unrestored: map[string]bool{},
	}
}

func (b *summaryBuilder) setRecord(deploymentID string) {
	b.summary.DeploymentID = deploymentID
	b.summary.DisplayID = ""
	if deploymentID != "" {
		b.summary.DisplayID = remotestate.FormatDeploymentID(deploymentID)
	}
}

func (b *summaryBuilder) setDuration(seconds float64) {
	b.summary.DurationSeconds = seconds
}

func (b *summaryBuilder) setRevision(revision string, commit string) {
	b.summary.Revision = revision
	b.summary.Commit = commit
}

func (b *summaryBuilder) addService(row SummaryService) {
	b.summary.Services = append(b.summary.Services, row)
}

// addFailedService adds a failed row; unrestored marks a service that may
// differ from its pre-operation state, which makes the outcome partial.
func (b *summaryBuilder) addFailedService(row SummaryService, unrestored bool) {
	row.Change = SummaryChangeFailed
	b.summary.Services = append(b.summary.Services, row)
	if unrestored {
		b.unrestored[row.Name] = true
	}
}

func (b *summaryBuilder) addURL(url SummaryURL) {
	b.summary.URLs = append(b.summary.URLs, url)
}

func (b *summaryBuilder) addNote(note string) {
	if strings.TrimSpace(note) != "" {
		b.summary.Notes = append(b.summary.Notes, note)
	}
}

// addNextStep adds an explicit next step after the derived ones.
func (b *summaryBuilder) addNextStep(reason string, command string) {
	b.extraSteps = append(b.extraSteps, SummaryNextStep{Reason: reason, Command: command})
}

// setFailed marks the operation failed with its error text.
func (b *summaryBuilder) setFailed(errText string) {
	b.failed = true
	b.summary.Error = errText
}

func (b *summaryBuilder) setUndo(undo *SummaryUndo) {
	if undo != nil && undo.Unavailable == UndoUnavailableRestored && !b.changedSomething() {
		undo.Unavailable = UndoUnavailableNotMutated
	}
	b.summary.Undo = undo
}

func (b *summaryBuilder) environment() SummaryEnvironment {
	return b.summary.Environment
}

// outcome derives the outcome from the rows and the failure flag (D9).
func (b *summaryBuilder) outcome() string {
	kept, warmed, unrestored := false, false, false
	for _, row := range b.summary.Services {
		switch {
		case summaryChangeKept[row.Change]:
			kept = true
		case row.Change == SummaryChangeWarmed:
			warmed = true
		case row.Change == SummaryChangeFailed && b.unrestored[row.Name]:
			unrestored = true
		}
	}
	if b.failed {
		if kept || unrestored {
			return SummaryOutcomePartial
		}
		return SummaryOutcomeFailed
	}
	switch {
	case kept:
		return SummaryOutcomeSucceeded
	case warmed:
		return SummaryOutcomeWarmed
	default:
		return SummaryOutcomeUpToDate
	}
}

// changedSomething reports whether a row records an attempted mutation, the
// condition for the post-operation health read.
func (b *summaryBuilder) changedSomething() bool {
	for _, row := range b.summary.Services {
		switch row.Change {
		case SummaryChangeUnchanged, SummaryChangeNotAttempted, SummaryChangeSkipped:
		default:
			return true
		}
	}
	return false
}

// applyHealth fills replicas, health and nodes of every row whose health is
// still open from a post-operation read. A read error marks those rows
// unknown with a note.
func (b *summaryBuilder) applyHealth(health map[string]serviceHealth, err error) {
	if err != nil {
		b.addNote(fmt.Sprintf("health not read: %v", err))
		return
	}
	for index := range b.summary.Services {
		row := &b.summary.Services[index]
		if row.Health != "" {
			continue
		}
		read, ok := health[row.Name]
		if !ok {
			continue
		}
		row.Replicas = &SummaryReplicas{Desired: read.Desired, Running: read.Running}
		row.Health = read.Health
		row.Nodes = append([]string(nil), read.Nodes...)
	}
}

// build finalizes the summary: sorted rows and URLs, the outcome, derived
// next steps, and redacted free text.
func (b *summaryBuilder) build() *OperationSummary {
	summary := b.summary
	summary.Outcome = b.outcome()
	summary.Services = append([]SummaryService{}, summary.Services...)
	sort.SliceStable(summary.Services, func(i, j int) bool { return summary.Services[i].Name < summary.Services[j].Name })
	for index := range summary.Services {
		row := &summary.Services[index]
		if row.Health == "" {
			row.Health = SummaryHealthUnknown
		}
		row.Reason = b.redact(row.Reason)
		row.Error = b.redact(row.Error)
	}
	if len(summary.URLs) > 0 {
		summary.URLs = append([]SummaryURL(nil), summary.URLs...)
		sortSummaryURLs(summary.URLs)
		for index := range summary.URLs {
			summary.URLs[index].Detail = b.redact(summary.URLs[index].Detail)
		}
	}
	summary.NextSteps = summaryNextSteps(summary, b.extraSteps)
	for index := range summary.NextSteps {
		summary.NextSteps[index].Reason = b.redact(summary.NextSteps[index].Reason)
		summary.NextSteps[index].Command = b.redact(summary.NextSteps[index].Command)
	}
	if len(summary.Notes) > 0 {
		notes := make([]string, 0, len(summary.Notes))
		for _, note := range summary.Notes {
			notes = append(notes, b.redact(note))
		}
		summary.Notes = notes
	}
	summary.Error = b.redact(summary.Error)
	summary.Commit = b.redact(summary.Commit)
	if summary.Undo != nil {
		undo := *summary.Undo
		undo.Argv = append([]string(nil), undo.Argv...)
		undo.Unavailable = b.redact(undo.Unavailable)
		if len(undo.Notes) > 0 {
			notes := make([]string, 0, len(undo.Notes))
			for _, note := range undo.Notes {
				notes = append(notes, b.redact(note))
			}
			undo.Notes = notes
		}
		summary.Undo = &undo
	}
	return &summary
}

func sortSummaryURLs(urls []SummaryURL) {
	sort.SliceStable(urls, func(i, j int) bool {
		if urls[i].Service != urls[j].Service {
			return urls[i].Service < urls[j].Service
		}
		if urls[i].Kind != urls[j].Kind {
			return urls[i].Kind > urls[j].Kind // public before internal
		}
		return urls[i].URL < urls[j].URL
	})
}

// summaryNextSteps derives the next steps (D7) followed by explicit extras,
// without duplicates and capped at summaryNextStepLimit.
func summaryNextSteps(summary OperationSummary, extra []SummaryNextStep) []SummaryNextStep {
	env := summary.Environment
	var steps []SummaryNextStep
	seen := map[string]bool{}
	add := func(reason string, argv []string) {
		command := ShellJoin(argv)
		if seen[command] {
			return
		}
		seen[command] = true
		steps = append(steps, SummaryNextStep{Reason: reason, Command: command})
	}
	for _, row := range summary.Services {
		if row.Change == SummaryChangeWarmed {
			add(fmt.Sprintf("%s is warmed for manual promotion", row.Name), append([]string{"tako", "promote", row.Name}, summaryEnvironmentArgs(env)...))
		}
	}
	for _, row := range summary.Services {
		switch row.Change {
		case SummaryChangeFailed:
			add(fmt.Sprintf("%s failed", row.Name), summaryLogsArgv(env, row.Name))
		case SummaryChangeRestored:
			add(fmt.Sprintf("%s was restored after a failed rollout", row.Name), summaryLogsArgv(env, row.Name))
		}
	}
	for _, url := range summary.URLs {
		if url.Status != SummaryURLPending && url.Status != SummaryURLFailed {
			continue
		}
		domain := summaryURLHost(url.URL)
		add(fmt.Sprintf("%s is not ready (%s)", domain, url.Status), append(append([]string{"tako", "domains", "status"}, summaryEnvironmentArgs(env)...), domain))
	}
	for _, row := range summary.Services {
		if row.Health == SummaryHealthUnhealthy || row.Health == SummaryHealthDegraded {
			add(fmt.Sprintf("%s is %s", row.Name, row.Health), append([]string{"tako", "ps"}, summaryEnvironmentArgs(env)...))
		}
	}
	if summary.Outcome == SummaryOutcomePartial {
		add("the operation failed part-way", append([]string{"tako", "history"}, summaryEnvironmentArgs(env)...))
	}
	for _, step := range extra {
		if seen[step.Command] {
			continue
		}
		seen[step.Command] = true
		steps = append(steps, step)
	}
	if len(steps) > summaryNextStepLimit {
		steps = steps[:summaryNextStepLimit]
	}
	return steps
}

func summaryLogsArgv(env SummaryEnvironment, service string) []string {
	argv := append([]string{"tako", "logs"}, summaryEnvironmentArgs(env)...)
	return append(argv, "--service", service, "--since", "10m")
}

func summaryURLHost(url string) string {
	host := url
	if index := strings.Index(host, "://"); index >= 0 {
		host = host[index+3:]
	}
	if index := strings.IndexAny(host, "/?#"); index >= 0 {
		host = host[:index]
	}
	return host
}

// summaryEnvironmentArgs selects the operation's environment explicitly in
// every command the summary derives. It is the single place a preview
// environment would swap `-e <env>` for its own selector.
func summaryEnvironmentArgs(env SummaryEnvironment) []string {
	return []string{"-e", env.Name}
}

// newUndo builds an undo from argv with its shell-quoted command.
func newUndo(argv []string, targetDeploymentID string) *SummaryUndo {
	return &SummaryUndo{Argv: argv, Command: ShellJoin(argv), TargetDeploymentID: targetDeploymentID}
}

func unavailableUndo(reason string) *SummaryUndo {
	return &SummaryUndo{Unavailable: reason}
}

// undoForDeploy derives a deploy's undo (D6): an explicit project rollback to
// the newest stable record before the deploy.
func undoForDeploy(env SummaryEnvironment, outcome string, previous *remotestate.DeploymentState, historyErr error, persistentChanged []string) *SummaryUndo {
	switch outcome {
	case SummaryOutcomeUpToDate:
		return unavailableUndo(UndoUnavailableNothingChanged)
	case SummaryOutcomeWarmed:
		return unavailableUndo(UndoUnavailableWarmed)
	case SummaryOutcomeFailed:
		return unavailableUndo(UndoUnavailableRestored)
	}
	if historyErr != nil {
		return unavailableUndo(UndoUnavailableHistory)
	}
	if previous == nil || previous.ID == "" {
		return unavailableUndo(UndoUnavailableFirstDeploy)
	}
	argv := append(append([]string{"tako", "rollback"}, summaryEnvironmentArgs(env)...), remotestate.FormatDeploymentID(previous.ID))
	undo := newUndo(argv, previous.ID)
	if len(persistentChanged) > 0 {
		names := append([]string(nil), persistentChanged...)
		sort.Strings(names)
		undo.Notes = append(undo.Notes, fmt.Sprintf("persistent services %s are skipped by project rollback; add --include-persistent to include them", strings.Join(names, ", ")))
	}
	return undo
}

// undoForRollback derives a rollback's undo: a roll-forward to the stable
// record that served before the rollback. service is empty for a project
// rollback.
func undoForRollback(env SummaryEnvironment, outcome string, service string, rollForward *remotestate.DeploymentState, target *remotestate.DeploymentState) *SummaryUndo {
	switch outcome {
	case SummaryOutcomeUpToDate:
		return unavailableUndo(UndoUnavailableNothingChanged)
	case SummaryOutcomeFailed:
		return unavailableUndo(UndoUnavailableRestored)
	}
	if rollForward == nil || rollForward.ID == "" || (target != nil && rollForward.ID == target.ID) {
		return unavailableUndo("no newer stable deployment to roll forward to")
	}
	argv := append([]string{"tako", "rollback"}, summaryEnvironmentArgs(env)...)
	if service != "" {
		argv = append(argv, "--service", service)
	}
	argv = append(argv, remotestate.FormatDeploymentID(rollForward.ID))
	return newUndo(argv, rollForward.ID)
}

// undoForPromote derives a promotion's undo: re-promote the previous
// revision while it is retained, else a rollback pinned to the newest stable
// record before the promotion that contains the service.
func undoForPromote(env SummaryEnvironment, service string, fromRevision string, retained bool, fallback *remotestate.DeploymentState, historyErr error) *SummaryUndo {
	if fromRevision != "" && retained {
		argv := append(append([]string{"tako", "promote", service}, summaryEnvironmentArgs(env)...), "--revision", fromRevision)
		return newUndo(argv, "")
	}
	if historyErr != nil {
		return unavailableUndo(UndoUnavailableHistory)
	}
	if fallback == nil || fallback.ID == "" {
		return unavailableUndo(fmt.Sprintf("previous revision of %s is no longer retained and no earlier stable deployment of it is recorded", service))
	}
	argv := append(append([]string{"tako", "rollback"}, summaryEnvironmentArgs(env)...), "--service", service, remotestate.FormatDeploymentID(fallback.ID))
	return newUndo(argv, fallback.ID)
}

// undoForScale restores the previous replica counts of the services whose
// count the scale changed or attempted to change.
func undoForScale(env SummaryEnvironment, previous map[string]int, changed []string) *SummaryUndo {
	if len(changed) == 0 {
		return unavailableUndo(UndoUnavailableNothingChanged)
	}
	names := append([]string(nil), changed...)
	sort.Strings(names)
	argv := append([]string{"tako", "scale"}, summaryEnvironmentArgs(env)...)
	for _, name := range names {
		argv = append(argv, fmt.Sprintf("%s=%d", name, previous[name]))
	}
	return newUndo(argv, "")
}

// newestStableRecord returns the newest stable (success or rolled_back)
// history record that is not excluded, optionally limited to records
// containing service and strictly older than before.
func newestStableRecord(history *remotestate.DeploymentHistory, service string, before time.Time, exclude string) *remotestate.DeploymentState {
	if history == nil {
		return nil
	}
	var newest *remotestate.DeploymentState
	for _, record := range history.Deployments {
		if record == nil || record.ID == "" || record.ID == exclude || !isRollbackStableStatus(record.Status) {
			continue
		}
		if service != "" {
			if _, ok := record.Services[service]; !ok {
				continue
			}
		}
		if !before.IsZero() && !record.Timestamp.Before(before) {
			continue
		}
		if newest == nil || record.Timestamp.After(newest.Timestamp) {
			newest = record
		}
	}
	return newest
}

// FailedBeforeMutationSummary is the summary of an operation that stopped
// before changing anything (for example a failing pre-operation hook):
// every planned service is not_attempted and no undo exists.
func (e *Engine) FailedBeforeMutationSummary(operation string, project string, environment string, services []string, startedAt time.Time, errText string) *OperationSummary {
	builder := e.newSummaryBuilder(operation, project, environment, startedAt)
	names := append([]string(nil), services...)
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		builder.addService(SummaryService{Name: name, Change: SummaryChangeNotAttempted, Health: SummaryHealthNA})
	}
	builder.setFailed(errText)
	builder.setUndo(unavailableUndo(UndoUnavailableNotMutated))
	return builder.build()
}

// UndoUnavailableUnknownState explains a missing undo when an operation failed
// without reporting which services it changed.
const UndoUnavailableUnknownState = "the operation failed without reporting what it changed; check deployment history"

// FailedWithoutResultSummary is the summary of an operation that failed
// after it may have started mutating but returned no result (the cmd layer
// synthesizes the result document). Every service in scope is a failed,
// possibly unrestored row, so the outcome is partial and the next steps
// point at history and logs instead of guessing an undo target.
func (e *Engine) FailedWithoutResultSummary(operation string, project string, environment string, services []string, startedAt time.Time, errText string) *OperationSummary {
	builder := e.newSummaryBuilder(operation, project, environment, startedAt)
	names := append([]string(nil), services...)
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		builder.addFailedService(SummaryService{Name: name, Health: SummaryHealthUnknown}, true)
	}
	builder.setFailed(errText)
	builder.setUndo(unavailableUndo(UndoUnavailableUnknownState))
	return builder.build()
}

var shellSafeArgument =regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// ShellJoin joins argv with POSIX shell quoting: arguments made only of safe
// characters are left bare, others are single-quoted.
func ShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, ShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// ShellQuote quotes one argument for a POSIX shell.
func ShellQuote(arg string) string {
	if arg != "" && shellSafeArgument.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
