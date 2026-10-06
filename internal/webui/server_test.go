package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		req := httptest.NewRequest("GET", "/view", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: got %d, want %d", host, rec.Code, want)
		}
	}
}

func sqls(d *dash) []string {
	var out []string
	for _, ev := range d.after(0) {
		out = append(out, ev.SQL)
	}
	return out
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
	if got := sqls(s.dash); len(got) != 1 {
		t.Fatalf("after partial write: %q, want 1 event", got)
	}

	write(os.O_APPEND, `nd":"query","sql":"b"}`+"\n")
	s.poll()
	if got := sqls(s.dash); len(got) != 2 || got[1] != "b" {
		t.Fatalf("after completing line: %q", got)
	}

	write(os.O_TRUNC, `{"target":"db","pid":2,"kind":"start"}`+"\n")
	s.poll()
	evs := s.dash.after(0)
	if len(evs) != 3 || evs[2].Kind != "start" || evs[2].ID != 3 {
		t.Fatalf("after truncation: %+v", evs)
	}
	if _, ok := s.pids["db"][2]; !ok {
		t.Fatalf("pid 2 not tracked: %v", s.pids)
	}
}

func TestFilterMatches(t *testing.T) {
	ev := func(kind, sql, errMsg string, ms float64) *Event {
		return &Event{Event: querylog.Event{Target: "db", Kind: kind, SQL: sql, Error: errMsg, DurationMS: ms, Tool: "execute_sql"}}
	}
	def := Filter{Only: "all", Noise: true, Tools: true, Tab: "live", Sort: "total"}
	cases := []struct {
		name string
		f    func(Filter) Filter
		ev   *Event
		want bool
	}{
		{"query", nil, ev("query", "SELECT 1", "", 1), true},
		{"noise hidden", nil, ev("query", "BEGIN", "", 1), false},
		{"noise shown", func(f Filter) Filter { f.Noise = false; return f }, ev("query", "BEGIN", "", 1), true},
		{"noise error kept", nil, ev("query", "COMMIT", "boom", 1), true},
		{"tool", nil, ev("tool", "SET x = 1", "", 1), true},
		{"tool hidden", func(f Filter) Filter { f.Tools = false; return f }, ev("tool", "SELECT 1", "", 1), false},
		{"sys hidden", nil, ev("connect", "", "", 0), false},
		{"sys shown", func(f Filter) Filter { f.Sys = true; return f }, ev("connect", "", "", 0), true},
		{"errors only", func(f Filter) Filter { f.Only = "err"; return f }, ev("query", "SELECT 1", "", 1), false},
		{"slow only", func(f Filter) Filter { f.Only = "slow"; return f }, ev("query", "SELECT 1", "", 150), true},
		{"text", func(f Filter) Filter { f.Q = "users"; return f }, ev("query", "SELECT * FROM Users", "", 1), true},
		{"text tool name", func(f Filter) Filter { f.Q = "execute_sql"; return f }, ev("tool", "SELECT 1", "", 1), true},
		{"other target", func(f Filter) Filter { f.Target = "other"; return f }, ev("query", "SELECT 1", "", 1), false},
	}
	for _, c := range cases {
		f := def
		if c.f != nil {
			f = c.f(f)
		}
		if got := f.Matches(c.ev); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFilterQueryRoundTrip(t *testing.T) {
	r := httptest.NewRequest("GET", "/view?target=db&q=Users&only=err&noise=0&noise=1&sys=0&sys=1&tools=0&tab=top&sort=tokens", nil)
	f := parseFilter(r)
	want := Filter{Target: "db", Q: "users", Only: "err", Noise: true, Sys: true, Tools: false, Tab: "top", Sort: "tokens"}
	if f != want {
		t.Fatalf("parseFilter = %+v, want %+v", f, want)
	}
	if got := parseFilter(httptest.NewRequest("GET", "/view?"+f.Query(), nil)); got != f {
		t.Fatalf("round trip = %+v, want %+v", got, f)
	}
	if q := parseFilter(httptest.NewRequest("GET", "/", nil)).Query(); q != "" {
		t.Errorf("default filter query = %q, want empty", q)
	}
}

func TestFingerprint(t *testing.T) {
	for in, want := range map[string]string{
		"/* crystaldba */ SELECT * FROM t WHERE id = 42 AND name = 'bob'": "SELECT * FROM t WHERE id = ? AND name = ?",
		"select 1 from t where id in (1, 2, 3)":                           "select ? from t where id in (?, …)",
		"select * from t where id IN ($1)":                                "select * from t where id IN (?, …)",
	} {
		if got := fingerprint(in); got != want {
			t.Errorf("fingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHighlightEscapes(t *testing.T) {
	got := string(highlight("SELECT '<b>' -- x<y"))
	want := `<span class="k">SELECT</span> <span class="s">&#39;&lt;b&gt;&#39;</span> <span class="c">-- x&lt;y</span>`
	if got != want {
		t.Errorf("highlight = %s\nwant      %s", got, want)
	}
}

// testServer serves the routes with a few events loaded.
func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New(t.TempDir(), []string{"db", "idle"}, "", nil)
	now := time.Now()
	for i, e := range []querylog.Event{
		{Target: "db", PID: os.Getpid(), Kind: "connect", Conn: 1, Database: "shop", User: "bob"},
		{Target: "db", PID: os.Getpid(), Kind: "query", Conn: 1, SQL: "SELECT * FROM users WHERE id = 1", DurationMS: 2.5, Tag: "SELECT 1", Rows: 1},
		{Target: "db", PID: os.Getpid(), Kind: "query", Conn: 1, SQL: "SELECT * FROM users WHERE id = 2", DurationMS: 150, Tag: "SELECT 1", Rows: 1},
		{Target: "db", PID: os.Getpid(), Kind: "query", Conn: 1, SQL: "SELECT * FROM nope", Error: "relation \"nope\" does not exist", SQLState: "42P01"},
		{Target: "db", PID: os.Getpid(), Kind: "tool", Tool: "execute_sql", SQL: "SELECT * FROM users WHERE id = 1", DurationMS: 3, Tokens: 12000, TokensIn: 10, Bytes: 42000},
	} {
		e.Time = now.Add(time.Duration(i) * time.Millisecond)
		s.dash.add(e)
	}
	s.notePID("db", os.Getpid())
	ts := httptest.NewServer(localOnly(s.routes()))
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&b)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b.String())
	}
	return b.String()
}

func TestRoutes(t *testing.T) {
	_, ts := testServer(t)

	page := get(t, ts, "/")
	for _, want := range []string{`id="filters"`, `sse-connect="stream?since=5"`, "static/htmx-2.0.11.min.js", "≈ 12k tok", `class="tname">execute_sql`, "42P01"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}

	if v := get(t, ts, "/view?only=err"); !strings.Contains(v, "nope") || strings.Contains(v, "users") {
		t.Errorf("errors-only view: %s", v)
	}
	if v := get(t, ts, "/view?q=zzz"); !strings.Contains(v, "Nothing matches") {
		t.Errorf("filtered view: %s", v)
	}

	top := get(t, ts, "/view?tab=top&sort=tokens")
	if !strings.Contains(top, "≈ 12k") || !strings.Contains(top, `data-value="tokens" class="on"`) {
		t.Errorf("top view: %s", top)
	}

	side := get(t, ts, "/sidebar?target=db")
	for _, want := range []string{`data-value="db" style`, "idle", `hx-swap-oob="outerHTML:#kpis"`, "⇄ 1", "≈ 12k tok"} {
		if !strings.Contains(side, want) {
			t.Errorf("sidebar missing %q:\n%s", want, side)
		}
	}

	if row := get(t, ts, "/event/2?open=1"); !strings.Contains(row, `class="detail"`) || !strings.Contains(row, "conn <b>#1</b>") {
		t.Errorf("expanded row: %s", row)
	}
	if resp, _ := http.Get(ts.URL + "/event/999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown event: %d", resp.StatusCode)
	}

	if bar := get(t, ts, "/pending?since=2"); !strings.Contains(bar, "3 new") {
		t.Errorf("pending: %s", bar)
	}
}

