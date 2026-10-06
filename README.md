<p align="center">
  <img src="assets/logo.png" alt="pg-mcp-tunnel logo" width="280" />
</p>

# pg-mcp-tunnel

A single Go binary that serves a Postgres MCP server (a Go port of [`crystaldba/postgres-mcp`](https://github.com/crystaldba/postgres-mcp)) with in-process SSH tunneling. No Python, no `uv`, no cgo.

It lets Claude Code (or any MCP client) talk to a Postgres database that is only reachable through SSH — without bash scripts, without `ssh -L`, without manual tunnel management.

## How it works

```
MCP client ──stdio──▶ pg-mcp-tunnel (MCP server + pgx pool)
                            │
                            │ net.Listen          pool connects to
                            ▼                     127.0.0.1:LOCAL
                    127.0.0.1:LOCAL
                            │
                            │ TCP proxy (Go)
                            ▼
                    127.0.0.1:INTERNAL ◀──ssh -L──▶ remote-host:5432
                            ▲
                            │ shepherd (sh + lifeline FD 3)
                            │ kills ssh if pg-mcp-tunnel dies (any cause)
```

- **Async startup.** pg-mcp-tunnel binds the user-visible local port itself (`net.Listen`) in milliseconds and answers MCP `initialize` immediately; the connection pool is lazy. ssh runs in parallel on a separate internal port; per inbound connection the Go proxy waits for ssh to come up, then forwards bytes. Claude doesn't time out on MCP `initialize` while ssh is still negotiating with the remote.
- **SSH auth** is delegated to OpenSSH: `ssh-agent`, `~/.ssh/config`, `ProxyCommand`/`ProxyJump`, FIDO keys, the macOS Keychain — all work out of the box.
- **In-process MCP server** built on the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk). The pool connects to the user-visible local port with `connect_timeout=30`, so it waits patiently while the proxy holds the connection until ssh has finished its handshake.
- **No orphans on parent death.** ssh runs under a `/bin/sh` shepherd that watches a lifeline pipe on FD 3. When pg-mcp-tunnel dies for any reason — clean shutdown, panic, SIGKILL, Claude force-quit — the kernel closes the pipe, the shepherd reads EOF, SIGTERMs ssh, and exits. No more stray `ssh -N` processes accumulating in the background.
- **Failure surfacing.** If ssh dies or doesn't come up within 30 s, the tail of ssh's stderr (8 KB ring buffer) is captured. The local listener is closed (subsequent reconnects get `ECONNREFUSED` instantly instead of hanging 30 s), the error is written to stderr, and the server keeps running: every tool call answers `Error: ssh tunnel failed: <reason>` so the model sees the real cause.

## Install

```bash
go install github.com/ulule/pg-mcp-tunnel/cmd/pg-mcp-tunnel@latest
```

That's it: one pure-Go binary (`CGO_ENABLED=0` works), nothing else to install. The Postgres parser used by restricted mode is embedded as WASM.

## Configure

Create `~/.config/pg-mcp-tunnel/config.yaml`:

```yaml
targets:
  local:
    postgres:
      uri: postgresql://user:pass@localhost:5432/mydb?sslmode=disable
    access_mode: unrestricted

  prod:
    ssh:
      host: db.example.com
      user: alice
    postgres:
      remote_host: localhost
      remote_port: 5432
      database: mydb
      user: alice
      sslmode: disable
    access_mode: restricted
    # Optional: extra read-only functions allowed in restricted mode.
    allowed_functions: [unaccent, public.my_readonly_fn]
```

## Tools

Same names and parameters as postgres-mcp, so existing permission rules (`mcp__postgres-prod__execute_sql`, …) keep working:

| Tool | What it does |
|---|---|
| `list_schemas`, `list_objects`, `get_object_details` | Catalog browsing (tables, views, sequences, extensions; columns, constraints, indexes). |
| `execute_sql` | Runs SQL; returns rows as JSON. Several statements are allowed, the last result set is returned. |
| `explain_query` | `EXPLAIN` as text, with `analyze` and `hypothetical_indexes` (needs the `hypopg` extension). `$1` placeholders use `GENERIC_PLAN` (PG 16+). |
| `get_top_queries` | Slowest / most resource-intensive queries from `pg_stat_statements`. |
| `analyze_db_health` | Index, connection, vacuum, sequence, replication, buffer and constraint checks. |

### Access modes

- **`unrestricted`**: any SQL, autocommit. `explain_query` with `analyze` runs inside a transaction that is rolled back.
- **`restricted`**: two layers.
  1. The SQL is parsed with the real Postgres parser (libpg_query) and every node is checked against an allowlist ported from postgres-mcp: `SELECT`/`EXPLAIN`/`SHOW`/… only, ~490 allowed functions (no `pg_sleep`, `set_config`, `pg_terminate_backend`, `dblink`, …), no `FOR UPDATE`, no `EXPLAIN ANALYZE`. Errors say what was rejected; extend the function list per target with `allowed_functions`.
  2. Every call runs in `BEGIN TRANSACTION READ ONLY` with `SET LOCAL statement_timeout = 30s`, then `ROLLBACK`.

  Use a read-only Postgres role for production anyway.

## Use with Claude Code

In `.mcp.json`:

```json
{
  "mcpServers": {
    "postgres-local": {
      "command": "pg-mcp-tunnel",
      "args": ["--target", "local"]
    },
    "postgres-prod": {
      "command": "pg-mcp-tunnel",
      "args": ["--target", "prod"]
    }
  }
}
```

## Live query dashboard

Every `pg-mcp-tunnel --target X` process observes the Postgres wire protocol flowing through its relay and appends one JSON line per query to `/tmp/pg-mcp-tunnel-<uid>/X.queries.jsonl`: SQL, duration, command tag, row count, error / SQLSTATE, connection open/close. `/tmp` is wiped at reboot, so recordings start fresh. This includes the queries the server issues on its own (`BEGIN TRANSACTION READ ONLY`, health checks, catalog introspection); its connections use `application_name=pg-mcp-tunnel`.

```bash
pg-mcp-tunnel ui --open        # http://localhost:7878
```

The dashboard tails all targets' files and shows, per database: live stream, running tunnel / open connections, q/s sparkline, avg/p95, errors, plus a *Top queries* view grouped by normalized SQL (sort by calls, total, avg, max). Keyboard: `/` to filter, `space` to pause.

MCP tool calls are recorded too, with an **estimated token cost** (≈ 3.5 characters per token) of the arguments and of the answer sent back to the model: shown per call in the live stream, per database in the sidebar, in total in the header, and per query in *Top queries* (sortable). Handy to spot the `SELECT *` that just ate 50k tokens of context.

The **⚙ config.yaml** link in the header shows the targets config (passwords masked) and opens it in your default editor.

Notes:
- Observation needs plaintext between the pool and the relay: SSH targets always go through the relay; direct targets are relayed only when their URI has `sslmode=disable` (otherwise they connect directly as before). Connections that negotiate TLS are shown as opaque.
- Recorded files contain SQL text (possibly with literal values), are `0600`, and are capped at 20 MiB. Disable with `--record-queries=false`.
- The UI only answers requests addressed to `localhost`/`127.0.0.1` (DNS-rebinding guard).
- The UI is rendered server-side with [htmx](https://htmx.org) (vendored in the binary, no CDN): filters live in the URL, so a reload or a bookmark restores the view; new rows are pushed over SSE.

## License

MIT
