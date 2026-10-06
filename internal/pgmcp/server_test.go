package pgmcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Integration tests need a disposable database, e.g.:
//
//	createdb pgmcp_test
//	PGMCP_TEST_DATABASE_URL='postgresql://localhost/pgmcp_test?sslmode=disable' go test ./internal/pgmcp/...
//
// The fixtures (users, orders) are created by the test if missing.
func testURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("PGMCP_TEST_DATABASE_URL")
	if uri == "" {
		t.Skip("PGMCP_TEST_DATABASE_URL not set")
	}
	return uri
}

type client struct {
	t  *testing.T
	cs *mcp.ClientSession
}

func connect(t *testing.T, opts Options) *client {
	t.Helper()
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverT) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		cancel()
		<-done
		srv.Close()
	})
	return &client{t: t, cs: cs}
}

// call returns the text content and whether the tool reported an error.
func (c *client) call(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	res, err := c.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		sb.WriteString(content.(*mcp.TextContent).Text)
	}
	return sb.String(), res.IsError
}

func (c *client) ok(name string, args map[string]any) string {
	c.t.Helper()
	text, isErr := c.call(name, args)
	if isErr {
		c.t.Fatalf("%s(%v): %s", name, args, text)
	}
	return text
}

func setupFixtures(t *testing.T, uri string) {
	t.Helper()
	c := connect(t, Options{URI: uri, Mode: ModeUnrestricted})
	c.ok("execute_sql", map[string]any{"sql": `
		CREATE TABLE IF NOT EXISTS users (id serial PRIMARY KEY, email text NOT NULL UNIQUE, name text, data jsonb, score numeric(10,2), ratio float8, active bool);
		CREATE TABLE IF NOT EXISTS orders (id bigserial PRIMARY KEY, user_id int REFERENCES users(id), amount numeric);
		INSERT INTO users (email, name, data, score, ratio, active)
		SELECT 'u' || g || '@ex.com', 'n' || (g % 50), jsonb_build_object('g', g), g * 1.5, g / 4.0, g % 2 = 0
		FROM generate_series(1, 200) g
		ON CONFLICT DO NOTHING`})
}

