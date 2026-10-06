package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

func TestLocalOnlyRejectsForeignHosts(t *testing.T) {
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for host, want := range map[string]int{
		"localhost:7878":  http.StatusOK,
		"127.0.0.1:7878":  http.StatusOK,
		"[::1]:7878":      http.StatusOK,
		"evil.com:7878":   http.StatusForbidden,
		"rebind.attacker": http.StatusForbidden,
	} {
		req := httptest.NewRequest("GET", "/events", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: got %d, want %d", host, rec.Code, want)
		}
	}
}

func TestTailHandlesPartialLinesAndTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db"+querylog.FileSuffix)
	s := New(dir, nil, "", nil)

	write := func(flag int, data string) {
		f, err := os.OpenFile(path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(data); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}

	write(os.O_APPEND, `{"target":"db","pid":1,"kind":"query","sql":"a"}`+"\n"+`{"target":"db","pid":1,"ki`)
	s.poll()
	if len(s.backlog) != 1 {
		t.Fatalf("after partial write: backlog=%d, want 1", len(s.backlog))
	}

	write(os.O_APPEND, `nd":"query","sql":"b"}`+"\n")
	s.poll()
	if len(s.backlog) != 2 || string(s.backlog[1]) != `{"target":"db","pid":1,"kind":"query","sql":"b"}` {
		t.Fatalf("after completing line: %q", s.backlog)
	}

	write(os.O_TRUNC, `{"target":"db","pid":2,"kind":"start"}`+"\n")
	s.poll()
	if len(s.backlog) != 3 || string(s.backlog[2]) != `{"target":"db","pid":2,"kind":"start"}` {
		t.Fatalf("after truncation: %q", s.backlog)
	}
	if _, ok := s.pids["db"][2]; !ok {
		t.Fatalf("pid 2 not tracked: %v", s.pids)
	}
}

func TestMaskSecrets(t *testing.T) {
	in := "targets:\n  a:\n    postgres:\n      uri: postgresql://bob:s3cr3t@localhost:5432/db?sslmode=disable\n      password: hunter2\n      user: bob\n"
	got := maskSecrets(in)
	for _, leak := range []string{"s3cr3t", "hunter2"} {
		if strings.Contains(got, leak) {
			t.Errorf("secret %q not masked:\n%s", leak, got)
		}
	}
	for _, keep := range []string{"postgresql://bob:***@localhost:5432/db", `password: "***"`, "user: bob"} {
		if !strings.Contains(got, keep) {
			t.Errorf("missing %q in:\n%s", keep, got)
		}
	}
}

func TestOpenConfigRequiresSameOrigin(t *testing.T) {
	s := New(t.TempDir(), nil, "/nonexistent/config.yaml", nil)
	for origin, want := range map[string]int{
		"http://evil.com":       http.StatusForbidden,
		"http://localhost:7878": http.StatusNotFound, // allowed, but the file doesn't exist
	} {
		req := httptest.NewRequest("POST", "/config/open", nil)
		req.Host = "localhost:7878"
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.handleOpenConfig(rec, req)
		if rec.Code != want {
			t.Errorf("origin %q: got %d, want %d", origin, rec.Code, want)
		}
	}
}
