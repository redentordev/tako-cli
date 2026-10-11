package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

// deploySummaryInput carries everything a deploy summary is built from. It
// is filled by DeploySession.Apply on every return path.
type deploySummaryInput struct {
	Project     string
	Environment string
	StartedAt   time.Time
	Duration    float64
	// DeploymentID is the history record of this deploy, when written.
	DeploymentID string
	Revision     string
	Commit       string

	// Services is the deploy scope: every service, or the targeted one.
	Services         map[string]config.ServiceConfig
	RunPrerequisites []string
	Plan             *reconcile.ReconciliationPlan
	// ServicesToDeploy is nil on the up-to-date fast path.
	ServicesToDeploy map[string]config.ServiceConfig
	Outcomes         []ServiceOutcome
	ActualBefore     map[string]*reconcile.ActualService
	ActualAfter      map[string]*reconcile.ActualService

	RemovalsApplied bool
	RemovalsFailed  bool

	Failed bool
	// Error is the (redacted) failure text.
	Error string
	// BookkeepingError is set when the rollout succeeded but persisting
	// state afterwards failed.
	BookkeepingError string
	// AttentionError is set when the deploy succeeded but needs attention
	// (for example strict domain checks).
	AttentionError string
	// Warnings are non-fatal post-rollout problems, shown as notes.
	Warnings []string

	PreviousStable *remotestate.DeploymentState
	HistoryErr     error

	DomainChecked  bool
	StrictDomains  bool
	DomainStatuses []health.DomainStatus

	Health     map[string]serviceHealth
	HealthErr  error
	HealthRead bool

	// Preview is set for a `deploy --preview` run.
	Preview *SummaryPreview
}

// deployNeedsHealth reports whether the deploy attempted any mutation, which
// is when the post-operation health read runs.
func deployNeedsHealth(outcomes []ServiceOutcome, removals bool) bool {
	if removals {
		return true
	}
	for _, outcome := range outcomes {
		if outcome.Action != OutcomeUpToDate {
			return true
		}
	}
	return false
}

