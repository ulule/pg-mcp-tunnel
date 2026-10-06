// Package webui serves a small live dashboard of the queries recorded by every
// pg-mcp-tunnel process. Each tunnel process appends events to its own
// <target>.queries.jsonl file; this server tails all of them, aggregates them
// and renders HTML fragments that htmx swaps into the page (new rows are
// pushed over Server-Sent Events).
package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

const (
	pollInterval = 250 * time.Millisecond
	// On first discovery of a file, only its tail is replayed.
	initialTail = 2 * 1024 * 1024
)

type Server struct {
	dir        string
	targets    []string // configured targets, shown even before any event
	configPath string
	logger     *slog.Logger
	dash       *dash

	mu    sync.Mutex // guards files and pids
	files map[string]*tailFile
	pids  map[string]map[int]struct{} // target -> tunnel pids seen
}

type tailFile struct {
	offset  int64
	partial []byte
}

func New(dir string, targets []string, configPath string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		dir:        dir,
		targets:    targets,
		configPath: configPath,
		logger:     logger,
		dash:       newDash(),
		files:      make(map[string]*tailFile),
		pids:       make(map[string]map[int]struct{}),
	}
}

// Serve polls the events files and serves HTTP on l until ctx is canceled.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.poll() // load the backlog before the first client connects
	s.dash.sortByTime()
	go s.pollLoop(ctx)

	srv := &http.Server{Handler: localOnly(s.routes()), ReadHeaderTimeout: 5 * time.Second}
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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /view", s.handleView)
	mux.HandleFunc("GET /sidebar", s.handleSidebar)
	mux.HandleFunc("GET /stream", s.handleStream)
	mux.HandleFunc("GET /pending", s.handlePending)
	mux.HandleFunc("GET /event/{id}", s.handleEvent)
	mux.HandleFunc("POST /clear", s.handleClear)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config/open", s.handleOpenConfig)
	return mux
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

// ---------- pages and fragments ----------

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("render", "template", name, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) viewData(f Filter) any {
	if f.Tab == "top" {
		return s.dash.top(f)
	}
	return s.dash.live(f)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	s.render(w, "page", struct {
		Filter  Filter
		Sidebar sidebarData
		View    any
	}{f, s.dash.sidebar(f, s.targets, s.alivePIDs()), s.viewData(f)})
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	// Keep the address bar in sync so a reload restores the view.
	if q := f.Query(); q != "" {
		w.Header().Set("HX-Replace-Url", "./?"+q)
	} else {
		w.Header().Set("HX-Replace-Url", "./")
	}
	s.render(w, "view", s.viewData(f))
}

func (s *Server) handleSidebar(w http.ResponseWriter, r *http.Request) {
	s.render(w, "sidebar-oob", s.dash.sidebar(parseFilter(r), s.targets, s.alivePIDs()))
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	since, _ := strconv.ParseUint(r.FormValue("since"), 10, 64)
	s.render(w, "newbar", struct {
		N     int
		Query string
		Since uint64
	}{s.dash.pending(f, since), f.Query(), since})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	ev := s.dash.get(id)
	if ev == nil {
		http.Error(w, "event no longer buffered", http.StatusNotFound)
		return
	}
	s.render(w, "row", rowData{Ev: ev, Expanded: r.FormValue("open") == "1"})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	s.dash.clear()
	s.render(w, "view", s.viewData(parseFilter(r)))
}

// handleStream pushes rendered rows matching the filter over SSE: first the
// buffered ones newer than "since" (or the Last-Event-ID the browser sends
// when it reconnects), then live ones.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	f := parseFilter(r)
	since, _ := strconv.ParseUint(r.FormValue("since"), 10, 64)
	stale := false
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if id, ok := s.dash.parseSSEID(last); ok {
			since = id
		} else {
			stale = true // ids from a previous ui process
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := s.dash.subscribe()
	defer s.dash.unsubscribe(ch)

	var buf bytes.Buffer
	if stale {
		buf.WriteString("event: reset\ndata: reload\n\n")
	}
	sent := since
	send := func(ev *Event, fresh bool) bool {
		if ev.ID <= sent {
			return true
		}
		sent = ev.ID
		if !f.Matches(ev) {
			return true
		}
		var html bytes.Buffer
		if err := tmpl.ExecuteTemplate(&html, "row", rowData{Ev: ev, Fresh: fresh}); err != nil {
			s.logger.Error("render row", "err", err)
			return false
		}
		writeSSE(&buf, s.dash.sseID(ev.ID), "row", html.Bytes())
		return true
	}
	for _, ev := range s.dash.after(since) {
		send(ev, false)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // too slow: the browser reconnects with Last-Event-ID
			}
			buf.Reset()
			send(ev, true)
			// Coalesce whatever else is queued into one write.
			for drained := false; !drained; {
				select {
				case more, ok := <-ch:
					if !ok {
						return
					}
					send(more, true)
				default:
					drained = true
				}
			}
			if buf.Len() == 0 {
				continue
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

func writeSSE(buf *bytes.Buffer, id, event string, data []byte) {
	fmt.Fprintf(buf, "id: %s\nevent: %s\n", id, event)
	for _, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'}) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
}

// ---------- tailing ----------

func (s *Server) pollLoop(ctx context.Context) {
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			s.poll()
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

// readNew reads whatever was appended to path since the last poll and adds
// each complete line to the dashboard.
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
		var ev querylog.Event
		if json.Unmarshal(line, &ev) != nil || ev.Target == "" {
			continue
		}
		s.notePID(ev.Target, ev.PID)
		s.dash.add(ev)
	}
}

func (s *Server) notePID(target string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pids[target] == nil {
		s.pids[target] = make(map[int]struct{})
	}
	s.pids[target][pid] = struct{}{}
}

// alivePIDs returns, per target, the tunnel processes still running.
func (s *Server) alivePIDs() map[string][]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]int)
	for name, pids := range s.pids {
		for pid := range pids {
			if processAlive(pid) {
				out[name] = append(out[name], pid)
			}
		}
		sort.Ints(out[name])
	}
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
