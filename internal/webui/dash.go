package webui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
)

const (
	maxEvents  = 5000
	maxDurs    = 500
	sparkSecs  = 60
	subBuffer  = 1024
	maxRows    = 800
	slowMillis = 100
)

// Event is a recorded event with a dashboard-wide sequence ID.
type Event struct {
	querylog.Event
	ID      uint64
	SysText string // human text for non-query events
}

type targetStats struct {
	count, errors int
	durs          []float64 // last maxDurs query durations
	conns         map[string]struct{}
	buckets       map[int64]int // events per unix second
	calls, tokens int
}

// dash aggregates events from every target: the last maxEvents events plus
// running per-target stats. Subscribers get new events as they arrive.
type dash struct {
	epoch string // changes when the ui process restarts: stale SSE ids are detected

	mu      sync.Mutex
	nextID  uint64
	events  []*Event
	targets map[string]*targetStats
	subs    map[chan *Event]struct{}
}

func newDash() *dash {
	return &dash{
		epoch:   strconv.FormatInt(time.Now().UnixNano(), 36),
		nextID:  1,
		targets: make(map[string]*targetStats),
		subs:    make(map[chan *Event]struct{}),
	}
}

func (d *dash) stats(name string) *targetStats {
	t := d.targets[name]
	if t == nil {
		t = &targetStats{conns: map[string]struct{}{}, buckets: map[int64]int{}}
		d.targets[name] = t
	}
	return t
}

func (d *dash) add(qe querylog.Event) {
	ev := &Event{Event: qe}
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.stats(ev.Target)
	key := fmt.Sprintf("%d:%d", ev.PID, ev.Conn)
	switch ev.Kind {
	case querylog.KindConnect:
		t.conns[key] = struct{}{}
		ev.SysText = fmt.Sprintf("connection #%d opened · db=%s user=%s", ev.Conn, ev.Database, ev.User)
		if ev.App != "" {
			ev.SysText += " app=" + ev.App
		}
	case querylog.KindDisconnect:
		delete(t.conns, key)
		ev.SysText = fmt.Sprintf("connection #%d closed", ev.Conn)
	case querylog.KindStart, querylog.KindStop:
		prefix := strconv.Itoa(ev.PID) + ":"
		for k := range t.conns {
			if strings.HasPrefix(k, prefix) {
				delete(t.conns, k)
			}
		}
		verb := "started"
		if ev.Kind == querylog.KindStop {
			verb = "stopped"
		}
		ev.SysText = fmt.Sprintf("tunnel %s (pid %d)", verb, ev.PID)
	case querylog.KindOpaque:
		ev.SysText = fmt.Sprintf("connection #%d switched to TLS: queries not visible", ev.Conn)
	case querylog.KindTool:
		t.calls++
		t.tokens += ev.Tokens + ev.TokensIn
	case querylog.KindQuery, querylog.KindError:
		t.count++
		if ev.Error != "" {
			t.errors++
		}
		if ev.Kind == querylog.KindQuery {
			t.durs = append(t.durs, ev.DurationMS)
			if len(t.durs) > maxDurs {
				t.durs = t.durs[len(t.durs)-maxDurs:]
			}
		} else {
			ev.SysText = "server error"
		}
		sec := ev.Time.Unix()
		t.buckets[sec]++
		for s := range t.buckets {
			if s < sec-2*sparkSecs {
				delete(t.buckets, s)
			}
		}
	}

	ev.ID = d.nextID
	d.nextID++
	d.events = append(d.events, ev)
	if len(d.events) > maxEvents {
		d.events = append(d.events[:0:0], d.events[len(d.events)-maxEvents:]...)
	}
	for ch := range d.subs {
		select {
		case ch <- ev:
		default:
			// Too slow: drop it; the browser reconnects with Last-Event-ID
			// and catches up from the buffer.
			close(ch)
			delete(d.subs, ch)
		}
	}
}

// sortByTime orders the initial backlog by timestamp (files are read one
// after the other) and renumbers it.
func (d *dash) sortByTime() {
	d.mu.Lock()
	defer d.mu.Unlock()
	sort.SliceStable(d.events, func(i, j int) bool { return d.events[i].Time.Before(d.events[j].Time) })
	for i, ev := range d.events {
		ev.ID = uint64(i + 1)
	}
	d.nextID = uint64(len(d.events) + 1)
}

func (d *dash) clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = nil
	for _, t := range d.targets {
		conns := t.conns
		*t = targetStats{conns: conns, buckets: map[int64]int{}}
	}
}

func (d *dash) subscribe() chan *Event {
	ch := make(chan *Event, subBuffer)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *dash) unsubscribe(ch chan *Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.subs[ch]; ok {
		delete(d.subs, ch)
		close(ch)
	}
}

// after returns the buffered events with ID > id, oldest first.
func (d *dash) after(id uint64) []*Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := sort.Search(len(d.events), func(i int) bool { return d.events[i].ID > id })
	return append([]*Event(nil), d.events[i:]...)
}

func (d *dash) lastID() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nextID - 1
}

func (d *dash) get(id uint64) *Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := sort.Search(len(d.events), func(i int) bool { return d.events[i].ID >= id })
	if i < len(d.events) && d.events[i].ID == id {
		return d.events[i]
	}
	return nil
}

// sseID encodes an event ID for the SSE id: field; parseSSEID rejects IDs
// from a previous ui process.
func (d *dash) sseID(id uint64) string { return d.epoch + "-" + strconv.FormatUint(id, 10) }

func (d *dash) parseSSEID(s string) (uint64, bool) {
	epoch, n, ok := strings.Cut(s, "-")
	if !ok || epoch != d.epoch {
		return 0, false
	}
	id, err := strconv.ParseUint(n, 10, 64)
	return id, err == nil
}
