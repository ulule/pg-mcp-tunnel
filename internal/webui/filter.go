package webui

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

// Filter is the dashboard view state. It lives in the URL query, so the
// server keeps no per-client state and a view survives reloads.
type Filter struct {
	Target string // "" = all databases
	Q      string // lowercased substring of SQL, error, tag or tool
	Only   string // all, err, slow
	Noise  bool   // hide BEGIN/COMMIT/SET...
	Sys    bool   // show connection events
	Tools  bool   // show MCP tool calls
	Tab    string // live, top
	Sort   string // top queries: total, count, avg, max, tokens
	Paused bool
}

func parseFilter(r *http.Request) Filter {
	_ = r.ParseForm()
	// Checkboxes come as a hidden "0" followed by the checkbox's "1": the
	// last value wins.
	last := func(k, def string) string {
		if v := r.Form[k]; len(v) > 0 {
			return v[len(v)-1]
		}
		return def
	}
	f := Filter{
		Target: last("target", ""),
		Q:      strings.ToLower(strings.TrimSpace(last("q", ""))),
		Only:   last("only", "all"),
		Noise:  last("noise", "1") == "1",
		Sys:    last("sys", "0") == "1",
		Tools:  last("tools", "1") == "1",
		Tab:    last("tab", "live"),
		Sort:   last("sort", "total"),
		Paused: last("paused", "0") == "1",
	}
	switch f.Only {
	case "all", "err", "slow":
	default:
		f.Only = "all"
	}
	if f.Tab != "top" {
		f.Tab = "live"
	}
	switch f.Sort {
	case "total", "count", "avg", "max", "tokens":
	default:
		f.Sort = "total"
	}
	return f
}

// Query encodes the filter; defaults are omitted to keep URLs short.
func (f Filter) Query() string {
	v := url.Values{}
	set := func(k, val, def string) {
		if val != def {
			v.Set(k, val)
		}
	}
	b := func(x bool) string {
		if x {
			return "1"
		}
		return "0"
	}
	set("target", f.Target, "")
	set("q", f.Q, "")
	set("only", f.Only, "all")
	set("noise", b(f.Noise), "1")
	set("sys", b(f.Sys), "0")
	set("tools", b(f.Tools), "1")
	set("tab", f.Tab, "live")
	set("sort", f.Sort, "total")
	set("paused", b(f.Paused), "0")
	return v.Encode()
}

var noiseRe = regexp.MustCompile(`(?i)^\s*(BEGIN|START\s+TRANSACTION|COMMIT|END|ROLLBACK|SET|RESET|SHOW|DISCARD|DEALLOCATE|SAVEPOINT|RELEASE)\b`)

func isQueryKind(kind string) bool {
	return kind == querylog.KindQuery || kind == querylog.KindError || kind == querylog.KindTool
}

// Matches reports whether ev is shown under f.
func (f Filter) Matches(ev *Event) bool {
	if f.Target != "" && ev.Target != f.Target {
		return false
	}
	isTool := ev.Kind == querylog.KindTool
	if isTool && !f.Tools {
		return false
	}
	if !isQueryKind(ev.Kind) {
		return f.Sys && f.Q == "" && f.Only == "all"
	}
	if f.Only == "err" && ev.Error == "" {
		return false
	}
	if f.Only == "slow" && ev.DurationMS < 100 {
		return false
	}
	if f.Noise && !isTool && ev.Error == "" && ev.SQL != "" && noiseRe.MatchString(ev.SQL) {
		return false
	}
	if f.Q != "" {
		hay := strings.ToLower(ev.SQL + " " + ev.Error + " " + ev.Tag + " " + ev.Tool)
		if !strings.Contains(hay, f.Q) {
			return false
		}
	}
	return true
}

var (
	spaceRe          = regexp.MustCompile(`\s+`)
	leadingCommentRe = regexp.MustCompile(`^(?:/\*[\s\S]*?\*/\s*|--[^\n]*\n\s*)+`)
	fpStringRe       = regexp.MustCompile(`'(?:[^']|'')*'`)
	fpParamRe        = regexp.MustCompile(`\$\d+`)
	fpNumberRe       = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	fpListRe         = regexp.MustCompile(`\(\s*\?(?:\s*,\s*\?)+\s*\)`)
	fpInRe           = regexp.MustCompile(`(?i)\bIN\s*\(\?\)`)
)

// preview is the one-line form of sql without leading comments.
func preview(sql string) string {
	return leadingCommentRe.ReplaceAllString(strings.TrimSpace(spaceRe.ReplaceAllString(sql, " ")), "")
}

// fingerprint normalizes literals so similar queries group together.
func fingerprint(sql string) string {
	s := preview(sql)
	s = fpStringRe.ReplaceAllString(s, "?")
	s = fpParamRe.ReplaceAllString(s, "?")
	s = fpNumberRe.ReplaceAllString(s, "?")
	s = fpListRe.ReplaceAllString(s, "(?, …)")
	return fpInRe.ReplaceAllString(s, "IN (?, …)")
}
