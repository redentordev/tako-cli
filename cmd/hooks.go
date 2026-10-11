package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// Canonical command names reported in TAKO_COMMAND. Aliases expand to these
// before dispatch, so hooks never see an alias name.
const (
	hookCommandDeploy   = "deploy"
	hookCommandRollback = "rollback"
	hookCommandPromote  = "promote"
	hookCommandSetup    = "setup"
)

const skipHooksFlagUsage = "Skip local lifecycle hooks (including on-failure) for this invocation"

// hookRunner runs one resolved hook. *hooks.Runner implements it; tests
// substitute a fake.
type hookRunner interface {
	Run(ctx context.Context, invocation hooks.Invocation, env []string) hooks.Outcome
}

// newHookRunner builds the runner for a session. It is a seam for tests.
var newHookRunner = func(sink events.Sink, cfg *config.HooksConfig) hookRunner {
	level := events.LevelInfo
	if cfg.OutputMode() == config.HookOutputQuiet {
		level = events.LevelDebug
	}
	return &hooks.Runner{Sink: sink, Timeout: cfg.TimeoutDuration(), OutputLevel: level}
}

// hookEnviron is the process environment hooks.BuildEnv filters. Seam for
// tests.
var hookEnviron = os.Environ

// hookSession owns the local lifecycle hooks for one command invocation:
// the resolved hooks, the --skip-hooks downgrade, outcome collection, and
// the failure wrapping that maps hook results onto exit codes.
type hookSession struct {
	command    string
	pre, post  hooks.Name
	cfg        *config.Config
	resolution *hooks.Resolution
	// active lists the resolved hooks this command can run (pre, post,
	// on-failure); empty means the session is inert.
	active   map[hooks.Name]hooks.Invocation
	skip     bool
	runner   hookRunner
	stream   events.Sink
	redact   func(string) string
	base     hooks.Context
	outcomes []engine.HookOutcome
	// reached records that the operation got to its pre-hook stage, which
	// makes on-failure eligible to run.
	reached    bool
	failedHook hooks.Name
}

// newHookSession resolves hooks relative to the config file's directory at
// command start, before any lock or lease. Resolution errors for hooks this
// command would run fail with an invalid-request error (exit 2) unless
// skip is set, in which case they are downgraded to a debug event and every
// relevant defined hook reports hook.skipped.
func newHookSession(command string, cfg *config.Config, configPath string, skip bool) (*hookSession, error) {
	pre, post := hooks.Name("pre-"+command), hooks.Name("post-"+command)
	projectDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return nil, &engine.InvalidRequestError{Err: fmt.Errorf("resolve hook directory: %w", err)}
	}
	eng := cliEngine()
	session := &hookSession{
		command:    command,
		pre:        pre,
		post:       post,
		cfg:        cfg,
		resolution: hooks.Resolve(projectDir, cfg.Hooks, runtime.GOOS),
		active:     map[hooks.Name]hooks.Invocation{},
		skip:       skip,
		stream:     eng.EventStream(),
		redact:     eng.RedactSensitive,
	}
	relevant := map[hooks.Name]bool{pre: true, post: true, hooks.OnFailure: true}

	var resolveErrs []error
	for _, resolveErr := range session.resolution.Errors {
		var hookErr hooks.HookError
		if errors.As(resolveErr, &hookErr) && !relevant[hookErr.HookName()] {
			continue
		}
		resolveErrs = append(resolveErrs, resolveErr)
	}
	for _, unknown := range session.resolution.Unknown {
		session.emit(events.TypeWarning, events.LevelWarn, "Warning: "+unknown.Message+"\n", map[string]any{"file": unknown.Name})
	}

	if skip {
		for _, resolveErr := range resolveErrs {
			session.emit(events.TypeLogLine, events.LevelDebug, fmt.Sprintf("Ignoring hook problem because of --skip-hooks: %v\n", resolveErr), nil)
		}
		for _, name := range session.resolution.Defined {
			if !relevant[name] {
				continue
			}
			session.emit(events.TypeHookSkipped, events.LevelInfo, fmt.Sprintf("→ Skipping %s hook (--skip-hooks)\n", name), map[string]any{"hook": string(name), "reason": "--skip-hooks"})
			session.outcomes = append(session.outcomes, engine.HookOutcome{Name: string(name), Status: engine.HookSkipped})
		}
		return session, nil
	}
	if len(resolveErrs) > 0 {
		return nil, &engine.InvalidRequestError{Err: errors.Join(resolveErrs...)}
	}
	for name, invocation := range session.resolution.Hooks {
		if relevant[name] {
			session.active[name] = invocation
		}
	}
	if len(session.active) == 0 {
		return session, nil
	}

	session.runner = newHookRunner(session.stream, cfg.Hooks)
	envName := getEnvironmentName(cfg)
	session.base = hooks.Context{
		Command:     command,
		Project:     cfg.Project.Name,
		Environment: envName,
		Performer:   remotestate.GetCurrentUser(),
		Version:     Version,
	}
	if servers, err := cfg.GetEnvironmentServers(envName); err == nil {
		session.base.Servers = servers
	}
	if instance := cfg.PreviewInstance(); instance != nil && instance.Identity == envName {
		session.base.Preview = &hooks.PreviewContext{Name: instance.Name, Base: instance.Base, ExpiresAt: previewExpiryEstimate(instance.Policy.EffectiveTTL())}
	}
	session.registerSecrets(envName)
	return session, nil
}

