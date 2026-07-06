// Package engine exposes Tako's deployment workflows as a library. The CLI
// commands in cmd/ are thin adapters over this package: they parse flags into
// request structs, run the engine, and render the emitted event stream. The
// engine never prompts, never prints, and never exits the process; progress
// flows through the events sink and outcomes are returned as typed results
// and errors.
package engine

import (
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
}

// New constructs an Engine. Every emitted event passes through a secrets
// redactor; operations register sensitive values (service env values, SSH
// passwords) before emitting anything that could contain them.
func New(opts Options) *Engine {
	redactor := secrets.NewRedactor()
	return &Engine{
		cliVersion:    opts.CLIVersion,
		cliCommit:     opts.CLICommit,
		redactor:      redactor,
		stream:        events.NewStream(opts.Sink, redactor.Redact),
		stateAutoSync: opts.StateAutoSync,
	}
}

// RegisterSecret adds a value to the event redactor.
func (e *Engine) RegisterSecret(value string) {
	if value != "" {
		e.redactor.Register(value)
	}
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
