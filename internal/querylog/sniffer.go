package querylog

import (
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Wire-protocol constants (https://www.postgresql.org/docs/current/protocol-message-formats.html).
const (
	protocolV3     = 196608
	sslRequestCode = 80877103
	gssRequestCode = 80877104

	// Bodies of interesting messages are buffered up to this size; longer SQL
	// is truncated in the event. Uninteresting messages (DataRow, CopyData…)
	// are skipped without buffering.
	maxBufferedBody = 64 * 1024
	// Anything claiming to be larger than this is not Postgres protocol.
	maxMessageLen = 1 << 30
)

// Sniffer parses both directions of one Postgres connection and emits events.
// It implements tcpproxy.Tap. It never fails the proxied connection: on any
// parse problem (or once TLS starts) it silently goes dark.
type Sniffer struct {
	mu   sync.Mutex
	emit func(Event)
	now  func() time.Time
	conn uint64

	front, back msgReader
	dead        bool
	closed      bool

	// Backend replies to SSLRequest/GSSENCRequest with a single untyped byte.
	pendingNegotiation int

	db, user, app string

	stmts      map[string]string // prepared statement name -> SQL
	batch      []string          // statements bound since the last Sync
	batchStart time.Time
	inflight   []*roundTrip // FIFO, one per Query/Sync, popped by ReadyForQuery
}

type roundTrip struct {
	sqls  []string
	start time.Time
	tags  []string
	rows  int64
	err   string
	state string
}

func newSniffer(conn uint64, emit func(Event), now func() time.Time) *Sniffer {
	s := &Sniffer{
		emit:  emit,
		now:   now,
		conn:  conn,
		stmts: make(map[string]string),
	}
	s.front = msgReader{untyped: true, keep: frontendKeep, onMsg: s.onFrontend}
	s.back = msgReader{keep: backendKeep, onMsg: s.onBackend}
	return s
}

func (s *Sniffer) ClientToServer() io.Writer { return writerFunc(s.feedFront) }
func (s *Sniffer) ServerToClient() io.Writer { return writerFunc(s.feedBack) }

func (s *Sniffer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, rt := range s.inflight {
		if rt.err == "" {
			rt.err = "connection closed before completion"
		}
		s.emitRoundTrip(rt)
	}
	s.inflight = nil
	s.emit(Event{Kind: KindDisconnect, Conn: s.conn, Database: s.db, User: s.user})
}

type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}

func (s *Sniffer) feedFront(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead || s.closed {
		return
	}
	if !s.front.feed(p) {
		s.dead = true
	}
}

func (s *Sniffer) feedBack(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead || s.closed {
		return
	}
	for s.pendingNegotiation > 0 && len(p) > 0 {
		switch p[0] {
		case 'N': // server declined SSL/GSS: plaintext startup follows
			s.pendingNegotiation--
			p = p[1:]
		case 'S', 'G': // encrypted from here on
			s.goOpaque()
			return
		default: // e.g. an ErrorResponse from a pre-SSL-aware server
			s.pendingNegotiation = 0
		}
	}
	if !s.back.feed(p) {
		s.dead = true
	}
}

func (s *Sniffer) goOpaque() {
	s.dead = true
	s.emit(Event{Kind: KindOpaque, Conn: s.conn})
}

func frontendKeep(typ byte) bool { return typ == 0 || typ == 'Q' || typ == 'P' || typ == 'B' }
func backendKeep(typ byte) bool  { return typ == 'C' || typ == 'E' || typ == 'Z' }

func (s *Sniffer) onFrontend(typ byte, body []byte, truncated bool) bool {
	switch typ {
	case 0: // untyped startup-phase message
		if len(body) < 4 {
			return false
		}
		switch code := binary.BigEndian.Uint32(body); code {
		case sslRequestCode, gssRequestCode:
			s.pendingNegotiation++
		case protocolV3:
			s.front.untyped = false
			s.parseStartup(body[4:])
		default: // CancelRequest or an unknown protocol: nothing to observe
			return false
		}
	case 'Q':
		sql, _ := cstring(body)
		s.inflight = append(s.inflight, &roundTrip{sqls: []string{markTruncated(sql, truncated)}, start: s.now()})
	case 'P':
		name, rest := cstring(body)
		sql, _ := cstring(rest)
		s.stmts[name] = markTruncated(sql, truncated)
		s.startBatch()
	case 'B':
		_, rest := cstring(body) // portal
		stmt, _ := cstring(rest)
		sql, ok := s.stmts[stmt]
		if !ok {
			sql = "-- unknown prepared statement " + strconv.Quote(stmt)
		}
		s.startBatch()
		s.batch = append(s.batch, sql)
	case 'C': // Close: forget a prepared statement
		if len(body) > 1 && body[0] == 'S' {
			name, _ := cstring(body[1:])
			delete(s.stmts, name)
		}
	case 'S': // Sync ends an extended-protocol batch
		s.inflight = append(s.inflight, &roundTrip{sqls: s.batch, start: s.batchStart})
		s.batch = nil
		s.batchStart = time.Time{}
	}
	return true
}

func (s *Sniffer) startBatch() {
	if s.batchStart.IsZero() {
		s.batchStart = s.now()
	}
}

func (s *Sniffer) parseStartup(body []byte) {
	for len(body) > 0 && body[0] != 0 {
		var k, v string
		k, body = cstring(body)
		v, body = cstring(body)
		switch k {
		case "database":
			s.db = v
		case "user":
			s.user = v
		case "application_name":
			s.app = v
		}
	}
	if s.db == "" {
		s.db = s.user
	}
	s.emit(Event{Kind: KindConnect, Conn: s.conn, Database: s.db, User: s.user, App: s.app})
}

func (s *Sniffer) onBackend(typ byte, body []byte, _ bool) bool {
	var head *roundTrip
	if len(s.inflight) > 0 {
		head = s.inflight[0]
	}
	switch typ {
	case 'C':
		if head != nil {
			tag, _ := cstring(body)
			head.tags = append(head.tags, tag)
			head.rows += tagRows(tag)
		}
	case 'E':
		msg, code := parseError(body)
		if head != nil {
			head.err, head.state = msg, code
		} else {
			// Error outside any query, e.g. authentication failure.
			s.emit(Event{Kind: KindError, Conn: s.conn, Database: s.db, User: s.user, Error: msg, SQLState: code})
		}
	case 'Z':
		if head != nil {
			s.inflight = s.inflight[1:]
			s.emitRoundTrip(head)
		}
	}
	return true
}

func (s *Sniffer) emitRoundTrip(rt *roundTrip) {
	if len(rt.sqls) == 0 {
		return // e.g. Parse-only + Sync, nothing executed
	}
	s.emit(Event{
		Kind:       KindQuery,
		Conn:       s.conn,
		Database:   s.db,
		User:       s.user,
		SQL:        strings.Join(rt.sqls, ";\n"),
		DurationMS: float64(s.now().Sub(rt.start).Microseconds()) / 1000,
		Tag:        strings.Join(rt.tags, ", "),
		Rows:       rt.rows,
		Error:      rt.err,
		SQLState:   rt.state,
	})
}

// msgReader incrementally frames a stream of Postgres messages. Header bytes
// and selected bodies are buffered; other bodies are skipped.
type msgReader struct {
	untyped bool // startup phase: messages have no type byte
	keep    func(typ byte) bool
	onMsg   func(typ byte, body []byte, truncated bool) bool

	hdr       [5]byte
	hdrLen    int
	inBody    bool
	typ       byte
	remain    int
	buffering bool
	body      []byte
	truncated bool
}

// feed consumes p and reports false if the stream is not valid protocol.
func (r *msgReader) feed(p []byte) bool {
	for len(p) > 0 {
		if r.inBody {
			n := min(r.remain, len(p))
			if r.buffering {
				take := min(n, maxBufferedBody-len(r.body))
				r.body = append(r.body, p[:take]...)
				if take < n {
					r.truncated = true
				}
			}
			r.remain -= n
			p = p[n:]
			if r.remain == 0 {
				if !r.finish() {
					return false
				}
			}
			continue
		}

		hdrSize := 5
		if r.untyped {
			hdrSize = 4
		}
		n := copy(r.hdr[r.hdrLen:hdrSize], p)
		r.hdrLen += n
		p = p[n:]
		if r.hdrLen < hdrSize {
			return true
		}
		r.hdrLen = 0

		var length uint32
		if r.untyped {
			r.typ = 0
			length = binary.BigEndian.Uint32(r.hdr[0:4])
		} else {
			r.typ = r.hdr[0]
			length = binary.BigEndian.Uint32(r.hdr[1:5])
		}
		if length < 4 || length > maxMessageLen {
			return false
		}
		r.remain = int(length) - 4
		r.buffering = r.keep(r.typ)
		r.inBody = true
		if r.remain == 0 && !r.finish() {
			return false
		}
	}
	return true
}

func (r *msgReader) finish() bool {
	r.inBody = false
	ok := r.onMsg(r.typ, r.body, r.truncated)
	r.body = r.body[:0]
	r.truncated = false
	return ok
}

// cstring splits a NUL-terminated string off the front of b.
func cstring(b []byte) (string, []byte) {
	head, rest, _ := bytes.Cut(b, []byte{0})
	return string(head), rest
}

func markTruncated(sql string, truncated bool) string {
	if truncated {
		return sql + " …[truncated]"
	}
	return sql
}

// parseError extracts the message and SQLSTATE from an ErrorResponse body.
func parseError(body []byte) (msg, code string) {
	for len(body) > 0 && body[0] != 0 {
		field := body[0]
		var v string
		v, body = cstring(body[1:])
		switch field {
		case 'M':
			msg = v
		case 'C':
			code = v
		}
	}
	return msg, code
}

// tagRows returns the row count of a CommandComplete tag such as "SELECT 5"
// or "INSERT 0 3", or 0 for tags without one ("BEGIN", "SET").
func tagRows(tag string) int64 {
	i := strings.LastIndexByte(tag, ' ')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(tag[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