func TestToolsList(t *testing.T) {
	// No database needed: the pool is lazy.
	c := connect(t, Options{URI: "postgresql://nobody@127.0.0.1:1/x", Mode: ModeRestricted})
	res, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	slices.Sort(names)
	want := []string{"analyze_db_health", "execute_sql", "explain_query", "get_object_details", "get_top_queries", "list_objects", "list_schemas"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if d := byName["execute_sql"].Description; d != "Execute a read-only SQL query" {
		t.Errorf("execute_sql description in restricted mode = %q", d)
	}

	schema, _ := json.Marshal(byName["get_top_queries"].InputSchema)
	for _, s := range []string{`"sort_by"`, `"default":"resources"`, `"limit"`, `"default":10`} {
		if !strings.Contains(string(schema), s) {
			t.Errorf("get_top_queries schema %s missing %s", schema, s)
		}
	}
}

func TestToolsUnrestricted(t *testing.T) {
	uri := testURI(t)
	setupFixtures(t, uri)
	c := connect(t, Options{URI: uri, Mode: ModeUnrestricted})

	if out := c.ok("list_schemas", nil); !strings.Contains(out, `"schema_name":"public"`) {
		t.Errorf("list_schemas: %s", out)
	}
	if out := c.ok("list_objects", map[string]any{"schema_name": "public"}); !strings.Contains(out, `{"schema":"public","name":"users","type":"BASE TABLE"}`) {
		t.Errorf("list_objects: %s", out)
	}
	if out := c.ok("list_objects", map[string]any{"schema_name": "public", "object_type": "extension"}); !strings.Contains(out, `"name":"plpgsql"`) {
		t.Errorf("list_objects extension: %s", out)
	}
	if out, isErr := c.call("list_objects", map[string]any{"schema_name": "public", "object_type": "nope"}); !isErr || out != "Error: Unsupported object type: nope" {
		t.Errorf("list_objects nope: %v %s", isErr, out)
	}

	out := c.ok("get_object_details", map[string]any{"schema_name": "public", "object_name": "users"})
	var details struct {
		Columns []struct {
			Column string `json:"column"`
		} `json:"columns"`
		Constraints []struct {
			Name    string   `json:"name"`
			Type    string   `json:"type"`
			Columns []string `json:"columns"`
		} `json:"constraints"`
		Indexes []struct{ Name string } `json:"indexes"`
	}
	if err := json.Unmarshal([]byte(out), &details); err != nil {
		t.Fatalf("get_object_details: %v: %s", err, out)
	}
	if len(details.Columns) < 7 || details.Columns[0].Column != "id" || len(details.Indexes) < 2 {
		t.Errorf("get_object_details: %s", out)
	}

	// Typed JSON values, column order kept.
	out = c.ok("execute_sql", map[string]any{"sql": `SELECT 1::int AS i, 2.50::numeric AS n, 0.5::float8 AS f, true AS b, NULL AS z, '{"a":1}'::jsonb AS j, 'x' AS s, '2024-01-02'::date AS d`})
	if want := `[{"i":1,"n":2.50,"f":0.5,"b":true,"z":null,"j":{"a":1},"s":"x","d":"2024-01-02"}]`; out != want {
		t.Errorf("execute_sql types:\n got %s\nwant %s", out, want)
	}
	if out := c.ok("execute_sql", map[string]any{"sql": "SELECT 1 WHERE false"}); out != "[]" {
		t.Errorf("empty select: %s", out)
	}
	if out := c.ok("execute_sql", map[string]any{"sql": "CREATE TEMP TABLE tmp_x (id int)"}); out != "No results" {
		t.Errorf("ddl: %s", out)
	}
	if out := c.ok("execute_sql", map[string]any{"sql": "SELECT 1 AS a; SELECT 2 AS b"}); out != `[{"b":2}]` {
		t.Errorf("multi statement: %s", out)
	}
	if out, isErr := c.call("execute_sql", map[string]any{"sql": "SELECT * FROM does_not_exist"}); !isErr || !strings.Contains(out, "does_not_exist") {
		t.Errorf("sql error: %v %s", isErr, out)
	}

	if out := c.ok("explain_query", map[string]any{"sql": "SELECT * FROM users WHERE id = 1"}); !strings.Contains(out, "→ Index Scan") {
		t.Errorf("explain: %s", out)
	}
	if out := c.ok("explain_query", map[string]any{"sql": "SELECT * FROM users WHERE id = $1"}); !strings.Contains(out, "→ ") {
		t.Errorf("explain generic plan: %s", out)
	}
	hypo := map[string]any{"sql": "SELECT * FROM users WHERE name = 'n1'", "hypothetical_indexes": []any{map[string]any{"table": "users", "columns": []any{"name"}}}}
	if out := c.ok("explain_query", hypo); !strings.Contains(out, "hypopg") && !strings.Contains(out, "pgmcp_idx_users_name_1") {
		t.Errorf("explain hypothetical: %s", out)
	}
	hypo["hypothetical_indexes"] = []any{map[string]any{"table": "users (id); DROP TABLE users; --", "columns": []any{"name"}}}
	if out, isErr := c.call("explain_query", hypo); !isErr && !strings.Contains(out, "hypopg") {
		t.Errorf("explain hypothetical injection: %s", out)
	}
	// EXPLAIN ANALYZE of a write is measured, then rolled back.
	before := c.ok("execute_sql", map[string]any{"sql": "SELECT count(*) AS n FROM orders"})
	if out := c.ok("explain_query", map[string]any{"sql": "INSERT INTO orders (amount) VALUES (1)", "analyze": true}); !strings.Contains(out, "Execution Time") {
		t.Errorf("explain analyze: %s", out)
	}
	if after := c.ok("execute_sql", map[string]any{"sql": "SELECT count(*) AS n FROM orders"}); after != before {
		t.Errorf("EXPLAIN ANALYZE committed: %s -> %s", before, after)
	}

	out = c.ok("analyze_db_health", nil)
	for _, want := range []string{"Invalid index check: ", "Duplicate index check: ", "Connection health: ", "Vacuum health: ",
		"Sequence health: ", "Replication health: ", "Buffer health for tables: ", "Constraint health: "} {
		if !strings.Contains(out, want) {
			t.Errorf("analyze_db_health missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Error:") {
		t.Errorf("analyze_db_health has errors:\n%s", out)
	}
	if out := c.ok("analyze_db_health", map[string]any{"health_type": "bogus"}); !strings.HasPrefix(out, "Invalid health types provided") {
		t.Errorf("analyze_db_health bogus: %s", out)
	}

	for _, sortBy := range []string{"resources", "mean_time", "total_time"} {
		out, isErr := c.call("get_top_queries", map[string]any{"sort_by": sortBy, "limit": 3})
		if isErr {
			t.Errorf("get_top_queries %s: %s", sortBy, out)
		}
	}
}

func TestToolsRestricted(t *testing.T) {
	uri := testURI(t)
	setupFixtures(t, uri)
	c := connect(t, Options{URI: uri, Mode: ModeRestricted, ExtraFunctions: []string{"pg_backend_pid"}})

	if out := c.ok("execute_sql", map[string]any{"sql": "SELECT count(*) AS n FROM users WHERE email LIKE 'u1%'"}); !strings.HasPrefix(out, `[{"n":`) {
		t.Errorf("restricted select: %s", out)
	}
	if out := c.ok("execute_sql", map[string]any{"sql": "SHOW statement_timeout"}); out != `[{"statement_timeout":"30s"}]` {
		t.Errorf("statement_timeout: %s", out)
	}
	if out := c.ok("execute_sql", map[string]any{"sql": "SHOW transaction_read_only"}); out != `[{"transaction_read_only":"on"}]` {
		t.Errorf("transaction_read_only: %s", out)
	}
	c.ok("execute_sql", map[string]any{"sql": "SELECT pg_backend_pid()"})

	for sql, want := range map[string]string{
		"DELETE FROM users":              "only SELECT",
		"SELECT pg_sleep(1)":             "function pg_sleep",
		"SELECT 1; COMMIT":               "only SELECT",
		"SELECT * FROM users FOR UPDATE": "locking clause",
	} {
		out, isErr := c.call("execute_sql", map[string]any{"sql": sql})
		if !isErr || !strings.Contains(out, want) {
			t.Errorf("%s: %v %s", sql, isErr, out)
		}
	}
	// Even if validation were bypassed, the transaction is read-only.
	srv, err := New(Options{URI: uri, Mode: ModeRestricted})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := srv.db.Query(context.Background(), "DELETE FROM orders"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("write in restricted tx: %v", err)
	}

	if out, isErr := c.call("explain_query", map[string]any{"sql": "SELECT 1", "analyze": true}); !isErr || !strings.Contains(out, "not supported in restricted mode") {
		t.Errorf("restricted explain analyze: %v %s", isErr, out)
	}
	if out, isErr := c.call("explain_query", map[string]any{"sql": "DELETE FROM users"}); !isErr || !strings.Contains(out, "DeleteStmt") {
		t.Errorf("restricted explain delete: %v %s", isErr, out)
	}
	c.ok("explain_query", map[string]any{"sql": "SELECT * FROM users WHERE name = 'n1'"})
	if out := c.ok("analyze_db_health", nil); strings.Contains(out, "Error:") {
		t.Errorf("restricted analyze_db_health:\n%s", out)
	}
}

func TestTunnelFailure(t *testing.T) {
	boom := errors.New("Permission denied (publickey)")
	c := connect(t, Options{URI: "postgresql://nobody@127.0.0.1:1/x", Mode: ModeRestricted, TunnelErr: func() error { return boom }})
	out, isErr := c.call("execute_sql", map[string]any{"sql": "SELECT 1"})
	if !isErr || out != "Error: ssh tunnel failed: Permission denied (publickey)" {
		t.Errorf("got %v %q", isErr, out)
	}
}
