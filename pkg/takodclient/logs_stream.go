package takodclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Logs stream frame types (POST /v1/logs/stream, capability logs.stream-v1).
const (
	LogsFrameLine   = "line"
	LogsFrameSource = "source"
	LogsFrameRun    = "run"
	LogsFrameReady  = "ready"
	LogsFrameEnd    = "end"
	LogsFrameError  = "error"
)

// CapabilityLogsStreamV1 is the takod capability guarding LogsStreamEndpoint.
const CapabilityLogsStreamV1 = "logs.stream-v1"

// LogsStreamFrame is one NDJSON frame of the takod logs stream:
//   - line: one container log line (service, slot, container, revision,
//     stream stdout|stderr, Docker time, data)
//   - source: a container attached or detached (event)
//   - run: a job run started or finished (job, exitCode)
//   - ready: initial containers attached (sources)
//   - end / error: terminal frames; error carries message
type LogsStreamFrame struct {
	Type      string `json:"type"`
	Event     string `json:"event,omitempty"`
	Service   string `json:"service,omitempty"`
	Slot      string `json:"slot,omitempty"`
	Container string `json:"container,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Stream    string `json:"stream,omitempty"`
	Time      string `json:"time,omitempty"`
	Data      string `json:"data,omitempty"`
	Job       string `json:"job,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Sources   *int   `json:"sources,omitempty"`
	Message   string `json:"message,omitempty"`
}

// LogsStreamError is a node-side failure reported in-band by an error frame.
type LogsStreamError struct {
	Message string
}

func (e *LogsStreamError) Error() string { return e.Message }

// ErrLogsStreamTruncated means the stream ended without a terminal frame
// (connection lost, or the request was cancelled).
var ErrLogsStreamTruncated = errors.New("logs stream ended without a terminal frame")

// maxLogsFrameBytes bounds one frame; Docker splits long log lines at 16KiB.
const maxLogsFrameBytes = 4 << 20

// DecodeLogsStream reads NDJSON frames from reader and calls handle for every
// known non-terminal frame, ignoring unknown frame types (the protocol grows
// additively). It returns nil after an end frame, a *LogsStreamError after an
// error frame, ErrLogsStreamTruncated when the input ends early, or the
// first error returned by handle.
func DecodeLogsStream(reader io.Reader, handle func(LogsStreamFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogsFrameBytes)
	for scanner.Scan() {
		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}
		var frame LogsStreamFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return fmt.Errorf("invalid logs stream frame: %w", err)
		}
		switch frame.Type {
		case LogsFrameEnd:
			return nil
		case LogsFrameError:
			message := frame.Message
			if message == "" {
				message = "node reported a logs stream error"
			}
			return &LogsStreamError{Message: message}
		case LogsFrameLine, LogsFrameSource, LogsFrameRun, LogsFrameReady:
			if err := handle(frame); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ErrLogsStreamTruncated
}
