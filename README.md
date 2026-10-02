<p align="center">
  <img src="assets/logo.png" alt="pg-mcp-tunnel logo" width="280" />
</p>

# pg-mcp-tunnel

A small Go binary that wraps [`crystaldba/postgres-mcp`](https://github.com/crystaldba/postgres-mcp) and adds in-process SSH tunneling.

It lets Claude Code (or any MCP client) talk to a Postgres database that is only reachable through SSH — without bash scripts, without `ssh -L`, without manual tunnel management.

## How it works

```
MCP client ──stdio──▶ pg-mcp-tunnel ──stdio──▶ postgres-mcp (Python)
                            │                         │
                            │ net.Listen              │ DATABASE_URI=
                            ▼                         │  127.0.0.1:LOCAL
                    127.0.0.1:LOCAL ◀────────────────┘
                            │
                            │ TCP proxy (Go)
                            ▼
                    127.0.0.1:INTERNAL ◀──ssh -L──▶ remote-host:5432
                            ▲
                            │ shepherd (sh + lifeline FD 3)
                            │ kills ssh if pg-mcp-tunnel dies (any cause)
```

- **Async startup.** pg-mcp-tunnel binds the user-visible local port itself (`net.Listen`) in milliseconds and launches `uv run postgres-mcp` immediately. ssh runs in parallel on a separate internal port; per inbound connection the Go proxy waits for ssh to come up, then forwards bytes. Claude doesn't time out on MCP `initialize` while ssh is still negotiating with the remote.
- **SSH auth** is delegated to OpenSSH: `ssh-agent`, `~/.ssh/config`, `ProxyCommand`/`ProxyJump`, FIDO keys, the macOS Keychain — all work out of the box.
- **Stdio passthrough.** postgres-mcp's stdin/stdout are wired straight to the MCP client; the MCP protocol is fully transparent.
- **DATABASE_URI** points at the user-visible local port with `connect_timeout=30` appended, so psycopg waits patiently while the proxy holds the connection until ssh has finished its handshake.
- **No orphans on parent death.** ssh runs under a `/bin/sh` shepherd that watches a lifeline pipe on FD 3. When pg-mcp-tunnel dies for any reason — clean shutdown, panic, SIGKILL, Claude force-quit — the kernel closes the pipe, the shepherd reads EOF, SIGTERMs ssh, and exits. No more stray `ssh -N` processes accumulating in the background.
- **Failure surfacing.** If ssh dies or doesn't come up within 30 s, the tail of ssh's stderr (8 KB ring buffer) is captured. The local listener is closed (subsequent reconnects get `ECONNREFUSED` instantly instead of hanging 30 s), and pg-mcp-tunnel exits with the ssh error on its own stderr — which Claude Desktop shows in the "Server disconnected" tooltip.

## Install

### 1. Install `postgres-mcp`

```bash
uv pip install postgres-mcp --system
```

### 2. Install the Go binary

```bash
go install github.com/ulule/pg-mcp-tunnel/cmd/pg-mcp-tunnel@latest
```

Verify with `uv run postgres-mcp --help`. The Go binary spawns `uv run postgres-mcp` as a subprocess.

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
```

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

Every `pg-mcp-tunnel --target X` process observes the Postgres wire protocol flowing through its relay and appends one JSON line per query to `/tmp/pg-mcp-tunnel-<uid>/X.queries.jsonl`: SQL, duration, command tag, row count, error / SQLSTATE, connection open/close. `/tmp` is wiped at reboot, so recordings start fresh. This includes the queries postgres-mcp issues on its own (`BEGIN TRANSACTION READ ONLY`, health checks, catalog introspection).

```bash
pg-mcp-tunnel ui --open        # http://localhost:7878
```

The dashboard tails all targets' files and shows, per database: live stream, running tunnel / open connections, q/s sparkline, avg/p95, errors, plus a *Top queries* view grouped by normalized SQL (sort by calls, total, avg, max). Keyboard: `/` to filter, `space` to pause.

Notes:
- Observation needs plaintext between postgres-mcp and the relay: SSH targets always go through the relay; direct targets are relayed only when their URI has `sslmode=disable` (otherwise they connect directly as before). Connections that negotiate TLS are shown as opaque.
- Recorded files contain SQL text (possibly with literal values), are `0600`, and are capped at 20 MiB. Disable with `--record-queries=false`.
- The UI only answers requests addressed to `localhost`/`127.0.0.1` (DNS-rebinding guard).

## License

MIT
