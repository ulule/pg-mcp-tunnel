package safesql

import (
	"strings"
	"testing"

	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestValidate(t *testing.T) {
	v := New([]string{"public.my_readonly_fn", "unaccent"})

	allowed := []string{
		"SELECT 1",
		"select id, name from users where id = 42 order by name limit 10",
		"SELECT u.id, count(*) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id HAVING count(*) > 1",
		"WITH recent AS (SELECT * FROM orders WHERE created_at > now() - interval '1 day') SELECT * FROM recent",
		"SELECT id, row_number() OVER (PARTITION BY user_id ORDER BY created_at) FROM orders",
		"SELECT pg_catalog.pg_relation_size('users')",
		"SELECT * FROM users WHERE email LIKE '%@ulule.com'",
		"SELECT * FROM users WHERE email ILIKE 'a%'",
		"SELECT coalesce(a, b)::text, CASE WHEN x IS NULL THEN 1 ELSE 2 END FROM t",
		"SELECT * FROM t WHERE id IN (1, 2, 3) AND name = ANY(ARRAY['a', 'b'])",
		"SELECT * FROM t WHERE id IN (SELECT id FROM u) OR EXISTS (SELECT 1 FROM v)",
		"SELECT data->>'key', data @> '{\"a\":1}'::jsonb FROM t",
		"SHOW statement_timeout",
		"EXPLAIN SELECT * FROM users",
		"EXPLAIN (FORMAT JSON, GENERIC_PLAN) SELECT * FROM users WHERE id = $1",
		"SELECT 1; SELECT 2",
		"SELECT public.my_readonly_fn(1)",
		"SELECT unaccent(name) FROM users",
		"CREATE EXTENSION IF NOT EXISTS hypopg",
		"SELECT hypopg_reset()",
	}
	for _, sql := range allowed {
		if err := v.Validate(sql); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", sql, err)
		}
	}

	denied := map[string]string{
		"INSERT INTO t VALUES (1)":         "only SELECT",
		"UPDATE t SET a = 1":               "only SELECT",
		"DELETE FROM t":                    "only SELECT",
		"DROP TABLE t":                     "only SELECT",
		"CREATE TABLE t (id int)":          "only SELECT",
		"TRUNCATE t":                       "only SELECT",
		"COPY t TO STDOUT":                 "only SELECT",
		"SET statement_timeout = 0":        "only SELECT",
		"BEGIN":                            "only SELECT",
		"COMMIT":                           "only SELECT",
		"SELECT 1; COMMIT; DELETE FROM t":  "only SELECT",
		"SELECT * FROM t FOR UPDATE":       "locking clause",
		"EXPLAIN ANALYZE SELECT 1":         "EXPLAIN ANALYZE",
		"EXPLAIN (ANALYZE false) SELECT 1": "EXPLAIN ANALYZE",
		"EXPLAIN DELETE FROM t":            "node type DeleteStmt",
		"SELECT pg_sleep(100)":             "function pg_sleep",
		"SELECT set_config('statement_timeout', '0', false)":    "function set_config",
		"SELECT pg_terminate_backend(123)":                      "function pg_terminate_backend",
		"SELECT dblink('x', 'y')":                               "function dblink",
		"SELECT lo_import('/etc/passwd')":                       "function lo_import",
		"SELECT public.other_fn()":                              "function public.other_fn",
		"SELECT * INTO new_t FROM t":                            "node type IntoClause",
		"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x": "node type DeleteStmt",
		"SELECT * FROM t WHERE a LIKE b":                        "LIKE pattern",
		"CREATE EXTENSION plpython3u":                           "CREATE EXTENSION plpython3u",
		"SELEC 1":                                               "Failed to parse",
		"":                                                      "empty",
	}
	for sql, want := range denied {
		err := v.Validate(sql)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want error containing %q", sql, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%q) = %q, want it to contain %q", sql, err, want)
		}
	}
}

func TestCheckIndexDefinition(t *testing.T) {
	if err := CheckIndexDefinition("CREATE INDEX i ON users USING btree (email, lower(name))"); err != nil {
		t.Errorf("valid definition rejected: %v", err)
	}
	for _, def := range []string{
		"CREATE INDEX i ON users USING btree (email); DROP TABLE users",
		"DROP TABLE users",
		"CREATE INDEX i ON users USING btree (email",
	} {
		if err := CheckIndexDefinition(def); err == nil {
			t.Errorf("CheckIndexDefinition(%q) = nil, want error", def)
		}
	}
}

// Every allowlisted node and statement name must exist in the parser's
// protobuf schema, otherwise the allowlist silently drifts from the parser.
func TestAllowlistNamesExist(t *testing.T) {
	files := (&pgq.ParseResult{}).ProtoReflect().Descriptor().ParentFile()
	msgs := files.Messages()
	exists := func(name string) bool { return msgs.ByName(protoreflect.Name(name)) != nil }
	for name := range allowedNodes {
		if !exists(name) {
			t.Errorf("allowedNodes: %s is not a pg_query message", name)
		}
	}
	for name := range allowedStmts {
		if !exists(name) {
			t.Errorf("allowedStmts: %s is not a pg_query message", name)
		}
		if _, ok := allowedNodes[name]; !ok {
			t.Errorf("allowedStmts: %s is missing from allowedNodes", name)
		}
	}
}
