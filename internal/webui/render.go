package webui

import (
	"embed"
	"fmt"
	"html"
	"html/template"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

//go:embed templates.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"color":     func(name string) template.CSS { return template.CSS(colorFor(name)) },
	"split":     func(s string) []string { return strings.Split(s, "|") },
	"withSince": withSince,
	"fmtTime":   fmtTime,
	"fmtMs":     fmtMs,
	"durClass":  durClass,
	"durBar":    durBar,
	"fmtTok":    fmtTok,
	"tokClass":  tokClass,
	"highlight": highlight,
	"preview":   preview,
	"rowsLabel": rowsLabel,
	"tokens":    func(ev *Event) int { return ev.Tokens + ev.TokensIn },
	"isTool":    func(ev *Event) bool { return ev.Kind == querylog.KindTool },
	"isSys":     func(ev *Event) bool { return ev.SysText != "" && ev.Kind != querylog.KindError },
	"fullTime":  func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	"pct":       func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
}).ParseFS(templatesFS, "templates.html"))

// withSince builds path?<filter query>&since=<id>.
func withSince(path, query string, since uint64) string {
	if query != "" {
		query += "&"
	}
	return path + "?" + query + "since=" + strconv.FormatUint(since, 10)
}

var palette = []string{"#3b82f6", "#10b981", "#f59e0b", "#ef4444", "#8b5cf6", "#ec4899", "#14b8a6", "#f97316", "#6366f1", "#84cc16"}

// colorFor gives each target a stable color.
func colorFor(name string) string {
	var h uint32
	for _, c := range name {
		h = h*31 + uint32(c)
	}
	return palette[h%uint32(len(palette))]
}

func fmtTime(t time.Time) string { return t.Local().Format("15:04:05.000") }

func fmtMs(ms float64) string {
	switch {
	case ms < 1:
		return strconv.FormatFloat(ms, 'f', 2, 64) + "ms"
	case ms < 100:
		return strconv.FormatFloat(ms, 'f', 1, 64) + "ms"
	case ms < 10000:
		return strconv.Itoa(int(math.Round(ms))) + "ms"
	default:
		return strconv.FormatFloat(ms/1000, 'f', 1, 64) + "s"
	}
}

func durClass(ms float64) string {
	switch {
	case ms < 10:
		return "d0"
	case ms < 100:
		return "d1"
	case ms < 1000:
		return "d2"
	default:
		return "d3"
	}
}

// durBar is the width in px of the log-scaled duration bar.
func durBar(ms float64) int {
	w := (math.Log10(math.Max(ms, 0.1)) + 1) * 15
	return int(math.Max(2, math.Min(78, w)))
}

func fmtTok(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	case n < 1e6:
		return strconv.Itoa(int(math.Round(float64(n)/1000))) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	}
}

func tokClass(n int) string {
	if n >= 10000 {
		return "tok big"
	}
	return "tok"
}

var rowsTagRe = regexp.MustCompile(`^(SELECT|INSERT|UPDATE|DELETE|MERGE|COPY|FETCH|MOVE)\b`)

// rowsLabel is the rows/tag column of a query row.
func rowsLabel(ev *Event) string {
	switch {
	case ev.Error != "":
		if ev.SQLState != "" {
			return ev.SQLState
		}
		return "error"
	case ev.Tag == "":
		return ""
	case ev.Rows != 0 && !rowsTagRe.MatchString(ev.Tag):
		return strconv.FormatInt(ev.Rows, 10) + " rows"
	default:
		return ev.Tag
	}
}

const sqlKeywords = "SELECT|FROM|WHERE|AND|OR|NOT|IN|IS|NULL|AS|ON|JOIN|LEFT|RIGHT|INNER|OUTER|FULL|CROSS|LATERAL|GROUP|BY|ORDER|HAVING|LIMIT|OFFSET|INSERT|INTO|VALUES|UPDATE|SET|DELETE|RETURNING|WITH|RECURSIVE|UNION|ALL|DISTINCT|CASE|WHEN|THEN|ELSE|END|EXISTS|BETWEEN|LIKE|ILIKE|ASC|DESC|CREATE|ALTER|DROP|TABLE|INDEX|VIEW|BEGIN|COMMIT|ROLLBACK|TRANSACTION|READ|ONLY|EXPLAIN|ANALYZE|VERBOSE|BUFFERS|FORMAT|COUNT|SUM|AVG|MIN|MAX|COALESCE|CAST|OVER|PARTITION|FILTER|TRUE|FALSE|INTERVAL|NOW|ANY|SHOW|USING|NULLS|FIRST|LAST"