// previewExpiryEstimate is the expiry a successful deploy now would record
// for ttl, RFC 3339 UTC, or empty for ttl none or an invalid ttl.
func previewExpiryEstimate(ttl string) string {
	duration, never, err := config.ParsePreviewTTL(ttl)
	if err != nil || never {
		return ""
	}
	return time.Now().Add(duration).UTC().Format(time.RFC3339)
}

// setPreviewExpiry replaces the TAKO_PREVIEW_EXPIRES_AT value of every
// later hook.
func (s *hookSession) setPreviewExpiry(expiresAt string) {
	if s == nil || s.base.Preview == nil {
		return
	}
	s.base.Preview.ExpiresAt = expiresAt
}

// registerSecrets makes sure hook output and TAKO_ERROR are redacted the
// same way for every command, including setup, which registers nothing
// itself: config secrets plus passEnv values with credential-like names.
func (s *hookSession) registerSecrets(envName string) {
	eng := cliEngine()
	eng.RegisterServerPasswords(s.cfg.Servers)
	if services, err := s.cfg.GetServices(envName); err == nil {
		eng.RegisterPublicConfigNames(s.cfg, services)
		eng.RegisterServiceEnvValues(services)
		eng.RegisterBackupSecrets(services)
		for _, failure := range engine.EnvFileUnreadableErrors(eng.RegisterServiceEnvFileValues(services)) {
			s.emit(events.TypeWarning, events.LevelWarn, eng.RedactSensitive(engine.EnvFileUnreadableWarning(failure)), map[string]any{
				"environment": envName, "service": failure.Service, "reason": engine.EnvFileUnreadableReason, "stream": "stderr",
			})
		}
	}
	eng.RegisterRegistrySecrets(s.cfg)
	eng.RegisterACMEDNSSecrets(s.cfg)
	eng.RegisterNotificationSecrets(s.cfg)
	var passEnv []string
	if s.cfg.Hooks != nil {
		passEnv = s.cfg.Hooks.PassEnv
	}
	for _, credential := range hooks.CredentialEnv(hookEnviron(), passEnv, runtime.GOOS) {
		eng.RegisterSecretAs(credential.Value, secrets.HookPassEnvLabel(credential.Name))
	}
}

func (s *hookSession) emit(eventType string, level events.Level, message string, data map[string]any) {
	s.stream.Emit(events.Event{Type: eventType, Phase: events.PhaseHooks, Level: level, Message: message, Data: data})
}

// inert reports whether no hook can run in this session.
func (s *hookSession) inert() bool {
	return s == nil || s.skip || len(s.active) == 0
}

// hookOutcomes returns the per-hook outcomes for the result document, or
// nil when no hook was considered.
func (s *hookSession) hookOutcomes() []engine.HookOutcome {
	if s == nil || len(s.outcomes) == 0 {
		return nil
	}
	return append([]engine.HookOutcome(nil), s.outcomes...)
}

// preHookFailed reports whether the command's pre hook failed, which means
// the operation stopped before any mutation.
func (s *hookSession) preHookFailed() bool {
	return s != nil && s.failedHook != "" && s.failedHook == s.pre
}

