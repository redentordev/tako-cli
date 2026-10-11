// Package engine exposes Tako's deployment workflows as a library. The CLI
// commands in cmd/ are thin adapters over this package: they parse flags into
// request structs, run the engine, and render the emitted event stream. The
// engine never prompts, never prints, and never exits the process; progress
// flows through the events sink and outcomes are returned as typed results
// and errors.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// Engine runs Tako operations against configured servers.
type Engine struct {
	cliVersion    string
	cliCommit     string
	redactor      *secrets.Redactor
	stream        *events.Stream
	stateAutoSync StateAutoSyncFunc
	buildOutput   io.Writer
	buildWriter   *redactingWriter
	// logsHooks substitutes node access for StreamLogs in tests.
	logsHooks *logsHooks
}

type redactingWriter struct {
	mu      sync.Mutex
	writer  io.Writer
	redact  func(string) string
	pending bytes.Buffer
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.pending.Write(p)
	data := w.pending.Bytes()
	lastNewline := bytes.LastIndexByte(data, '\n')
	if carriage := bytes.LastIndexByte(data, '\r'); carriage > lastNewline {
		lastNewline = carriage
	}
	if lastNewline < 0 {
		return len(p), nil
	}
	complete := append([]byte(nil), data[:lastNewline+1]...)
	remainder := append([]byte(nil), data[lastNewline+1:]...)
	w.pending.Reset()
	_, _ = w.pending.Write(remainder)
	if _, err := io.WriteString(w.writer, w.redact(string(complete))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(w.writer, w.redact(w.pending.String()))
	w.pending.Reset()
	return err
}

// Options configures an Engine.
type Options struct {
	// CLIVersion and CLICommit stamp deployment state records.
	CLIVersion string
	CLICommit  string
	// Sink receives the operation event stream. Nil discards events.
	Sink events.Sink
	// StateAutoSync optionally refreshes local deployment state from the
	// remote mesh before planning; errors are non-fatal.
	StateAutoSync StateAutoSyncFunc
	// BuildOutput redirects deployer build/progress output. Nil keeps the
	// deployer's default (stdout); machine-output modes route this to
	// stderr so stdout stays parseable.
	BuildOutput io.Writer
}

// New constructs an Engine. Every emitted event passes through a secrets
// redactor; operations register sensitive values (credential-like service env
// values, secrets, SSH passwords) before emitting anything that could contain
// them, and mark names and image references public so they stay readable.
func New(opts Options) *Engine {
	redactor := secrets.NewRedactor()
	return &Engine{
		cliVersion:    opts.CLIVersion,
		cliCommit:     opts.CLICommit,
		redactor:      redactor,
		stream:        events.NewStream(opts.Sink, redactor.Redact),
		stateAutoSync: opts.StateAutoSync,
		buildOutput:   opts.BuildOutput,
	}
}

// RegisterSecret adds a value to the event redactor without a label, so it
// renders as `[REDACTED]`. In-tree callers use RegisterSecretAs.
func (e *Engine) RegisterSecret(value string) {
	e.RegisterSecretAs(value, "")
}

// RegisterSecretAs adds a value to the event redactor under a source label
// (see the label constructors in pkg/secrets), rendered as
// `[REDACTED:<label>]`. Labels are built from config names, never values.
func (e *Engine) RegisterSecretAs(value, label string) {
	if value != "" {
		e.redactor.RegisterLabeled(value, label)
	}
}

func (e *Engine) registerEntries(entries []secrets.RedactionEntry) {
	for _, entry := range entries {
		if entry.Masked {
			e.RegisterSecretAs(entry.Value, entry.Label)
		}
	}
}

// RegisterServerPasswords registers the SSH password of each server as
// `servers.<name>.password`.
func (e *Engine) RegisterServerPasswords(servers map[string]config.ServerConfig) {
	e.registerEntries(secrets.ClassifyServerPasswords(servers))
}

// RegisterNotificationSecrets registers notification URLs as
// `notifications.<kind>`.
func (e *Engine) RegisterNotificationSecrets(cfg *config.Config) {
	e.registerEntries(secrets.ClassifyNotificationURLs(cfg))
}

// RegisterBuildArgs registers every shared build arg value as
// `builds.<build>.args.<ARG>`.
func (e *Engine) RegisterBuildArgs(cfg *config.Config) {
	e.registerEntries(secrets.ClassifyBuildArgs(cfg))
}

// RegisterBackupSecrets registers each service's backup storage credentials
// as `services.<service>.backup.<field>`.
func (e *Engine) RegisterBackupSecrets(services map[string]config.ServiceConfig) {
	e.registerEntries(secrets.ClassifyBackupCredentials(services))
}

// RegisterServiceEnvFileValues registers the envFile/envFiles values of
// services that look like credentials (the same rule as inline `env:`
// values) as `envFile.<NAME>`. Services whose files cannot be read register
// nothing; the returned error joins one error per such service.
func (e *Engine) RegisterServiceEnvFileValues(services map[string]config.ServiceConfig) error {
	entries, failures := secrets.ClassifyServiceEnvFiles(services)
	e.registerEntries(entries)
	if len(failures) == 0 {
		return nil
	}
	errs := make([]error, 0, len(failures))
	for _, failure := range failures {
		errs = append(errs, &EnvFileUnreadableError{Service: failure.Service, Err: failure.Err})
	}
	return errors.Join(errs...)
}

// exportPasswordLabel labels the `config export` SSH password.
var exportPasswordLabel = secrets.ExportPasswordLabel()

func singleService(name string, service config.ServiceConfig) map[string]config.ServiceConfig {
	return map[string]config.ServiceConfig{name: service}
}

// EnvFileUnreadableError reports a service envFile that could not be read
// while registering redaction values.
type EnvFileUnreadableError struct {
	Service string
	Err     error
}

func (e *EnvFileUnreadableError) Error() string {
	return fmt.Sprintf("could not read envFile for %s: %v", e.Service, e.Err)
}

func (e *EnvFileUnreadableError) Unwrap() error { return e.Err }

// registerServiceEnvFileValuesOrWarn registers envFile values and, for each
// service whose files cannot be read, emits one warning with
// `data.reason: "envfile-unreadable"` and continues. Read-only commands do
// not otherwise need the files.
func (e *Engine) registerServiceEnvFileValuesOrWarn(services map[string]config.ServiceConfig, envName string, phase string) {
	for _, failure := range EnvFileUnreadableErrors(e.RegisterServiceEnvFileValues(services)) {
		e.emit(events.Event{
			Type:    events.TypeWarning,
			Phase:   phase,
			Level:   events.LevelWarn,
			Message: e.RedactSensitive(EnvFileUnreadableWarning(failure)),
			Data:    map[string]any{"environment": envName, "service": failure.Service, "reason": EnvFileUnreadableReason, "stream": "stderr"},
		})
	}
}

// EnvFileUnreadableReason is the `data.reason` of the warning emitted when a
// service envFile cannot be read for redaction registration.
const EnvFileUnreadableReason = "envfile-unreadable"

// EnvFileUnreadableWarning renders the warning message for an unreadable
// envFile. Callers redact it before emitting.
func EnvFileUnreadableWarning(failure *EnvFileUnreadableError) string {
	cause := ""
	if failure.Err != nil {
		cause = failure.Err.Error()
	}
	return fmt.Sprintf("Warning: could not read envFile for %s; its values will not be redacted: %s\n", failure.Service, cause)
}

// EnvFileUnreadableErrors splits an error returned by
// RegisterServiceEnvFileValues into its per-service failures.
func EnvFileUnreadableErrors(err error) []*EnvFileUnreadableError {
	if err == nil {
		return nil
	}
	parts := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts = joined.Unwrap()
	}
	failures := make([]*EnvFileUnreadableError, 0, len(parts))
	for _, part := range parts {
		var unreadable *EnvFileUnreadableError
		if errors.As(part, &unreadable) {
			failures = append(failures, unreadable)
		} else {
			failures = append(failures, &EnvFileUnreadableError{Err: part})
		}
	}
	return failures
}

