// Package pgmcp is the in-process Postgres MCP server, a Go port of
// crystaldba/postgres-mcp. Tool names and parameters match postgres-mcp so
// existing client permission rules (mcp__<target>__execute_sql, ...) keep
// working.
package pgmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/safesql"
)

const (
	ModeRestricted   = "restricted"
	ModeUnrestricted = "unrestricted"
)

// Version is reported in the MCP initialize response.
var Version = "dev"

type Options struct {
	URI  string
	Mode string // ModeRestricted or ModeUnrestricted
	// ExtraFunctions extends the restricted-mode function allowlist.
	ExtraFunctions []string
	Logger         *slog.Logger
	// TunnelErr reports a permanent ssh tunnel failure; nil when healthy or
	// for direct targets.
	TunnelErr func() error
	// OnToolCall, if set, is called after every tool call (e.g. to record it
	// for the web UI).
	OnToolCall func(ToolCall)
}

// ToolCall describes one answered tool call.
type ToolCall struct {
	Name     string
	Args     json.RawMessage // arguments after defaults were applied
	Duration time.Duration
	Output   string // text returned to the model, "Error: ..." included
	IsError  bool
}

// EstimateTokens roughly estimates how many LLM tokens text costs: ~3.5
// characters per token, a middle ground between prose (~4) and JSON or SQL
// (~3). Good enough to compare calls, not to bill them.
func EstimateTokens(text string) int {
	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}
	return (n*2 + 6) / 7
}

type Server struct {
	db         *pgdb.DB
	validator  *safesql.Validator
	mcp        *mcp.Server
	logger     *slog.Logger
	onToolCall func(ToolCall)
}

// New builds the server. It never dials the database.
func New(opts Options) (*Server, error) {
	if opts.Mode != ModeRestricted && opts.Mode != ModeUnrestricted {
		return nil, errors.New("access mode must be restricted or unrestricted")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	db, err := pgdb.Open(pgdb.Options{
		URI:        opts.URI,
		Restricted: opts.Mode == ModeRestricted,
		TunnelErr:  opts.TunnelErr,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		db:         db,
		validator:  safesql.New(opts.ExtraFunctions),
		logger:     opts.Logger,
		onToolCall: opts.OnToolCall,
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "pg-mcp-tunnel", Version: Version}, &mcp.ServerOptions{
		Logger: opts.Logger,
	})
	if db.Restricted() {
		go safesql.Warmup()
	}
	s.mcp.AddReceivingMiddleware(nullArgumentsAsEmpty)
	s.registerTools()
	return s, nil
}

// nullArgumentsAsEmpty rewrites a tools/call with missing or null arguments
// to {}: the SDK panics applying schema defaults to a null object.
func nullArgumentsAsEmpty(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if r, ok := req.(*mcp.CallToolRequest); ok && r.Params != nil {
			if a := bytes.TrimSpace(r.Params.Arguments); len(a) == 0 || bytes.Equal(a, []byte("null")) {
				r.Params.Arguments = json.RawMessage("{}")
			}
		}
		return next(ctx, method, req)
	}
}

// Run serves MCP on t until the client disconnects or ctx is canceled.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	err := s.mcp.Run(ctx, t)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *Server) Close() { s.db.Close() }