// mutationReached reports whether the operation may have reached its
// mutation stage: false only when hooks prove it stopped before it (the pre
// hook failed, or a hook-gated engine never asked to mutate).
func (s *hookSession) mutationReached() bool {
	if s.preHookFailed() {
		return false
	}
	if s != nil && !s.inert() && !s.reached {
		return false
	}
	return true
}

// run executes one hook with the session context merged into ctx values.
func (s *hookSession) run(ctx context.Context, name hooks.Name, values hooks.Context) hooks.Outcome {
	if ctx == nil {
		ctx = context.Background()
	}
	invocation := s.active[name]
	hookCtx := s.base
	mergeHookContext(&hookCtx, values)
	hookCtx.Hook = name
	hookCtx.RecordedAt = time.Now()
	if len(hookCtx.Hosts) == 0 {
		hookCtx.Hosts = s.hostsFor(hookCtx.Servers)
	}
	var passEnv []string
	if s.cfg.Hooks != nil {
		passEnv = s.cfg.Hooks.PassEnv
	}
	env := hooks.BuildEnv(hookEnviron(), passEnv, hookCtx, runtime.GOOS)
	outcome := s.runner.Run(ctx, invocation, env)
	recorded := engine.HookOutcome{Name: string(name), Status: outcome.Status}
	if outcome.ExitCode != nil {
		code := *outcome.ExitCode
		recorded.ExitCode = &code
	}
	if outcome.Status != hooks.StatusSkipped {
		duration := outcome.DurationMs
		recorded.DurationMs = &duration
	}
	s.outcomes = append(s.outcomes, recorded)
	return outcome
}

