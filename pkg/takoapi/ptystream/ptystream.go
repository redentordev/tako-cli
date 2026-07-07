// Package ptystream defines the framed duplex byte-stream protocol spoken
// over a hijacked takod exec connection. It is the machine contract for
// interactive exec: a control plane bridges these frames to a browser
// terminal over its own SSH connection.
//
// After the HTTP/1.1 upgrade handshake (Upgrade: tako-pty/1) both sides
// exchange frames: 1 type byte, a 4-byte big-endian payload length, then the
// payload. The server's terminal frame is Exit; nothing follows it.
package ptystream

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Protocol is the HTTP Upgrade token that selects this stream version.
// Version bumps get a new token; the server rejects unknown tokens.
const Protocol = "tako-pty/1"

// MaxPayload bounds a single frame's payload. Larger frames are a protocol
// error: output is chunked by the PTY read loop and stdin by the terminal.
const MaxPayload = 1 << 20

// Frame types. Stdin/Resize flow client to server; the rest flow server to
// client.
const (
	// FrameStdin carries raw input bytes for the remote process.
	FrameStdin byte = 1
	// FrameStdout carries raw output bytes. A PTY merges stdout and stderr
	// into one stream by nature; non-PTY interactive exec merges them to
	// match, so there is no separate stderr frame.
	FrameStdout byte = 2
	// FrameResize carries a terminal size change: cols and rows as
	// big-endian uint16s (4 payload bytes).
	FrameResize byte = 3
	// FrameExit is the terminal frame: the remote command's exit code as a
	// big-endian int32 (4 payload bytes). -1 reports a run failure that
	// produced no exit code.
	FrameExit byte = 4
	// FrameContainer names the resolved target container. The server sends
	// it once before any output.
	FrameContainer byte = 5
	// FrameError carries a fatal server-side message emitted before Exit.
	FrameError byte = 6
)

// Frame is one decoded protocol frame.
type Frame struct {
	Type    byte
	Payload []byte
}

// ReadFrame decodes the next frame from r.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	frameType := header[0]
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxPayload {
		return Frame{}, fmt.Errorf("ptystream: frame payload %d exceeds %d bytes", length, MaxPayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, fmt.Errorf("ptystream: truncated frame payload: %w", err)
	}
	return Frame{Type: frameType, Payload: payload}, nil
}

// WriteFrame encodes one frame to w. Payloads above MaxPayload must be
// chunked by the caller.
func WriteFrame(w io.Writer, frameType byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("ptystream: frame payload %d exceeds %d bytes", len(payload), MaxPayload)
	}
	var header [5]byte
	header[0] = frameType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Writer serializes concurrent frame writes onto one connection (e.g. a
// stdin pump and a SIGWINCH handler sharing the client side).
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter wraps w for concurrent frame writing.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteFrame writes one frame atomically with respect to other WriteFrame
// calls on this Writer.
func (fw *Writer) WriteFrame(frameType byte, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return WriteFrame(fw.w, frameType, payload)
}

// Winsize is a terminal size in character cells.
type Winsize struct {
	Cols uint16
	Rows uint16
}

// EncodeResize renders a Resize frame payload.
func EncodeResize(size Winsize) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[0:], size.Cols)
	binary.BigEndian.PutUint16(payload[2:], size.Rows)
	return payload
}

// DecodeResize parses a Resize frame payload.
func DecodeResize(payload []byte) (Winsize, error) {
	if len(payload) != 4 {
		return Winsize{}, fmt.Errorf("ptystream: resize payload must be 4 bytes, got %d", len(payload))
	}
	return Winsize{
		Cols: binary.BigEndian.Uint16(payload[0:]),
		Rows: binary.BigEndian.Uint16(payload[2:]),
	}, nil
}

// EncodeExit renders an Exit frame payload.
func EncodeExit(code int) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(int32(code)))
	return payload
}

// DecodeExit parses an Exit frame payload.
func DecodeExit(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("ptystream: exit payload must be 4 bytes, got %d", len(payload))
	}
	return int(int32(binary.BigEndian.Uint32(payload))), nil
}
