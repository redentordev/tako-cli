package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

var cliEngineInstance *engine.Engine

// cliEngine returns the process-wide engine. In the default text mode events
// render as human terminal output on stdout. In machine modes (--output json
// or --events ndjson) stdout carries only parseable output: NDJSON events
// and/or the final result document, with human rendering and build logs
// routed to stderr.
func cliEngine() *engine.Engine {
	if cliEngineInstance == nil {
		var sink events.Sink
		var buildOutput io.Writer
		switch {
		case eventsFormatFlag == eventsFormatNDJSON:
			sink = events.NewFanoutSink(events.NewNDJSONSink(os.Stdout), humanEventSink{writer: os.Stderr})
			buildOutput = os.Stderr
		case outputFormatFlag == outputFormatJSON:
			sink = humanEventSink{writer: os.Stderr}
			buildOutput = os.Stderr
		default:
			sink = humanEventSink{writer: os.Stdout}
		}
		cliEngineInstance = engine.New(engine.Options{
			CLIVersion:  Version,
			CLICommit:   GitCommit,
			Sink:        sink,
			BuildOutput: buildOutput,
			StateAutoSync: func(pool *ssh.Pool, cfg *config.Config, envName string) error {
				return SyncStateOnDeployWithPool(pool, cfg, envName)
			},
		})
	}
	return cliEngineInstance
}

// machineOutputEnabled reports whether stdout is reserved for machine-
// parseable output and prompts must be replaced with typed errors.
func machineOutputEnabled() bool {
	return outputFormatFlag == outputFormatJSON || eventsFormatFlag == eventsFormatNDJSON
}

// humanOut returns the writer for human terminal output: stdout in text
// mode, stderr when a machine mode reserves stdout for parseable output.
func humanOut() io.Writer {
	if machineOutputEnabled() {
		return os.Stderr
	}
	return os.Stdout
}

// emitResultDocument delivers an operation's final result to machine
// consumers: as a terminal `result` event in NDJSON mode and/or as a JSON
// document on stdout in --output json mode. Text mode emits nothing.
func emitResultDocument(result any) error {
	if eventsFormatFlag == eventsFormatNDJSON {
		cliEngine().EventStream().Emit(events.Event{
			Type:  events.TypeResult,
			Level: events.LevelInfo,
			Data:  map[string]any{"result": result},
		})
	}
	if outputFormatFlag == outputFormatJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("failed to encode result document: %w", err)
		}
	}
	return nil
}

// confirmationRequiredDocument is the machine-output payload emitted when an
// operation needs explicit approval: the serialized plan plus the reason.
type confirmationRequiredDocument struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Reason     string            `json:"reason"`
	Plan       engine.DeployPlan `json:"plan"`
}

func newConfirmationRequiredDocument(reason string, plan engine.DeployPlan) confirmationRequiredDocument {
	return confirmationRequiredDocument{
		APIVersion: plan.APIVersion,
		Kind:       "ConfirmationRequired",
		Reason:     reason,
		Plan:       plan,
	}
}

// operationConfirmationRequiredDocument is emitted in machine modes when a
// destructive operation without a deploy plan (remove, destroy) needs
// explicit approval before executing.
type operationConfirmationRequiredDocument struct {
	APIVersion  string   `json:"apiVersion"`
	Kind        string   `json:"kind"`
	Reason      string   `json:"reason"`
	Operation   string   `json:"operation"`
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Servers     []string `json:"servers,omitempty"`
}

func newOperationConfirmationRequiredDocument(reason string, operation string, project string, environment string, servers []string) operationConfirmationRequiredDocument {
	return operationConfirmationRequiredDocument{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        "ConfirmationRequired",
		Reason:      reason,
		Operation:   operation,
		Project:     project,
		Environment: environment,
		Servers:     servers,
	}
}

// stateForgetNodeConfirmationRequiredDocument is emitted in machine modes when
// forget-node needs --yes approval before mutating replicated runtime state.
type stateForgetNodeConfirmationRequiredDocument struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	Operation   string `json:"operation"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	RetiredNode string `json:"retiredNode"`
}

func newStateForgetNodeConfirmationRequiredDocument(reason string, project string, environment string, retiredNode string) stateForgetNodeConfirmationRequiredDocument {
	return stateForgetNodeConfirmationRequiredDocument{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        "ConfirmationRequired",
		Reason:      reason,
		Operation:   "state.forget-node",
		Project:     project,
		Environment: environment,
		RetiredNode: retiredNode,
	}
}

// verifyPlanFileMatches asserts that a previously reviewed plan document
// still matches the freshly computed plan, refusing to apply on drift.
func verifyPlanFileMatches(planPath string, current engine.DeployPlan) error {
	payload, err := os.ReadFile(planPath)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("failed to read plan file %q: %w", planPath, err)}
	}
	var reviewed engine.DeployPlan
	if err := json.Unmarshal(payload, &reviewed); err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("failed to parse plan file %q: %w", planPath, err)}
	}
	if reviewed.Hash() != current.Hash() {
		return &engine.InvalidRequestError{Err: fmt.Errorf("plan drift detected: reviewed plan %q no longer matches the computed plan; re-run with --plan-only and review again", planPath)}
	}
	return nil
}

// humanEventSink renders engine events as the CLI's terminal output. Event
// messages carry the exact bytes the command historically printed, so this
// sink writes them verbatim; debug-level events render only with --verbose.
type humanEventSink struct {
	writer io.Writer
}

func (s humanEventSink) Emit(event events.Event) {
	if event.Level == events.LevelDebug && !verbose {
		return
	}
	if event.Message == "" {
		return
	}
	if stream, ok := event.Data["stream"].(string); ok && stream == "stderr" {
		fmt.Fprint(os.Stderr, event.Message)
		return
	}
	writer := s.writer
	if writer == nil {
		writer = os.Stdout
	}
	fmt.Fprint(writer, event.Message)
}