// RegisterPublic marks a value that output must keep readable, such as a
// service name, image reference or git commit, even when a registered secret
// is a substring of it or a secret pattern would match it.
func (e *Engine) RegisterPublic(value string) {
	if value != "" {
		e.redactor.RegisterPublic(value)
	}
}

// RegisterServiceEnvValues registers the plain `env:` values of services that
// look like credentials (see secrets.ShouldRedactEnvValue). Ordinary settings
// stay readable so they don't mask unrelated output.
func (e *Engine) RegisterServiceEnvValues(services map[string]config.ServiceConfig) {
	e.registerEntries(secrets.ClassifyServiceEnv(services))
}

// RegisterPublicConfigNames keeps the identifiers that appear throughout
// command output (project, environment, server and service names, and
// service image references) from being masked.
func (e *Engine) RegisterPublicConfigNames(cfg *config.Config, services map[string]config.ServiceConfig) {
	if cfg != nil {
		e.RegisterPublic(cfg.Project.Name)
		for name := range cfg.Environments {
			e.RegisterPublic(name)
		}
		for name := range cfg.Servers {
			e.RegisterPublic(name)
		}
	}
	for name, service := range services {
		e.RegisterPublic(name)
		e.RegisterPublic(service.Image)
	}
}

// RedactSensitive applies the same registered-secret redaction used by the
// event stream to adapter-owned result and error fields.
func (e *Engine) RedactSensitive(value string) string {
	return e.redactor.Redact(value)
}

