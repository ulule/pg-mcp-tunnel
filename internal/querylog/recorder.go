package querylog

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// FileSuffix is appended to the target name to form its events file, which
// lives next to the target's log file.
const FileSuffix = ".queries.jsonl"

// maxFileSize caps the events file; it is truncated once it grows past this.
const maxFileSize = 20 * 1024 * 1024

// Recorder appends events for one target to a JSON-lines file.
type Recorder struct {
	target string
	pid    int
	now    func() time.Time

	mu   sync.Mutex
	f    *os.File
	size int64

	nextConn atomic.Uint64
}

func OpenRecorder(path, target string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r := &Recorder{target: target, pid: os.Getpid(), now: time.Now, f: f, size: info.Size()}
	r.Record(Event{Kind: KindStart})
	return r, nil
}

// NewSniffer returns a Sniffer for one proxied connection whose events are
// written to this recorder.
func (r *Recorder) NewSniffer() *Sniffer {
	return newSniffer(r.nextConn.Add(1), r.Record, r.now)
}

// Record writes one event. Failures are dropped: observing queries must never
// interfere with the tunnel.
func (r *Recorder) Record(e Event) {
	if e.Time.IsZero() {
		e.Time = r.now()
	}
	e.Target, e.PID = r.target, r.pid
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	if r.size+int64(len(line)) > maxFileSize {
		if r.f.Truncate(0) == nil {
			r.size = 0
		}
	}
	n, _ := r.f.Write(line) // single write so concurrent appenders don't interleave
	r.size += int64(n)
}

func (r *Recorder) Close() error {
	r.Record(Event{Kind: KindStop})
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.f.Close()
	r.f = nil
	return err
}
