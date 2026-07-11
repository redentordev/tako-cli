// Package engine exposes Tako's deployment workflows as a library. The CLI
// commands in cmd/ are thin adapters over this package: they parse flags into
// request structs, run the engine, and render the emitted event stream. The
// engine never prompts, never prints, and never exits the process; progress
// flows through the events sink and outcomes are returned as typed results
// and errors.
package engine

import (
	"bytes"
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
		buildOutput:   opts.BuildOutput,
	}
}

// RegisterSecret adds a value to the event redactor.
func (e *Engine) RegisterSecret(value string) {
	if value != "" {
		e.redactor.Register(value)
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
	if cfg == nil {
		return
	}
	for _, registry := range cfg.Registries {
		e.RegisterSecret(registry.Password)
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