var sqlTokenRe = regexp.MustCompile(`(?i)(--[^\n]*|/\*[\s\S]*?\*/)|('(?:[^']|'')*'?)|(\$\d+|\b\d+(?:\.\d+)?\b)|\b(` + sqlKeywords + `)\b`)

// highlight escapes sql and wraps comments, strings, numbers and keywords
// in spans.
func highlight(sql string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range sqlTokenRe.FindAllStringSubmatchIndex(sql, -1) {
		b.WriteString(html.EscapeString(sql[last:m[0]]))
		cls := "k"
		switch {
		case m[2] >= 0:
			cls = "c"
		case m[4] >= 0:
			cls = "s"
		case m[6] >= 0:
			cls = "n"
		}
		b.WriteString(`<span class="` + cls + `">` + html.EscapeString(sql[m[0]:m[1]]) + `</span>`)
		last = m[1]
	}
	b.WriteString(html.EscapeString(sql[last:]))
	return template.HTML(b.String())
}

func p95(durs []float64) (float64, bool) {
	if len(durs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), durs...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(float64(len(s))*0.95))], true
}

// ---------- view models ----------

type rowData struct {
	Ev       *Event
	Expanded bool
	Fresh    bool
}

type liveData struct {
	Filter
	Query  string
	LastID uint64
	Rows   []rowData
	Empty  string
}

type topGroup struct {
	FP         string
	Count      int
	Errors     int
	Total, Max float64
	Rows       int64
	Targets    []string
	Tokens     int
	Share      float64
}

func (g topGroup) Avg() float64 { return g.Total / float64(g.Count) }

type topData struct {
	Filter
	Query  string
	Groups []topGroup
	Empty  string
}

type dbView struct {
	Name          string
	Color         template.CSS
	Active, Alive bool
	PIDs          string
	Count, Errors int
	Conns         int
	Avg, P95      string
	Calls, Tokens int
	Spark         template.HTML
}

type sidebarData struct {
	AllActive bool
	DBs       []dbView
	KPI       struct {
		QPS           string
		Total, Errors int
		P95           string
		Tokens        int
	}
}

const (
	emptyNoEvents = "no-events"
	emptyFiltered = "filtered"
)

func (d *dash) live(f Filter) liveData {
	d.mu.Lock()
	defer d.mu.Unlock()
	data := liveData{Filter: f, Query: f.Query(), LastID: d.nextID - 1}
	for i := len(d.events) - 1; i >= 0 && len(data.Rows) < maxRows; i-- {
		if f.Matches(d.events[i]) {
			data.Rows = append(data.Rows, rowData{Ev: d.events[i]})
		}
	}
	data.Empty = emptyState(len(d.events), len(data.Rows))
	return data
}

func emptyState(events, shown int) string {
	switch {
	case shown > 0:
		return ""
	case events == 0:
		return emptyNoEvents
	default:
		return emptyFiltered
	}
}

func (d *dash) pending(f Filter, since uint64) int {
	n := 0
	for _, ev := range d.after(since) {
		if f.Matches(ev) {
			n++
		}
	}
	return n
}

