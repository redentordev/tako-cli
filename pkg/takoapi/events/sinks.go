package events

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// NopSink discards all events.
type NopSink struct{}

func (NopSink) Emit(Event) {}

// BufferSink retains emitted events in order, for tests and for consumers
// that assemble a result after the fact.
type BufferSink struct {
	mu     sync.Mutex
	events []Event
}

func (b *BufferSink) Emit(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
}

// Events returns a copy of the buffered events in emission order.
func (b *BufferSink) Events() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.events))
	copy(out, b.events)
	return out
}

// FanoutSink forwards each event to every child sink in order.
type FanoutSink struct {
	sinks []Sink
}

func NewFanoutSink(sinks ...Sink) *FanoutSink {
	children := make([]Sink, 0, len(sinks))
	for _, sink := range sinks {
		if sink != nil {
			children = append(children, sink)
		}
	}
	return &FanoutSink{sinks: children}
}

func (f *FanoutSink) Emit(event Event) {
	for _, sink := range f.sinks {
		sink.Emit(event)
	}
}

// NDJSONSink writes one JSON document per event line. Write errors are
// dropped: event emission must never fail an operation.
type NDJSONSink struct {
	mu      sync.Mutex
	writer  io.Writer
	encoder *json.Encoder
}

func NewNDJSONSink(writer io.Writer) *NDJSONSink {
	return &NDJSONSink{writer: writer, encoder: json.NewEncoder(writer)}
}

func (n *NDJSONSink) Emit(event Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_ = n.encoder.Encode(event)
}

// Stream stamps events with sequence numbers, timestamps, and schema identity,
// applies a redaction function to all string content, and forwards them to a
// sink. All engine emissions go through a Stream so no event can bypass
// redaction.
type Stream struct {
	mu     sync.Mutex
	seq    int64
	sink   Sink
	redact func(string) string
	now    func() time.Time
}

// NewStream wraps sink. redact may be nil for streams that carry no secret
// material (tests); engine operations must always pass a redactor.
func NewStream(sink Sink, redact func(string) string) *Stream {
	if sink == nil {
		sink = NopSink{}
	}
	return &Stream{sink: sink, redact: redact, now: time.Now}
}

// SetNowFunc overrides the timestamp source, for deterministic tests.
func (s *Stream) SetNowFunc(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Emit stamps and forwards one event. Message and string Data values pass
// through the redactor.
func (s *Stream) Emit(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	event.Seq = s.seq
	event.APIVersion = APIVersionCurrent
	event.Kind = KindEvent
	event.Time = s.now()
	if event.Level == "" {
		event.Level = LevelInfo
	}
	if s.redact != nil {
		event.Message = s.redact(event.Message)
		if len(event.Data) > 0 {
			redacted := make(map[string]any, len(event.Data))
			for key, value := range event.Data {
				if str, ok := value.(string); ok {
					redacted[key] = s.redact(str)
				} else {
					redacted[key] = value
				}
			}
			event.Data = redacted
		}
	}
	s.sink.Emit(event)
}