// buildDeploySummary builds the deploy OperationSummary (D2-D9).
func buildDeploySummary(redact func(string) string, in deploySummaryInput) *OperationSummary {
	builder := newSummaryBuilder(redact, SummaryOperationDeploy, in.Project, in.Environment, in.StartedAt)
	builder.setRecord(in.DeploymentID)
	builder.setDuration(in.Duration)
	builder.setRevision(in.Revision, in.Commit)
	builder.setPreview(in.Preview)

	planChanges := map[string]reconcile.ChangeType{}
	var removals []string
	if in.Plan != nil {
		for _, change := range in.Plan.Changes {
			planChanges[change.ServiceName] = change.Type
			if change.Type == reconcile.ChangeRemove {
				removals = append(removals, change.ServiceName)
			}
		}
	}
	sort.Strings(removals)
	outcomes := map[string]ServiceOutcome{}
	for _, outcome := range in.Outcomes {
		outcomes[outcome.Name] = outcome
	}

	names := sortedServiceNames(in.Services)
	inScope := map[string]bool{}
	for _, name := range names {
		inScope[name] = true
	}
	var persistentChanged []string
	addRow := func(name string, service config.ServiceConfig, known bool) {
		row := SummaryService{Name: name}
		if known {
			row.Strategy, _ = serviceStrategyReport(&service)
			if service.IsRun() || service.IsJob() {
				row.Health = SummaryHealthNA
			}
		}
		if before := in.ActualBefore[name]; before != nil {
			row.FromImage = before.Image
			row.FromRevision = before.CurrentRevision
		}
		unrestored := false
		failed := false
		if outcome, ok := outcomes[name]; ok {
			switch outcome.Action {
			case OutcomeDeployed:
				row.Change = SummaryChangeUpdated
				if planChanges[name] == reconcile.ChangeAdd {
					row.Change = SummaryChangeAdded
				}
				row.ToImage = outcome.Image
			case OutcomeWarmed:
				row.Change = SummaryChangeWarmed
				row.ToImage = outcome.Image
			case OutcomeRan:
				row.Change = SummaryChangeRan
				row.ToImage = outcome.Image
				row.Health = SummaryHealthNA
				if outcome.Run != nil {
					row.Reason = fmt.Sprintf("exit %d in %s", outcome.Run.ExitCode, summaryDuration(time.Duration(outcome.Run.DurationMs)*time.Millisecond))
				}
			case OutcomeRestored:
				row.Change = SummaryChangeRestored
				row.ToImage = row.FromImage
				row.Error = outcome.Error
				if outcome.Image != "" {
					row.Reason = fmt.Sprintf("rollout of %s failed; the previous revision serves again", outcome.Image)
				}
			case OutcomeFailed:
				failed = true
				row.ToImage = outcome.Image
				row.Error = outcome.Error
				unrestored = outcome.Recovery != nil && outcome.Recovery.Status != deployer.RecoveryRestored
				if outcome.Run != nil {
					row.Health = SummaryHealthNA
				}
			case OutcomeRemoved:
				row.Change = SummaryChangeRemoved
				row.Health = SummaryHealthNA
			default:
				row.Change = SummaryChangeUnchanged
				row.ToImage = row.FromImage
			}
		} else if _, planned := in.ServicesToDeploy[name]; planned && in.ServicesToDeploy != nil {
			row.Change = SummaryChangeNotAttempted
			row.Health = SummaryHealthNA
		} else {
			row.Change = SummaryChangeUnchanged
			row.ToImage = row.FromImage
			if row.ToImage == "" && known && !service.IsRun() && !service.IsJob() {
				row.ToImage = service.Image
			}
		}
		if after := in.ActualAfter[name]; after != nil && row.Change != SummaryChangeNotAttempted && row.Health != SummaryHealthNA {
			row.ToRevision = after.CurrentRevision
		}
		if row.Change == SummaryChangeUnchanged && row.ToRevision == "" {
			row.ToRevision = row.FromRevision
		}
		if known && service.Persistent && (row.Change == SummaryChangeAdded || row.Change == SummaryChangeUpdated) {
			persistentChanged = append(persistentChanged, name)
		}
		if failed {
			builder.addFailedService(row, unrestored)
			return
		}
		builder.addService(row)
	}
	for _, name := range names {
		addRow(name, in.Services[name], true)
	}
	for _, name := range in.RunPrerequisites {
		if inScope[name] {
			continue
		}
		inScope[name] = true
		builder.addService(SummaryService{Name: name, Change: SummaryChangeUnchanged, Health: SummaryHealthNA, Reason: "deploy-time run prerequisite"})
	}
	for _, name := range removals {
		if inScope[name] {
			continue
		}
		inScope[name] = true
		row := SummaryService{Name: name, Health: SummaryHealthNA}
		if before := in.ActualBefore[name]; before != nil {
			row.FromImage = before.Image
			row.FromRevision = before.CurrentRevision
		}
		switch {
		case in.RemovalsApplied:
			row.Change = SummaryChangeRemoved
		case in.RemovalsFailed:
			builder.addFailedService(row, true)
			continue
		default:
			row.Change = SummaryChangeNotAttempted
		}
		builder.addService(row)
	}

	hintEnvironment := in.Environment
	if builder.environment().isPreview() {
		// `domains hosts` takes no --preview selector.
		hintEnvironment = ""
	}
	urls, urlNotes := deploySummaryURLs(hintEnvironment, in.Services, in.DomainChecked, in.StrictDomains, in.DomainStatuses)
	for _, url := range urls {
		builder.addURL(url)
	}

	if in.Failed {
		builder.setFailed(in.Error)
	}
	if in.BookkeepingError != "" {
		builder.addNote("rollout succeeded; state bookkeeping failed: " + in.BookkeepingError)
	}
	if in.AttentionError != "" {
		builder.addNote(in.AttentionError)
	}
	for _, warning := range in.Warnings {
		builder.addNote(warning)
	}
	for _, note := range urlNotes {
		builder.addNote(note)
	}
	if in.HealthRead {
		builder.applyHealth(in.Health, in.HealthErr)
	}

	outcome := builder.outcome()
	undo := undoForDeploy(builder.environment(), outcome, in.PreviousStable, in.HistoryErr, persistentChanged)
	builder.setUndo(undo)
	if undo.Unavailable == UndoUnavailableHistory {
		builder.addNextStep("deployment history could not be read", ShellJoin(append([]string{"tako", "history"}, summaryEnvironmentArgs(builder.environment())...)))
	}
	return builder.build()
}

