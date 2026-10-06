package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/safesql"
)

// HypotheticalIndex is one entry of explain_query's hypothetical_indexes.
type HypotheticalIndex struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Using   string   `json:"using,omitempty"`
}

// Name mirrors postgres-mcp's IndexDefinition.name (sql/index.py).
func (h HypotheticalIndex) Name() string {
	cleaned := make([]string, len(h.Columns))
	for i, col := range h.Columns {
		c := strings.NewReplacer("(", "_", ")", "_", " ", "_", ",", "_").Replace(col)
		for strings.Contains(c, "__") {
			c = strings.ReplaceAll(c, "__", "_")
		}
		cleaned[i] = strings.TrimRight(c, "_")
	}
	name := fmt.Sprintf("crystaldba_idx_%s_%s_%d", h.Table, strings.Join(cleaned, "_"), len(h.Columns))
	if u := h.using(); u != "btree" {
		name += "_" + u
	}
	return name
}

// Definition is the CREATE INDEX statement handed to hypopg_create_index.
func (h HypotheticalIndex) Definition() string {
	return fmt.Sprintf("CREATE INDEX %s ON %s USING %s (%s)", h.Name(), h.Table, h.using(), strings.Join(h.Columns, ", "))
}

func (h HypotheticalIndex) using() string {
	if h.Using == "" {
		return "btree"
	}
	return h.Using
}

// DB is the subset of *pgdb.DB used here.
type DB interface {
	pgdb.Querier
	Tx(ctx context.Context, fn func(q pgdb.Querier) error) error
	Restricted() bool
}

var (
	bindVarRe = regexp.MustCompile(`\$\d+`)
	likeRe    = regexp.MustCompile(`(?i)\bLIKE\b`)
)

// Run implements the explain_query tool. The returned error is shown to the
// model as "Error: <err>".
func Run(ctx context.Context, db DB, v *safesql.Validator, sql string, analyze bool, hypo []HypotheticalIndex) (string, error) {
	if len(hypo) > 0 && analyze {
		return "", errors.New("Cannot use analyze and hypothetical indexes together")
	}
	if analyze && db.Restricted() {
		return "", errors.New("EXPLAIN ANALYZE is not supported in restricted mode")
	}

	opts := []string{"FORMAT JSON"}
	if analyze {
		opts = append(opts, "ANALYZE")
	}
	if bindVarRe.MatchString(sql) {
		major, err := pgdb.ServerMajorVersion(ctx, db)
		if err != nil {
			return "", fmt.Errorf("Error executing explain plan: %w", err)
		}
		if major < 16 || likeRe.MatchString(sql) {
			return "", errors.New("query contains bind variables ($1, $2, ...): replace them with literal values " +
				"(generic plans need PostgreSQL 16+ and don't work with LIKE expressions)")
		}
		opts = append(opts, "GENERIC_PLAN")
	}
	if len(hypo) > 0 {
		opts = append(opts, "COSTS TRUE")
	}
	explainSQL := "EXPLAIN (" + strings.Join(opts, ", ") + ") " + sql
	if db.Restricted() {
		if err := v.Validate(explainSQL); err != nil {
			return "", err
		}
	}

	if len(hypo) > 0 {
		return explainHypothetical(ctx, db, explainSQL, hypo)
	}

	var res *pgdb.Result
	var err error
	if analyze {
		// EXPLAIN ANALYZE executes the statement: measure it, then roll back.
		err = db.Tx(ctx, func(q pgdb.Querier) error {
			res, err = q.Query(ctx, explainSQL)
			return err
		})
	} else {
		res, err = db.Query(ctx, explainSQL)
	}
	if err != nil {
		return "", fmt.Errorf("Error executing explain plan: %w", err)
	}
	return formatResult(res)
}

func explainHypothetical(ctx context.Context, db DB, explainSQL string, hypo []HypotheticalIndex) (string, error) {
	ext, err := pgdb.CheckExtension(ctx, db, "hypopg")
	if err != nil {
		return "", err
	}
	if !ext.Installed {
		return hypopgMissingMessage(ctx, db, ext), nil
	}
	defs := make([]string, len(hypo))
	for i, h := range hypo {
		if h.Table == "" {
			return "", errors.New("Missing 'table' in index definition")
		}
		if len(h.Columns) == 0 {
			return "", errors.New("Missing 'columns' in index definition")
		}
		defs[i] = h.Definition()
		if err := safesql.CheckIndexDefinition(defs[i]); err != nil {
			return "", fmt.Errorf("invalid hypothetical index %q: %w", defs[i], err)
		}
	}

	var res *pgdb.Result
	err = db.Tx(ctx, func(q pgdb.Querier) error {
		// Hypothetical indexes live in the backend, outside the transaction:
		// never leave them behind on a pooled connection. The savepoint lets
		// the reset run even after the EXPLAIN failed and aborted the tx.
		if _, err := q.Query(ctx, "SAVEPOINT pgmcp_hypo; SELECT hypopg_reset()"); err != nil {
			return err
		}
		defer q.Query(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT pgmcp_hypo; SELECT hypopg_reset()") //nolint:errcheck
		for _, d := range defs {
			if _, err := q.Query(ctx, "SELECT hypopg_create_index($1)", d); err != nil {
				return err
			}
		}
		var err error
		res, err = q.Query(ctx, explainSQL)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("Error generating explain plan with hypothetical indexes: %w", err)
	}
	return formatResult(res)
}

func formatResult(res *pgdb.Result) (string, error) {
	if res.Empty() {
		return "", errors.New("No results returned from EXPLAIN")
	}
	raw, ok := res.Rows[0][0].(json.RawMessage)
	if !ok {
		return "", fmt.Errorf("Expected JSON from EXPLAIN, got %T", res.Rows[0][0])
	}
	text, err := FormatPlan(raw)
	if err != nil {
		return "", fmt.Errorf("Internal error converting explain plan - do not retry: %w", err)
	}
	return text, nil
}

func hypopgMissingMessage(ctx context.Context, db DB, ext pgdb.Extension) string {
	if ext.Available {
		return "The **hypopg** extension is required to test hypothetical indexes, but it is not currently installed.\n\n" +
			"You can ask me to install 'hypopg' using the 'execute_query' tool.\n\n" +
			"**Is it safe?** Installing 'hypopg' is generally safe and a standard practice for index testing. " +
			"It adds a virtual layer that simulates indexes without actually creating them in the database. " +
			"It requires database privileges (often superuser) to install.\n\n" +
			"**What does it do?** It allows you to create virtual indexes and test how they would affect query performance " +
			"without the overhead of actually creating the indexes.\n\n" +
			"**How to undo?** If you later decide to remove it, you can ask me to run 'DROP EXTENSION hypopg;'."
	}
	major := "XX"
	if v, err := pgdb.ServerMajorVersion(ctx, db); err == nil && v > 0 {
		major = fmt.Sprint(v)
	}
	return "The **hypopg** extension is not available on this PostgreSQL server.\n\n" +
		"To install HypoPG:\n" +
		"1. For Debian/Ubuntu: `sudo apt-get install postgresql-" + major + "-hypopg`\n" +
		"2. For RHEL/CentOS: `sudo yum install postgresql" + major + "-hypopg`\n" +
		"3. For MacOS with Homebrew: `brew install hypopg`\n" +
		"4. For other systems, build from source: `git clone https://github.com/HypoPG/hypopg`\n\n" +
		"After installing the extension packages, connect to your database and run: `CREATE EXTENSION hypopg;`"
}
