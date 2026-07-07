package ptystream

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := []Frame{
		{Type: FrameContainer, Payload: []byte("demo_production_web_1")},
		{Type: FrameStdout, Payload: []byte("hello\r\n")},
		{Type: FrameStdin, Payload: []byte("ls -la\n")},
		{Type: FrameResize, Payload: EncodeResize(Winsize{Cols: 120, Rows: 40})},
		{Type: FrameStdout, Payload: nil},
		{Type: FrameExit, Payload: EncodeExit(7)},
	}
	for _, frame := range frames {
		if err := WriteFrame(&buf, frame.Type, frame.Payload); err != nil {
			t.Fatalf("WriteFrame(%d) returned error: %v", frame.Type, err)
		}
	}
	for i, want := range frames {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %d returned error: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d = %#v, want %#v", i, got, want)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing read error = %v, want io.EOF", err)
	}
}

func TestFramePinnedWireEncoding(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("hi")); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}
	want := []byte{2, 0, 0, 0, 2, 'h', 'i'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wire bytes = %v, want %v (type, 4-byte BE length, payload)", buf.Bytes(), want)
	}
}

func TestReadFrameRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{FrameStdout, 0xFF, 0xFF, 0xFF, 0xFF})
	if _, err := ReadFrame(&buf); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want oversized payload rejection", err)
	}
}

func TestReadFrameReportsTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}
	truncated := bytes.NewReader(buf.Bytes()[:buf.Len()-2])
	if _, err := ReadFrame(truncated); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want truncated payload error", err)
	}
}

func TestResizeAndExitCodecs(t *testing.T) {
	size, err := DecodeResize(EncodeResize(Winsize{Cols: 211, Rows: 52}))
	if err != nil || size.Cols != 211 || size.Rows != 52 {
		t.Fatalf("resize round trip = %+v, %v", size, err)
	}
	if _, err := DecodeResize([]byte{1, 2, 3}); err == nil {
		t.Fatal("short resize payload accepted")
	}
	for _, code := range []int{0, 7, 255, -1} {
		got, err := DecodeExit(EncodeExit(code))
		if err != nil || got != code {
			t.Fatalf("exit round trip(%d) = %d, %v", code, got, err)
		}
	}
	if _, err := DecodeExit(nil); err == nil {
		t.Fatal("empty exit payload accepted")
	}
}

func TestWriterSerializesConcurrentFrames(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = writer.WriteFrame(FrameStdin, []byte("0123456789"))
		}()
	}
	wg.Wait()
	for i := 0; i < 50; i++ {
		frame, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d read error: %v (interleaved writes?)", i, err)
		}
		if frame.Type != FrameStdin || string(frame.Payload) != "0123456789" {
			t.Fatalf("frame %d corrupted: %#v", i, frame)
		}
	}
}
