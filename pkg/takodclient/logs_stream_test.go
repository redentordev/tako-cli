package takodclient

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeLogsStreamDeliversKnownFramesAndIgnoresUnknown(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"ready","sources":1}`,
		`{"type":"source","event":"attached","service":"web","slot":"1","container":"web-1"}`,
		``,
		`{"type":"line","service":"web","slot":"1","container":"web-1","revision":"r1","stream":"stderr","time":"2026-10-11T12:00:00Z","data":"boom","futureField":true}`,
		`{"type":"progress","percent":50}`,
		`{"type":"run","event":"finished","job":"nightly","exitCode":3}`,
		`{"type":"end"}`,
		`{"type":"line","data":"after end"}`,
	}, "\n")
	var got []LogsStreamFrame
	err := DecodeLogsStream(strings.NewReader(input), func(frame LogsStreamFrame) error {
		got = append(got, frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("frames = %#v, want 4 known frames", got)
	}
	line := got[2]
	if line.Type != LogsFrameLine || line.Stream != "stderr" || line.Data != "boom" || line.Revision != "r1" || line.Time == "" {
		t.Fatalf("line frame = %#v", line)
	}
	if run := got[3]; run.ExitCode == nil || *run.ExitCode != 3 || run.Job != "nightly" {
		t.Fatalf("run frame = %#v", run)
	}
	if ready := got[0]; ready.Sources == nil || *ready.Sources != 1 {
		t.Fatalf("ready frame = %#v", ready)
	}
}

func TestDecodeLogsStreamTerminalErrors(t *testing.T) {
	err := DecodeLogsStream(strings.NewReader(`{"type":"line","data":"x"}`+"\n"+`{"type":"error","message":"docker failed"}`+"\n"), func(LogsStreamFrame) error { return nil })
	var streamErr *LogsStreamError
	if !errors.As(err, &streamErr) || streamErr.Message != "docker failed" {
		t.Fatalf("error = %v, want LogsStreamError", err)
	}
	if err := DecodeLogsStream(strings.NewReader(`{"type":"line","data":"x"}`+"\n"), func(LogsStreamFrame) error { return nil }); !errors.Is(err, ErrLogsStreamTruncated) {
		t.Fatalf("truncated error = %v", err)
	}
	if err := DecodeLogsStream(strings.NewReader("not json\n"), func(LogsStreamFrame) error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid logs stream frame") {
		t.Fatalf("malformed error = %v", err)
	}
	if LogsStreamEndpoint() != "/v1/logs/stream" {
		t.Fatalf("endpoint = %q", LogsStreamEndpoint())
	}
}
