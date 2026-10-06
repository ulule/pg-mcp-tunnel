// Package querylog observes Postgres wire-protocol traffic flowing through
// pg-mcp-tunnel, turns it into query events, and records them as JSON lines
// that the web UI tails.
package querylog

import "time"

const (
	KindStart      = "start"      // pg-mcp-tunnel process started
	KindStop       = "stop"       // pg-mcp-tunnel process exited cleanly
	KindConnect    = "connect"    // client completed a startup message
	KindDisconnect = "disconnect" // proxied connection closed
	KindQuery      = "query"      // one round trip, ended by ReadyForQuery
	KindError      = "error"      // server error outside any query (e.g. auth)
	KindOpaque     = "opaque"     // connection switched to TLS/GSS: no visibility
	KindTool       = "tool"       // one MCP tool call answered to the model
)

type Event struct {
	Time       time.Time `json:"ts"`
	Target     string    `json:"target"`
	PID        int       `json:"pid"`
	Kind       string    `json:"kind"`
	Conn       uint64    `json:"conn,omitempty"`
	Database   string    `json:"db,omitempty"`
	User       string    `json:"user,omitempty"`
	App        string    `json:"app,omitempty"`
	SQL        string    `json:"sql,omitempty"`
	DurationMS float64   `json:"duration_ms,omitempty"`
	Tag        string    `json:"tag,omitempty"`
	Rows       int64     `json:"rows,omitempty"`
	Error      string    `json:"error,omitempty"`
	SQLState   string    `json:"sqlstate,omitempty"`

	// Tool events only: the MCP tool name and estimated tokens of the
	// arguments the model sent (TokensIn) and of the answer it got back.
	Tool     string `json:"tool,omitempty"`
	TokensIn int    `json:"tokens_in,omitempty"`
	Tokens   int    `json:"tokens,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}
