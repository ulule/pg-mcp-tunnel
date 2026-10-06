// Package pgdb is the small Postgres access layer used by the MCP tools.
//
// Every call runs on its own pooled connection. In restricted mode each call
// is wrapped in BEGIN TRANSACTION READ ONLY / ROLLBACK with a server-side
// statement_timeout, mirroring postgres-mcp's SafeSqlDriver.
package pgdb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RestrictedTimeout bounds every statement in restricted mode.
const RestrictedTimeout = 30 * time.Second

// ErrTimeout is returned when a restricted-mode statement hits RestrictedTimeout.
var ErrTimeout = errors.New("Query execution timed out after 30 seconds in restricted mode. Consider simplifying your query or increasing the timeout.")

// Querier runs SQL and returns the last result set. Args are sent as text
// parameters ($1, $2, ...); without args the SQL may hold several statements.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (*Result, error)
}

type Options struct {
	URI        string
	Restricted bool
	// TunnelErr reports a permanent ssh tunnel failure; nil means healthy.
	TunnelErr func() error
}

type DB struct {
	pool       *pgxpool.Pool
	restricted bool
	tunnelErr  func() error
}

// Open builds the pool without dialing: connections are made on first use,
// so the MCP handshake never waits for the ssh tunnel.
func Open(opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(opts.URI)
	if err != nil {
		return nil, fmt.Errorf("parse database uri: %w", err)
	}
	cfg.MinConns = 0
	cfg.MaxConns = 5
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = 30 * time.Second
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "pg-mcp-tunnel"
	}
	// The default pings any connection idle for >1s: one extra ssh round trip
	// and a "-- ping" line in the query log on almost every tool call.
	cfg.ShouldPing = func(_ context.Context, p pgxpool.ShouldPingParams) bool {
		return p.IdleDuration > time.Minute
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool, restricted: opts.Restricted, tunnelErr: opts.TunnelErr}, nil
}

func (db *DB) Close() { db.pool.Close() }

func (db *DB) Restricted() bool { return db.restricted }

// Query runs sql on a fresh connection. In restricted mode it runs inside a
// read-only transaction that is always rolled back.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (*Result, error) {
	if !db.restricted {
		var res *Result
		err := db.withConn(ctx, func(c *pgconn.PgConn) error {
			var err error
			res, err = query(ctx, c, sql, args)
			return err
		})
		return res, err
	}
	var res *Result
	err := db.Tx(ctx, func(q Querier) error {
		var err error
		res, err = q.Query(ctx, sql, args...)
		return err
	})
	return res, err
}

// Tx pins one connection and runs fn inside a transaction that is always
// rolled back: read-only with statement_timeout in restricted mode, a plain
// transaction otherwise (used to measure EXPLAIN ANALYZE without side effects).
func (db *DB) Tx(ctx context.Context, fn func(q Querier) error) error {
	if db.restricted {
		var cancel context.CancelFunc
		// Backstop for a hung network: connect + statement_timeout + margin.
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second+RestrictedTimeout+5*time.Second)
		defer cancel()
	}
	return db.withConn(ctx, func(c *pgconn.PgConn) error {
		begin := "BEGIN"
		if db.restricted {
			begin = "BEGIN TRANSACTION READ ONLY; SET LOCAL statement_timeout = '" +
				strconv.FormatInt(RestrictedTimeout.Milliseconds(), 10) + "ms'"
		}
		if _, err := c.Exec(ctx, begin).ReadAll(); err != nil {
			return err
		}
		defer func() {
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := c.Exec(rbCtx, "ROLLBACK").ReadAll(); err != nil {
				// Don't hand a connection in an unknown state back to the pool.
				_ = c.Close(rbCtx)
			}
		}()
		err := fn(connQuerier{c})
		var pgErr *pgconn.PgError
		if db.restricted && errors.As(err, &pgErr) && pgErr.Code == "57014" {
			return ErrTimeout
		}
		return err
	})
}

func (db *DB) withConn(ctx context.Context, fn func(c *pgconn.PgConn) error) error {
	if db.tunnelErr != nil {
		if err := db.tunnelErr(); err != nil {
			return fmt.Errorf("ssh tunnel failed: %w", err)
		}
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		if db.tunnelErr != nil {
			if terr := db.tunnelErr(); terr != nil {
				return fmt.Errorf("ssh tunnel failed: %w", terr)
			}
		}
		return err
	}
	defer conn.Release()
	return fn(conn.Conn().PgConn())
}

type connQuerier struct{ c *pgconn.PgConn }

func (q connQuerier) Query(ctx context.Context, sql string, args ...any) (*Result, error) {
	return query(ctx, q.c, sql, args)
}

// query bypasses pgx's statement cache: the simple protocol without args (so
// several statements are allowed and the last result set wins), an unnamed
// extended-protocol statement with text parameters otherwise.
func query(ctx context.Context, c *pgconn.PgConn, sql string, args []any) (*Result, error) {
	if len(args) == 0 {
		mrr := c.Exec(ctx, sql)
		var last *Result
		for mrr.NextResult() {
			res, err := read(mrr.ResultReader())
			if err != nil {
				_ = mrr.Close()
				return nil, err
			}
			if len(res.Columns) > 0 {
				last = res
			}
		}
		if err := mrr.Close(); err != nil {
			return nil, err
		}
		if last == nil {
			last = &Result{}
		}
		return last, nil
	}
	params := make([][]byte, len(args))
	for i, a := range args {
		if a != nil {
			params[i] = fmt.Append(nil, a)
		}
	}
	return read(c.ExecParams(ctx, sql, params, nil, nil, nil))
}

// read drains rr. Unlike ResultReader.Read, it keeps the columns of a result
// set without rows.
func read(rr *pgconn.ResultReader) (*Result, error) {
	var rows [][][]byte
	for rr.NextRow() {
		values := rr.Values()
		row := make([][]byte, len(values))
		for i, v := range values {
			if v != nil {
				row[i] = append([]byte{}, v...)
			}
		}
		rows = append(rows, row)
	}
	fds := rr.FieldDescriptions()
	if _, err := rr.Close(); err != nil {
		return nil, err
	}
	return newResult(fds, rows), nil
}
