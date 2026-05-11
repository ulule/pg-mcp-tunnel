package sshtunnel

import (
	"io"
	"strings"
	"sync"
)

// ringBuffer is a thread-safe io.Writer that retains the last `cap` bytes
// written to it and tees every write through to an optional sink writer.
//
// Used to capture the tail of ssh's stderr so failure messages can include
// the relevant lines (e.g. "Permission denied (publickey)") without buffering
// the entire stream in memory or scraping the log file on disk.
type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	cap  int
	sink io.Writer
}

func newRingBuffer(cap int, sink io.Writer) *ringBuffer {
	if cap < 1 {
		cap = 1
	}
	return &ringBuffer{cap: cap, sink: sink}
}

// Write appends p to the in-memory ring (keeping at most cap bytes) and
// forwards p to the sink, if any. It always returns len(p) and nil on the
// ring side; if the sink returns an error, that error is propagated.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	if len(p) >= r.cap {
		// New data alone exceeds capacity — keep only the tail.
		r.buf = append(r.buf[:0], p[len(p)-r.cap:]...)
	} else {
		r.buf = append(r.buf, p...)
		if len(r.buf) > r.cap {
			r.buf = r.buf[len(r.buf)-r.cap:]
		}
	}
	r.mu.Unlock()

	if r.sink != nil {
		return r.sink.Write(p)
	}
	return len(p), nil
}

// tail returns the buffered bytes as a trimmed string. Returns a placeholder
// when nothing has been written, so error messages aren't bare.
func (r *ringBuffer) tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := strings.TrimSpace(string(r.buf))
	if s == "" {
		return "(no ssh stderr captured)"
	}
	return s
}