func (s *hookSession) hostsFor(servers []string) []string {
	var hosts []string
	seen := map[string]bool{}
	for _, name := range servers {
		host := s.cfg.Servers[name].Host
		if host != "" && !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// runPre marks the pre-hook stage as reached and runs the command's pre
// hook. A failing, unstartable, or timed-out hook returns a
// *engine.HookFailedError (exit 1, or 5 when cancelled).
func (s *hookSession) runPre(ctx context.Context, values hooks.Context) error {
	if s == nil {
		return nil
	}
	s.reached = true
	if s.inert() {
		return nil
	}
	if _, ok := s.active[s.pre]; !ok {
		return nil
	}
	outcome := s.run(ctx, s.pre, values)
	if !outcome.Failed() {
		return nil
	}
	s.failedHook = s.pre
	return &engine.HookFailedError{Hook: string(s.pre), ExitCode: outcome.ExitCode, TimedOut: outcome.TimedOut, Err: outcome.Err}
}

// beforeMutation adapts runPre to the engine's BeforeMutation seam for
// rollback and promote, capturing the resolved target for later hooks. It
// returns nil when no hook can run, leaving the engine unchanged.
func (s *hookSession) beforeMutation(captured *engine.MutationInfo) engine.BeforeMutationFunc {
	if s.inert() {
		return nil
	}
	return func(ctx context.Context, info engine.MutationInfo) error {
		*captured = info
		return s.runPre(ctx, mutationHookContext(info))
	}
}

// finish applies the post-operation hook semantics and returns the error
// the command exits with:
//   - success (or an attention-class result): run the post hook; its
//     failure turns success into an AttentionError (exit 6) and leaves an
//     existing attention error unchanged.
//   - failure after the pre-hook stage: run on-failure; its own failure is
//     only a warning and the operation error is returned unchanged.
func (s *hookSession) finish(ctx context.Context, opErr error, success func() hooks.Context, failure func() hooks.Context) error {
	if s.inert() || !s.reached {
		return opErr
	}
	if opErr == nil || engine.Classify(opErr) == engine.ClassAttention {
		if _, ok := s.active[s.post]; !ok {
			return opErr
		}
		values := hooks.Context{}
		if success != nil {
			values = success()
		}
		outcome := s.run(ctx, s.post, values)
		if outcome.Failed() && opErr == nil {
			return &engine.AttentionError{Err: fmt.Errorf("%s succeeded but %w", s.command, outcome.Err)}
		}
		return opErr
	}
	s.runOnFailure(opErr, failure)
	return opErr
}

func (s *hookSession) runOnFailure(opErr error, failure func() hooks.Context) {
	if _, ok := s.active[hooks.OnFailure]; !ok {
		return
	}
	values := hooks.Context{}
	if failure != nil {
		values = failure()
	}
	values.Result = "failed"
	values.ErrorClass = hookErrorClass(engine.Classify(opErr))
	values.Error = s.redact(opErr.Error())
	values.FailedHook = s.failedHook
	// The command context may already be cancelled; on-failure gets its own
	// lifetime bounded by the hook timeout.
	outcome := s.run(context.Background(), hooks.OnFailure, values)
	if outcome.Failed() {
		s.emit(events.TypeWarning, events.LevelWarn, fmt.Sprintf("Warning: on-failure hook failed (the %s error is reported unchanged): %v\n", s.command, outcome.Err), map[string]any{"hook": string(hooks.OnFailure)})
	}
}

// hookErrorClass names an engine error class for TAKO_ERROR_CLASS.
func hookErrorClass(class engine.Class) string {
	switch class {
	case engine.ClassInvalid:
		return "invalid"
	case engine.ClassLocked:
		return "locked"
	case engine.ClassConnectivity:
		return "connectivity"
	case engine.ClassCancelled:
		return "cancelled"
	case engine.ClassAttention:
		return "attention"
	case engine.ClassNone:
		return ""
	default:
		return "failed"
	}
}

// mergeHookContext copies the non-empty fields of values into dst.
func mergeHookContext(dst *hooks.Context, values hooks.Context) {
	if values.Preview != nil {
		preview := *values.Preview
		dst.Preview = &preview
	}
	if len(values.Services) > 0 {
		dst.Services = values.Services
	}
	if len(values.Servers) > 0 {
		dst.Servers = values.Servers
	}
	if len(values.Hosts) > 0 {
		dst.Hosts = values.Hosts
	}
	for _, field := range []struct {
		dst *string
		src string
	}{
		{&dst.Revision, values.Revision},
		{&dst.Image, values.Image},
		{&dst.GitCommit, values.GitCommit},
		{&dst.GitCommitShort, values.GitCommitShort},
		{&dst.GitBranch, values.GitBranch},
		{&dst.PlanHash, values.PlanHash},
		{&dst.DeploymentID, values.DeploymentID},
		{&dst.RollbackTarget, values.RollbackTarget},
		{&dst.Result, values.Result},
		{&dst.ErrorClass, values.ErrorClass},
		{&dst.Error, values.Error},
	} {
		if field.src != "" {
			*field.dst = field.src
		}
	}
	if values.GitDirty != nil {
		dst.GitDirty = values.GitDirty
	}
	if values.PlanEmpty != nil {
		dst.PlanEmpty = values.PlanEmpty
	}
	if values.PlanDestructive != nil {
		dst.PlanDestructive = values.PlanDestructive
	}
	if values.DurationSeconds != nil {
		dst.DurationSeconds = values.DurationSeconds
	}
	if values.FailedHook != "" {
		dst.FailedHook = values.FailedHook
	}
}

// deployHookContext renders the approved plan for deploy hooks.
func deployHookContext(plan engine.DeployPlan) hooks.Context {
	empty, destructive := plan.Empty, plan.Destructive
	values := hooks.Context{
		Services:        plan.Services,
		Servers:         plan.Servers,
		Revision:        plan.Revision,
		PlanHash:        plan.Hash(),
		PlanEmpty:       &empty,
		PlanDestructive: &destructive,
	}
	if plan.Git != nil {
		dirty := plan.Git.Dirty
		values.GitCommit = plan.Git.Commit
		values.GitCommitShort = plan.Git.CommitShort
		values.GitBranch = plan.Git.Branch
		values.GitDirty = &dirty
	}
	return values
}

// mutationHookContext renders a rollback or promote target for hooks.
func mutationHookContext(info engine.MutationInfo) hooks.Context {
	values := hooks.Context{
		Services: info.Services,
		Servers:  info.Servers,
		Revision: info.Revision,
		Image:    info.Image,
	}
	if info.Operation == hookCommandRollback {
		values.RollbackTarget = info.TargetDeploymentID
	}
	return values
}

func durationPointer(seconds float64) *float64 {
	return &seconds
}
