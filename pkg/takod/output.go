package takod

import (
	"bytes"
	"fmt"
)

const defaultCommandOutputMaxBytes = 1 << 20

type cappedOutputBuffer struct {
	buf       bytes.Buffer
	maxBytes  int
	truncated int64
}

func newCappedOutputBuffer(maxBytes int) *cappedOutputBuffer {
	if maxBytes <= 0 {
		maxBytes = defaultCommandOutputMaxBytes
	}
	return &cappedOutputBuffer{maxBytes: maxBytes}
}

func (b *cappedOutputBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() < b.maxBytes {
		remaining := b.maxBytes - b.buf.Len()
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
		if remaining < len(p) {
			b.truncated += int64(len(p) - remaining)
		}
	} else {
		b.truncated += int64(len(p))
	}
	return len(p), nil
}

func (b *cappedOutputBuffer) String() string {
	out := b.buf.String()
	if b.truncated > 0 {
		out += fmt.Sprintf("\n... output truncated, %d byte(s) omitted", b.truncated)
	}
	return out
}