func TestStream(t *testing.T) {
	s, ts := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream?only=err&since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	next := func() (id, event, data string) {
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if event != "" {
					return
				}
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data += line[6:] + "\n"
			}
		}
		t.Fatal("stream ended")
		return
	}

	// Backlog: only the error matches.
	id, event, data := next()
	if event != "row" || id != s.dash.sseID(4) || !strings.Contains(data, "nope") {
		t.Fatalf("first message: %s %s %s", id, event, data)
	}
	// Live: a new error is pushed with the "new" flash.
	s.dash.add(querylog.Event{Target: "db", Kind: "query", SQL: "SELECT boom", Error: "boom", Time: time.Now()})
	if _, _, data := next(); !strings.Contains(data, "boom") || !strings.Contains(data, " new") {
		t.Fatalf("live message: %s", data)
	}
}

func TestStreamStaleLastEventID(t *testing.T) {
	_, ts := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream", nil)
	req.Header.Set("Last-Event-ID", "oldepoch-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "event: reset\n" {
		t.Fatalf("got %q, want a reset event", line)
	}
}

func TestClearRequiresSameOrigin(t *testing.T) {
	s, ts := testServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/clear", nil)
	req.Header.Set("Origin", "http://evil.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(s.dash.after(0)) == 0 {
		t.Fatalf("cross-origin clear: %d, events left %d", resp.StatusCode, len(s.dash.after(0)))
	}
	req, _ = http.NewRequest("POST", ts.URL+"/clear", nil)
	req.Header.Set("Origin", ts.URL)
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(s.dash.after(0)) != 0 {
		t.Fatalf("clear: %d, events left %d", resp.StatusCode, len(s.dash.after(0)))
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
	for origin, want := range map[string]string{
		"http://evil.com":       "cross-origin",
		"http://localhost:7878": `<span id="msg" class="err">`, // allowed, but the file doesn't exist
	} {
		req := httptest.NewRequest("POST", "/config/open", nil)
		req.Host = "localhost:7878"
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.handleOpenConfig(rec, req)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("origin %q: got %d %q, want %q", origin, rec.Code, rec.Body.String(), want)
		}
	}
}