// deploySummaryURLs lists the public and internal URLs of the services in
// scope with their readiness, plus notes for dynamic domains and the
// internal-routes hosts hint.
func deploySummaryURLs(envName string, services map[string]config.ServiceConfig, domainChecked bool, strict bool, statuses []health.DomainStatus) ([]SummaryURL, []string) {
	byDomain := map[string]health.DomainStatus{}
	for _, status := range statuses {
		byDomain[status.Domain] = status
	}
	var urls []SummaryURL
	var notes []string
	hasInternal := false
	for _, name := range sortedServiceNames(services) {
		service := services[name]
		if service.Proxy == nil {
			continue
		}
		if service.IsPublic() {
			for _, domain := range service.Proxy.GetAllDomains() {
				url := SummaryURL{Service: name, URL: "https://" + domain, Kind: SummaryURLKindPublic, Status: SummaryURLNotChecked}
				if domainChecked {
					if status, ok := byDomain[domain]; ok {
						url.Status, url.Detail = summaryURLStatus(status, strict)
					}
				}
				urls = append(urls, url)
			}
			if service.Proxy.DynamicDomains != nil && service.Proxy.DynamicDomains.IsEnabled() {
				notes = append(notes, fmt.Sprintf("dynamic domains enabled for %s", name))
			}
		}
		if service.Proxy.IsInternal() && service.Proxy.GetPrimaryHost() != "" {
			for _, host := range service.Proxy.GetAllHosts() {
				urls = append(urls, SummaryURL{Service: name, URL: "http://" + host, Kind: SummaryURLKindInternal, Status: SummaryURLNotChecked})
				hasInternal = true
			}
		}
	}
	sortSummaryURLs(urls)
	if hasInternal && envName != "" {
		notes = append(notes, fmt.Sprintf("Run `tako domains hosts -e %s` to print /etc/hosts entries for internal routes.", envName))
	}
	return urls, notes
}

// summaryURLStatus maps a domain DNS/TLS check to a URL status and detail.
func summaryURLStatus(status health.DomainStatus, strict bool) (string, string) {
	detail := status.Message
	if detail == "" && status.Warning != "" {
		detail = status.Warning
	}
	switch {
	case status.Ready() && status.OriginRestricted:
		// A proxy.cdn origin that only the CDN can reach: the public probe
		// failed by design, but the certificate is valid on the node(s).
		return SummaryURLReady, "origin-restricted: certificate valid on the proxy node(s)"
	case status.Ready():
		return SummaryURLReady, detail
	case strict, status.State == health.DomainStateWrongDNS:
		if detail == "" {
			detail = string(status.State)
		}
		return SummaryURLFailed, detail
	default:
		if detail == "" {
			detail = string(status.State)
		}
		return SummaryURLPending, detail
	}
}

// deployResultURLs returns the sorted public and internal URL strings of
// DeployResult.urls and internalUrls.
func deployResultURLs(services map[string]config.ServiceConfig) ([]string, []string) {
	urls, _ := deploySummaryURLs("", services, false, false, nil)
	var public, internal []string
	for _, url := range urls {
		if url.Kind == SummaryURLKindInternal {
			internal = append(internal, url.URL)
			continue
		}
		public = append(public, url.URL)
	}
	return public, internal
}

func summaryDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Second).String()
}

// deployApplyState tracks what Apply did, for the summary.
type deployApplyState struct {
	servicesToDeploy map[string]config.ServiceConfig
	removalsApplied  bool
	removalsFailed   bool
	finalActual      map[string]*reconcile.ActualService
	domainChecked    bool
	domainStatuses   []health.DomainStatus
	bookkeepingErr   string
	// warnings are non-fatal post-rollout problems (alert configuration).
	warnings []string
}

// alertPolicyWarning describes an alert configuration push failure that
// does not fail the deploy.
func alertPolicyWarning(err error) string {
	if isDefaultOnlyAlertPolicyError(err) {
		return "warning: default node-side monitoring was not configured: " + err.Error() + " (retried on the next deploy)"
	}
	return "warning: alert configuration was not applied; the rollout succeeded: " + err.Error() + " (fix the cause, for example low free disk on the node, and re-run tako deploy)"
}

// isDefaultOnlyAlertPolicyError reports an alert push failure of a request
// that carried only default-derived node-side monitoring.
func isDefaultOnlyAlertPolicyError(err error) bool {
	var applyErr *deployer.AlertPolicyApplyError
	return errors.As(err, &applyErr) && applyErr.DefaultOnly
}

// finishSummary redacts the result's error fields in place and attaches the
// summary. opErr is the error Apply returns with result.
func (s *DeploySession) finishSummary(ctx context.Context, result *DeployResult, state *deployApplyState, opErr error) {
	if result == nil {
		return
	}
	e := s.engine
	result.Error = e.RedactSensitive(result.Error)
	for index := range result.Services {
		result.Services[index].Error = e.RedactSensitive(result.Services[index].Error)
	}
	if state == nil {
		state = &deployApplyState{}
	}
	result.Preview = s.previewResult()
	failed := opErr != nil && Classify(opErr) != ClassAttention
	in := deploySummaryInput{
		Project:          s.cfg.Project.Name,
		Environment:      s.envName,
		StartedAt:        result.StartedAt,
		Duration:         result.Duration,
		DeploymentID:     result.DeploymentID,
		Revision:         s.buildTag,
		Commit:           s.gitStrings.ShortHash,
		Services:         s.services,
		RunPrerequisites: s.runPrerequisiteNames,
		Plan:             s.plan,
		ServicesToDeploy: state.servicesToDeploy,
		Outcomes:         result.Services,
		ActualBefore:     s.actualState,
		ActualAfter:      state.finalActual,
		RemovalsApplied:  state.removalsApplied,
		RemovalsFailed:   state.removalsFailed,
		Failed:           failed,
		BookkeepingError: state.bookkeepingErr,
		PreviousStable:   s.previousStable,
		HistoryErr:       s.historyErr,
		DomainChecked:    state.domainChecked,
		StrictDomains:    s.req.StrictDomains,
		DomainStatuses:   state.domainStatuses,
		Preview:          s.summaryPreview(),
	}
	for _, warning := range state.warnings {
		in.Warnings = append(in.Warnings, e.RedactSensitive(warning))
	}
	if failed {
		in.Error = result.Error
		if in.Error == "" {
			in.Error = e.RedactSensitive(opErr.Error())
		}
	} else if opErr != nil {
		in.AttentionError = e.RedactSensitive(opErr.Error())
	}
	if in.Duration == 0 && !in.StartedAt.IsZero() {
		in.Duration = time.Since(in.StartedAt).Seconds()
	}
	if deployNeedsHealth(result.Services, state.removalsApplied || state.removalsFailed) {
		in.HealthRead = true
		in.Health, in.HealthErr = e.summaryHealth(ctx, s.runtime, s.cfg, s.envName, s.serverNames, s.services)
		if in.HealthErr != nil {
			in.HealthErr = fmt.Errorf("%s", e.RedactSensitive(in.HealthErr.Error()))
		}
	}
	result.Summary = buildDeploySummary(e.RedactSensitive, in)
}
