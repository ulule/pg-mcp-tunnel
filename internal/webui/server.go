// Package webui serves a small live dashboard of the queries recorded by every
// pg-mcp-tunnel process. Each tunnel process appends events to its own
// <target>.queries.jsonl file; this server tails all of them and streams the
// events to the browser over Server-Sent Events.
package webui

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

//go:embed index.html
var indexHTML []byte

const (
	pollInterval   = 250 * time.Millisecond
	statusInterval = 2 * time.Second
	backlogSize    = 5000
	// On first discovery of a file, only its tail is replayed.
	initialTail = 2 * 1024 * 1024
	clientBuf   = 1024
)

type Server struct {
	dir        string
	targets    []string // configured targets, shown even before any event
	configPath string
	logger     *slog.Logger

	mu      sync.Mutex
	backlog [][]byte
	clients map[chan []byte]struct{}
	files   map[string]*tailFile
	pids    map[string]map[int]struct{} // target -> tunnel pids seen
}

type tailFile struct {
	offset  int64
	partial []byte
}

func New(dir string, targets []string, configPath string, logger *slog.Logger) *Server {
	return &Server{
		dir:        dir,
		targets:    targets,
		configPath: configPath,
		logger:     logger,
		clients:    make(map[chan []byte]struct{}),
		files:      make(map[string]*tailFile),
		pids:       make(map[string]map[int]struct{}),
	}
}

// Serve polls the events files and serves HTTP on l until ctx is canceled.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.poll() // load backlog before the first client connects
	s.sortBacklog()
	go s.pollLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config/open", s.handleOpenConfig)

	srv := &http.Server{Handler: localOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localOnly rejects requests whose Host isn't a loopback name, so a malicious
// page can't read recorded SQL through DNS rebinding.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		switch strings.Trim(host, "[]") {
		case "localhost", "127.0.0.1", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
		}
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := make(chan []byte, clientBuf)
	s.mu.Lock()
	backlog := append([][]byte(nil), s.backlog...)
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	var buf bytes.Buffer
	for _, line := range backlog {
		writeSSE(&buf, line)
	}
	writeSSE(&buf, s.statusJSON())
	_, _ = w.Write(buf.Bytes())
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return // too slow, dropped by broadcast
			}
			buf.Reset()
			writeSSE(&buf, line)
			// Drain whatever else is queued to coalesce writes.
			for drained := false; !drained; {
				select {
				case more, ok := <-ch:
					if !ok {
						return
					}
					writeSSE(&buf, more)
				default:
					drained = true
				}
			}
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(buf *bytes.Buffer, line []byte) {
	buf.WriteString("data: ")
	buf.Write(line)
	buf.WriteString("\n\n")
}

func (s *Server) pollLoop(ctx context.Context) {
	poll := time.NewTicker(pollInterval)
	status := time.NewTicker(statusInterval)
	defer poll.Stop()
	defer status.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			s.poll()
		case <-status.C:
			s.broadcast(s.statusJSON())
		}
	}
}

func (s *Server) poll() {
	paths, err := filepath.Glob(filepath.Join(s.dir, "*"+querylog.FileSuffix))
	if err != nil {
		return
	}
	for _, path := range paths {
		s.readNew(path)
	}
}

// readNew reads whatever was appended to path since the last poll and
// broadcasts each complete line.
func (s *Server) readNew(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	tf, known := s.files[path]
	if !known {
		tf = &tailFile{offset: max(0, info.Size()-initialTail)}
		s.files[path] = tf
	}
	if info.Size() < tf.offset { // truncated by the recorder
		tf.offset, tf.partial = 0, nil
	}
	if info.Size() == tf.offset {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	data := make([]byte, info.Size()-tf.offset)
	n, _ := f.ReadAt(data, tf.offset)
	data = data[:n]
	skipFirst := !known && tf.offset > 0 // started mid-file: first line is partial
	tf.offset += int64(n)

	data = append(tf.partial, data...)
	lines := bytes.Split(data, []byte{'\n'})
	tf.partial = append([]byte(nil), lines[len(lines)-1]...)
	lines = lines[:len(lines)-1]
	if skipFirst && len(lines) > 0 {
		lines = lines[1:]
	}

	for _, line := range lines {
		var ev struct {
			Target string `json:"target"`
			PID    int    `json:"pid"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		s.notePID(ev.Target, ev.PID)
		s.broadcast(line)
	}
}

// sortBacklog orders the initial backlog by timestamp: files are read one
// after the other, so without it the live view starts grouped by target.
func (s *Server) sortBacklog() {
	ts := func(line []byte) string {
		var ev struct {
			TS string `json:"ts"`
		}
		_ = json.Unmarshal(line, &ev)
		return ev.TS
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]time.Time, len(s.backlog))
	for i, line := range s.backlog {
		keys[i], _ = time.Parse(time.RFC3339Nano, ts(line))
	}
	idx := make([]int, len(s.backlog))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]].Before(keys[idx[b]]) })
	sorted := make([][]byte, len(idx))
	for i, j := range idx {
		sorted[i] = s.backlog[j]
	}
	s.backlog = sorted
}

func (s *Server) notePID(target string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pids[target] == nil {
		s.pids[target] = make(map[int]struct{})
	}
	s.pids[target][pid] = struct{}{}
}

func (s *Server) broadcast(line []byte) {
	line = bytes.Clone(line)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.backlog) >= backlogSize {
		s.backlog = s.backlog[len(s.backlog)-backlogSize+1:]
	}
	s.backlog = append(s.backlog, line)
	for ch := range s.clients {
		select {
		case ch <- line:
		default:
			// Client can't keep up: disconnect it; the browser reconnects
			// and gets the backlog.
			close(ch)
			delete(s.clients, ch)
		}
	}
}

type targetStatus struct {
	Name  string `json:"name"`
	Alive bool   `json:"alive"`
	PIDs  []int  `json:"pids,omitempty"`
}

// statusJSON reports, per target, whether a tunnel process is running.
func (s *Server) statusJSON() []byte {
	s.mu.Lock()
	byName := make(map[string]*targetStatus)
	for _, name := range s.targets {
		byName[name] = &targetStatus{Name: name}
	}
	for name, pids := range s.pids {
		st := byName[name]
		if st == nil {
			st = &targetStatus{Name: name}
			byName[name] = st
		}
		for pid := range pids {
			if processAlive(pid) {
				st.Alive = true
				st.PIDs = append(st.PIDs, pid)
			}
		}
		sort.Ints(st.PIDs)
	}
	s.mu.Unlock()

	list := make([]*targetStatus, 0, len(byName))
	for _, st := range byName {
		list = append(list, st)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	out, _ := json.Marshal(map[string]any{"kind": "status", "targets": list})
	return out
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// URL returns the browser URL for a listener address.
func URL(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String()
	}
	host := tcp.IP.String()
	if tcp.IP.IsLoopback() && tcp.IP.To4() != nil {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(host, fmt.Sprint(tcp.Port)))
}
