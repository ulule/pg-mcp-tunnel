package querylog

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// --- wire builders ---

func msg(typ byte, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := []byte{typ, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)+4))
	return append(out, b...)
}

func cstr(s string) []byte { return append([]byte(s), 0) }

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

func untyped(body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	return append(u32(uint32(len(b)+4)), b...)
}

func startup(kv ...string) []byte {
	parts := [][]byte{u32(protocolV3)}
	for _, s := range kv {
		parts = append(parts, cstr(s))
	}
	parts = append(parts, []byte{0})
	return untyped(parts...)
}

func query(sql string) []byte { return msg('Q', cstr(sql)) }
func parse(name, sql string) []byte {
	return msg('P', cstr(name), cstr(sql), []byte{0, 0})
}
func bind(portal, stmt string) []byte {
	return msg('B', cstr(portal), cstr(stmt), []byte{0, 0, 0, 0, 0, 0})
}
func execute() []byte            { return msg('E', cstr(""), u32(0)) }
func syncMsg() []byte            { return msg('S') }
func complete(tag string) []byte { return msg('C', cstr(tag)) }
func ready() []byte              { return msg('Z', []byte{'I'}) }
func dataRow() []byte            { return msg('D', []byte{0, 1}, u32(1), []byte("x")) }
func pgError(code, m string) []byte {
	return msg('E', []byte{'S'}, cstr("ERROR"), []byte{'C'}, cstr(code), []byte{'M'}, cstr(m), []byte{0})
}

// --- harness ---

type harness struct {
	t      *testing.T
	s      *Sniffer
	events []Event
	clock  time.Time
	chunk  int
}

func newHarness(t *testing.T, chunk int) *harness {
	h := &harness{t: t, clock: time.Unix(1_700_000_000, 0), chunk: chunk}
	h.s = newSniffer(1, func(e Event) { h.events = append(h.events, e) }, func() time.Time { return h.clock })
	return h
}

func (h *harness) write(w func(p []byte), data ...[]byte) {
	b := bytes.Join(data, nil)
	for len(b) > 0 {
		n := min(h.chunk, len(b))
		w(b[:n])
		b = b[n:]
	}
}

func (h *harness) client(data ...[]byte) { h.write(h.s.feedFront, data...) }
func (h *harness) server(data ...[]byte) { h.write(h.s.feedBack, data...) }

