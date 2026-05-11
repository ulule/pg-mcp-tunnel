package sshtunnel

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingBufferRetainsTail(t *testing.T) {
	r := newRingBuffer(16, nil)
	_, _ = r.Write([]byte("hello "))
	_, _ = r.Write([]byte("world"))
	if got := r.tail(); got != "hello world" {
		t.Errorf("tail = %q, want %q", got, "hello world")
	}

	// Writing past capacity keeps only the last cap bytes.
	_, _ = r.Write([]byte(strings.Repeat("X", 20)))
	got := r.tail()
	if len(got) > 16 {
		t.Errorf("tail length = %d, want <= 16", len(got))
	}
	if !strings.HasSuffix(got, "XXXXXXXXXXXXXXXX") {
		t.Errorf("tail = %q, want XXX suffix", got)
	}
}

func TestRingBufferSingleWriteOversize(t *testing.T) {
	r := newRingBuffer(8, nil)
	_, _ = r.Write([]byte("0123456789abcdef"))
	if got := r.tail(); got != "89abcdef" {
		t.Errorf("tail = %q, want %q", got, "89abcdef")
	}
}

func TestRingBufferTeesToSink(t *testing.T) {
	var sink bytes.Buffer
	r := newRingBuffer(16, &sink)
	_, _ = r.Write([]byte("first "))
	_, _ = r.Write([]byte("second"))
	if got := sink.String(); got != "first second" {
		t.Errorf("sink = %q, want %q", got, "first second")
	}
}

func TestRingBufferEmptyTail(t *testing.T) {
	r := newRingBuffer(16, nil)
	if got := r.tail(); got != "(no ssh stderr captured)" {
		t.Errorf("empty tail = %q, want placeholder", got)
	}
}