func (e *Engine) buildOutputWriter() io.Writer {
	if e.buildWriter != nil {
		return e.buildWriter
	}
	target := e.buildOutput
	if target == nil {
		target = os.Stdout
	}
	e.buildWriter = &redactingWriter{writer: target, redact: e.redactor.Redact}
	return e.buildWriter
}

func (e *Engine) flushBuildOutput() {
	if e.buildWriter != nil {
		_ = e.buildWriter.Flush()
	}
}

// RegisterRegistrySecrets adds every configured registry credential to the
// event redactor so pull/build output and result docs never carry them.
func (e *Engine) RegisterRegistrySecrets(cfg *config.Config) {
	e.registerEntries(secrets.ClassifyRegistryCredentials(cfg))
}

// RegisterACMEDNSSecrets adds environment-scoped DNS provider credentials to
// the same redactor used for event streams, progress output, and result docs.
func (e *Engine) RegisterACMEDNSSecrets(cfg *config.Config) {
	e.registerEntries(secrets.ClassifyACMEDNSCredentials(cfg))
}

// EventStream exposes the engine's stamped, redacting event stream so
// adapters can bridge auxiliary output (for example build logs) into it.
func (e *Engine) EventStream() *events.Stream {
	return e.stream
}

func (e *Engine) emit(event events.Event) {
	e.stream.Emit(event)
}

// info emits an info-level event whose Message renders exactly as the CLI
// used to print, preserving transcript compatibility.
func (e *Engine) info(eventType string, phase string, message string) {
	e.emit(events.Event{Type: eventType, Phase: phase, Level: events.LevelInfo, Message: message})
}

func (e *Engine) debug(eventType string, phase string, message string) {
	e.emit(events.Event{Type: eventType, Phase: phase, Level: events.LevelDebug, Message: message})
}

func (e *Engine) warn(phase string, message string) {
	e.emit(events.Event{Type: events.TypeWarning, Phase: phase, Level: events.LevelWarn, Message: message})
}