func (h *harness) queries() []Event {
	var out []Event
	for _, e := range h.events {
		if e.Kind == KindQuery {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) handshake() {
	h.client(startup("user", "alice", "database", "shop", "application_name", "postgres-mcp"))
	h.server(msg('R', u32(0)), msg('S', cstr("server_version"), cstr("16.2")), msg('K', u32(42), u32(7)), ready())
}

// Feed everything byte-by-byte as well as in large chunks: framing must not
// depend on how TCP reads are split.
var chunkSizes = []int{1, 3, 7, 1 << 20}

func TestSimpleQuery(t *testing.T) {
	for _, chunk := range chunkSizes {
		h := newHarness(t, chunk)
		h.handshake()

		h.client(query("SELECT * FROM users"))
		h.clock = h.clock.Add(12500 * time.Microsecond)
		h.server(msg('T', []byte{0, 0}), dataRow(), dataRow(), complete("SELECT 2"), ready())

		if len(h.events) == 0 || h.events[0].Kind != KindConnect {
			t.Fatalf("chunk=%d: want connect first, got %+v", chunk, h.events)
		}
		if c := h.events[0]; c.Database != "shop" || c.User != "alice" || c.App != "postgres-mcp" {
			t.Fatalf("chunk=%d: connect event = %+v", chunk, c)
		}
		q := h.queries()
		if len(q) != 1 {
			t.Fatalf("chunk=%d: want 1 query, got %+v", chunk, q)
		}
		if q[0].SQL != "SELECT * FROM users" || q[0].Tag != "SELECT 2" || q[0].Rows != 2 || q[0].DurationMS != 12.5 || q[0].Database != "shop" {
			t.Fatalf("chunk=%d: query event = %+v", chunk, q[0])
		}
	}
}

func TestExtendedProtocolWithPreparedStatementReuse(t *testing.T) {
	for _, chunk := range chunkSizes {
		h := newHarness(t, chunk)
		h.handshake()

		// Unnamed statement.
		h.client(parse("", "SELECT $1::int"), bind("", ""), msg('D', []byte{'P'}, cstr("")), execute(), syncMsg())
		h.server(msg('1'), msg('2'), msg('T', []byte{0, 0}), dataRow(), complete("SELECT 1"), ready())

		// Named statement prepared once, then executed by name only (psycopg's
		// auto-prepare after prepare_threshold executions).
		h.client(parse("_pg3_0", "UPDATE t SET x = $1"), bind("", "_pg3_0"), execute(), syncMsg())
		h.server(msg('1'), msg('2'), complete("UPDATE 3"), ready())
		h.client(bind("", "_pg3_0"), execute(), syncMsg())
		h.server(msg('2'), complete("UPDATE 4"), ready())

		q := h.queries()
		if len(q) != 3 {
			t.Fatalf("chunk=%d: want 3 queries, got %+v", chunk, q)
		}
		want := []struct {
			sql  string
			rows int64
		}{{"SELECT $1::int", 1}, {"UPDATE t SET x = $1", 3}, {"UPDATE t SET x = $1", 4}}
		for i, w := range want {
			if q[i].SQL != w.sql || q[i].Rows != w.rows {
				t.Fatalf("chunk=%d: query %d = %+v, want %+v", chunk, i, q[i], w)
			}
		}
	}
}

func TestErrorIsAttachedToQuery(t *testing.T) {
	h := newHarness(t, 5)
	h.handshake()
	h.client(parse("", "SELECT nope"), bind("", ""), execute(), syncMsg())
	h.server(pgError("42703", `column "nope" does not exist`), ready())

	q := h.queries()
	if len(q) != 1 || q[0].Error != `column "nope" does not exist` || q[0].SQLState != "42703" {
		t.Fatalf("got %+v", q)
	}
}

func TestPipelinedQueriesMatchInOrder(t *testing.T) {
	h := newHarness(t, 4)
	h.handshake()
	h.client(query("BEGIN"), query("SELECT 1"), query("COMMIT"))
	h.server(complete("BEGIN"), ready(), complete("SELECT 1"), ready(), complete("COMMIT"), ready())

	q := h.queries()
	if len(q) != 3 || q[0].SQL != "BEGIN" || q[1].SQL != "SELECT 1" || q[2].SQL != "COMMIT" {
		t.Fatalf("got %+v", q)
	}
}

func TestSSLDeclinedThenPlaintext(t *testing.T) {
	h := newHarness(t, 2)
	h.client(untyped(u32(sslRequestCode)))
	h.server([]byte{'N'})
	h.handshake()
	h.client(query("SELECT 1"))
	h.server(complete("SELECT 1"), ready())

	if q := h.queries(); len(q) != 1 || q[0].SQL != "SELECT 1" {
		t.Fatalf("got %+v", h.events)
	}
}

func TestSSLAcceptedGoesOpaque(t *testing.T) {
	h := newHarness(t, 1)
	h.client(untyped(u32(sslRequestCode)))
	h.server([]byte{'S'})
	// TLS records: garbage from the protocol's point of view.
	h.client([]byte{0x16, 0x03, 0x01, 0x02, 0x00, 0xff, 0xff})
	h.server([]byte{0x16, 0x03, 0x03, 0x00, 0x7a})

	if len(h.events) != 1 || h.events[0].Kind != KindOpaque {
		t.Fatalf("got %+v", h.events)
	}
}

func TestGarbageDisablesSnifferWithoutPanicking(t *testing.T) {
	h := newHarness(t, 1)
	h.client([]byte{0, 0, 0, 1, 2, 3, 4, 5, 6})
	h.client(query("SELECT 1"))
	h.server(complete("SELECT 1"), ready())
	if len(h.events) != 0 {
		t.Fatalf("got %+v", h.events)
	}
}

func TestLargeQueryIsTruncatedAndLargeRowsAreSkipped(t *testing.T) {
	h := newHarness(t, 1<<20)
	h.handshake()
	big := "SELECT '" + string(bytes.Repeat([]byte("a"), maxBufferedBody*2)) + "'"
	h.client(query(big))
	h.server(msg('D', bytes.Repeat([]byte("z"), 5<<20)), complete("SELECT 1"), ready())

	q := h.queries()
	if len(q) != 1 || len(q[0].SQL) > maxBufferedBody+32 || !bytes.HasSuffix([]byte(q[0].SQL), []byte("[truncated]")) {
		t.Fatalf("got %d queries, sql len %d", len(q), len(q[0].SQL))
	}
}

func TestCloseFlushesInflightAndEmitsDisconnect(t *testing.T) {
	h := newHarness(t, 1<<20)
	h.handshake()
	h.client(query("SELECT pg_sleep(100)"))
	h.s.Close()
	h.s.Close() // idempotent

	q := h.queries()
	if len(q) != 1 || q[0].Error == "" {
		t.Fatalf("want inflight query flushed with error, got %+v", q)
	}
	last := h.events[len(h.events)-1]
	if last.Kind != KindDisconnect || last.Database != "shop" {
		t.Fatalf("want disconnect last, got %+v", last)
	}
	n := len(h.events)
	h.server(complete("SELECT 1"), ready()) // late bytes after Close are ignored
	if len(h.events) != n {
		t.Fatalf("events after close: %+v", h.events[n:])
	}
}

func TestPasswordMessageIsNeverBuffered(t *testing.T) {
	if frontendKeep('p') {
		t.Fatal("PasswordMessage must not be buffered")
	}
}
