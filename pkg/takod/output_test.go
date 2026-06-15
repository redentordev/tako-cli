package takod

import (
	"strings"
	"testing"
)

func TestCappedOutputBufferKeepsSmallOutput(t *testing.T) {
	buffer := newCappedOutputBuffer(10)

	n, err := buffer.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Fatalf("Write returned n = %d, want 5", n)
	}
	if got := buffer.String(); got != "hello" {
		t.Fatalf("String() = %q, want hello", got)
	}
}

func TestCappedOutputBufferTruncatesWithoutShortWrite(t *testing.T) {
	buffer := newCappedOutputBuffer(5)

	n, err := buffer.Write([]byte("123456789"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 9 {
		t.Fatalf("Write returned n = %d, want original input length", n)
	}

	got := buffer.String()
	if !strings.HasPrefix(got, "12345") {
		t.Fatalf("String() = %q, want retained prefix", got)
	}
	if !strings.Contains(got, "4 byte(s) omitted") {
		t.Fatalf("String() = %q, want truncation summary", got)
	}
}

func TestCappedOutputBufferTracksMultipleWrites(t *testing.T) {
	buffer := newCappedOutputBuffer(5)

	if _, err := buffer.Write([]byte("123")); err != nil {
		t.Fatalf("first Write returned error: %v", err)
	}
	if _, err := buffer.Write([]byte("4567")); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}

	got := buffer.String()
	if !strings.HasPrefix(got, "12345") {
		t.Fatalf("String() = %q, want retained prefix across writes", got)
	}
	if !strings.Contains(got, "2 byte(s) omitted") {
		t.Fatalf("String() = %q, want cumulative truncation summary", got)
	}
}
