# pg-mcp-tunnel

A small Go binary that wraps [`crystaldba/postgres-mcp`](https://github.com/crystaldba/postgres-mcp) and adds in-process SSH tunneling.

It lets Claude Code (or any MCP client) talk to a Postgres database that is only reachable through SSH — without bash scripts, without `ssh -L`, without manual tunnel management.

## How it works

```
MCP client ──stdio──▶ pg-mcp-tunnel ──stdio──▶ postgres-mcp (Python)
                            │
                            │ in-process SSH tunnel
                            ▼
                    127.0.0.1:RANDOM ◀──SSH channel──▶ remote-host:5432
```

- The binary opens an SSH connection (auth via `ssh-agent` or `~/.ssh/config`).
- It listens on a random `127.0.0.1` port and forwards connections to the remote Postgres through the SSH channel.
- It launches `uv run postgres-mcp` as a subprocess with `DATABASE_URI` pointing to that local port.
- Stdin/stdout are passed straight through, so the MCP protocol is fully transparent.
- When the client closes (or you kill the binary), the subprocess and the SSH tunnel are torn down cleanly.

## Install

### 1. Install the Go binary

```bash
go install github.com/ulule/pg-mcp-tunnel@latest
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

## License

MIT
