package events

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStreamStampsSequenceSchemaAndTime(t *testing.T) {
	buffer := &BufferSink{}
	stream := NewStream(buffer, nil)
	fixed := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	stream.SetNowFunc(func() time.Time { return fixed })

	stream.Emit(Event{Type: TypePhaseStarted, Message: "one"})
	stream.Emit(Event{Type: TypeLogLine, Message: "two", Level: LevelDebug})

	emitted := buffer.Events()
	if len(emitted) != 2 {
		t.Fatalf("events = %d, want 2", len(emitted))
	}
	if emitted[0].Seq != 1 || emitted[1].Seq != 2 {
		t.Fatalf("sequence = %d,%d, want 1,2", emitted[0].Seq, emitted[1].Seq)
	}
	if emitted[0].APIVersion != APIVersionCurrent || emitted[0].Kind != KindEvent {
		t.Fatalf("schema identity = %q %q", emitted[0].APIVersion, emitted[0].Kind)
	}
	if !emitted[0].Time.Equal(fixed) {
		t.Fatalf("time = %v, want %v", emitted[0].Time, fixed)
	}
	if emitted[0].Level != LevelInfo {
		t.Fatalf("default level = %q, want info", emitted[0].Level)
	}
	if emitted[1].Level != LevelDebug {
		t.Fatalf("explicit level = %q, want debug", emitted[1].Level)
	}
}

func TestStreamRedactsMessageAndStringData(t *testing.T) {
	buffer := &BufferSink{}
	redact := func(input string) string {
		return strings.ReplaceAll(input, "s3cret-value", "[REDACTED]")
	}
	stream := NewStream(buffer, redact)

	stream.Emit(Event{
		Type:    TypeLogLine,
		Message: "env DB_PASSWORD=s3cret-value applied",
		Data:    map[string]any{"value": "s3cret-value", "count": 2},
	})

	event := buffer.Events()[0]
	if strings.Contains(event.Message, "s3cret-value") {
		t.Fatalf("message leaked secret: %q", event.Message)
	}
	if value := event.Data["value"].(string); strings.Contains(value, "s3cret-value") {
		t.Fatalf("data leaked secret: %q", value)
	}
	if count := event.Data["count"].(int); count != 2 {
		t.Fatalf("non-string data mutated: %v", count)
	}
}

func TestNDJSONSinkWritesOneDocumentPerLine(t *testing.T) {
	var out bytes.Buffer
	sink := NewNDJSONSink(&out)
	stream := NewStream(sink, nil)
	stream.Emit(Event{Type: TypePhaseStarted, Message: "hello"})
	stream.Emit(Event{Type: TypePhaseCompleted})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var decoded Event
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("line 0 is not valid JSON: %v", err)
	}
	if decoded.Type != TypePhaseStarted || decoded.Seq != 1 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestFanoutSinkForwardsInOrder(t *testing.T) {
	first := &BufferSink{}
	second := &BufferSink{}
	fanout := NewFanoutSink(first, nil, second)
	fanout.Emit(Event{Type: TypeLogLine})
	if len(first.Events()) != 1 || len(second.Events()) != 1 {
		t.Fatalf("fanout did not reach all sinks")
	}
}