func (d *dash) top(f Filter) topData {
	d.mu.Lock()
	defer d.mu.Unlock()
	groups := map[string]*topGroup{}
	targets := map[string]map[string]bool{}
	tokens := map[string]int{}
	var grand float64
	for _, ev := range d.events {
		if !f.Matches(ev) || ev.SQL == "" {
			continue
		}
		if ev.Kind == querylog.KindTool {
			if ev.Tool == "execute_sql" {
				tokens[fingerprint(ev.SQL)] += ev.Tokens + ev.TokensIn
			}
			continue
		}
		if ev.Kind != querylog.KindQuery {
			continue
		}
		fp := fingerprint(ev.SQL)
		g := groups[fp]
		if g == nil {
			g = &topGroup{FP: fp}
			groups[fp] = g
			targets[fp] = map[string]bool{}
		}
		g.Count++
		g.Total += ev.DurationMS
		g.Max = math.Max(g.Max, ev.DurationMS)
		g.Rows += ev.Rows
		if ev.Error != "" {
			g.Errors++
		}
		targets[fp][ev.Target] = true
		grand += ev.DurationMS
	}
	list := make([]topGroup, 0, len(groups))
	for fp, g := range groups {
		g.Tokens = tokens[fp]
		for t := range targets[fp] {
			g.Targets = append(g.Targets, t)
		}
		sort.Strings(g.Targets)
		if grand > 0 {
			g.Share = 100 * g.Total / grand
		}
		list = append(list, *g)
	}
	key := func(g topGroup) float64 {
		switch f.Sort {
		case "count":
			return float64(g.Count)
		case "avg":
			return g.Avg()
		case "max":
			return g.Max
		case "tokens":
			return float64(g.Tokens)
		}
		return g.Total
	}
	sort.SliceStable(list, func(i, j int) bool {
		if a, b := key(list[i]), key(list[j]); a != b {
			return a > b
		}
		return list[i].FP < list[j].FP
	})
	if len(list) > 100 {
		list = list[:100]
	}
	return topData{Filter: f, Query: f.Query(), Groups: list, Empty: emptyState(len(d.events), len(list))}
}

// sidebar renders per-target stats; alive maps a target to its running
// tunnel pids.
func (d *dash) sidebar(f Filter, names []string, alive map[string][]int) sidebarData {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	all := map[string]bool{}
	for _, n := range names {
		all[n] = true
	}
	for n := range d.targets {
		all[n] = true
	}
	sorted := make([]string, 0, len(all))
	for n := range all {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	data := sidebarData{AllActive: f.Target == ""}
	var allDurs []float64
	recent := 0
	for _, name := range sorted {
		t := d.stats(name)
		pids := alive[name]
		alivePID := map[string]bool{}
		pidStrs := make([]string, len(pids))
		for i, p := range pids {
			pidStrs[i] = strconv.Itoa(p)
			alivePID[pidStrs[i]] = true
		}
		conns := 0
		for k := range t.conns {
			if pid, _, _ := strings.Cut(k, ":"); alivePID[pid] {
				conns++
			}
		}
		v := dbView{
			Name: name, Color: template.CSS(colorFor(name)), Active: f.Target == name, Alive: len(pids) > 0,
			PIDs: strings.Join(pidStrs, ", "), Count: t.count, Errors: t.errors, Conns: conns,
			Avg: "–", P95: "–", Calls: t.calls, Tokens: t.tokens, Spark: spark(t, now, colorFor(name)),
		}
		if len(t.durs) > 0 {
			var sum float64
			for _, x := range t.durs {
				sum += x
			}
			v.Avg = fmtMs(sum / float64(len(t.durs)))
			p, _ := p95(t.durs)
			v.P95 = fmtMs(p)
		}
		data.DBs = append(data.DBs, v)
		data.KPI.Total += t.count
		data.KPI.Errors += t.errors
		data.KPI.Tokens += t.tokens
		allDurs = append(allDurs, t.durs...)
		for s := now - 9; s <= now; s++ {
			recent += t.buckets[s]
		}
	}
	data.KPI.QPS = strconv.FormatFloat(float64(recent)/10, 'f', 1, 64)
	data.KPI.P95 = "–"
	if p, ok := p95(allDurs); ok {
		data.KPI.P95 = fmtMs(p)
	}
	return data
}

// spark draws the last sparkSecs seconds of activity as SVG bars.
func spark(t *targetStats, now int64, color string) template.HTML {
	const w, h = 240.0, 22.0
	vals := make([]int, sparkSecs)
	maxV := 1
	for i := range vals {
		vals[i] = t.buckets[now-sparkSecs+1+int64(i)]
		maxV = max(maxV, vals[i])
	}
	bw := w / sparkSecs
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %g %g" preserveAspectRatio="none" fill="%s"><rect x="0" y="%g" width="%g" height="1" fill="var(--border)"/>`, w, h, color, h-1, w)
	for i, v := range vals {
		if v == 0 {
			continue
		}
		bh := math.Max(2, float64(v)/float64(maxV)*h)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="1"/>`, float64(i)*bw, h-bh, bw-1, bh)
	}
	b.WriteString("</svg>")
	return template.HTML(b.String())
}
