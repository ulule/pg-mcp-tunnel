// Package safesql validates SQL for restricted mode, ported from
// postgres-mcp's SafeSqlDriver (sql/safe_sql.py).
//
// The query is parsed with the real Postgres parser (libpg_query, run as WASM
// by go-pgquery, no cgo) and every node of the tree is checked against an
// allowlist. Unknown node types are rejected, so a parser upgrade can only make
// validation stricter. This is the first layer; the second is the read-only
// transaction with statement_timeout in pgdb.
package safesql

import (
	"errors"
	"fmt"
	"strings"

	pgq "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Validator checks SQL against the allowlists, plus extra functions
// configured per target.
type Validator struct {
	extraFunctions map[string]struct{}
}

func New(extraFunctions []string) *Validator {
	v := &Validator{extraFunctions: make(map[string]struct{}, len(extraFunctions))}
	for _, f := range extraFunctions {
		v.extraFunctions[normalizeFunc(f)] = struct{}{}
	}
	return v
}

// Validate returns nil if sql only contains allowed read-only statements.
func (v *Validator) Validate(sql string) error {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return fmt.Errorf("Failed to parse SQL statement: %w", err)
	}
	if len(tree.GetStmts()) == 0 {
		return errors.New("empty SQL statement")
	}
	for _, raw := range tree.GetStmts() {
		stmt := nodeMessage(raw.GetStmt())
		if stmt == nil {
			return errors.New("empty SQL statement")
		}
		name := string(stmt.Descriptor().Name())
		if _, ok := allowedStmts[name]; !ok {
			return fmt.Errorf("only SELECT, ANALYZE, VACUUM, EXPLAIN, SHOW and other read-only statements are allowed, got %s", name)
		}
		if err := v.walk(stmt); err != nil {
			return err
		}
	}
	return nil
}

// structural messages wrap or group real nodes and carry no semantics.
var structural = map[string]struct{}{
	"Node":        {},
	"List":        {},
	"RawStmt":     {},
	"ParseResult": {},
}

func (v *Validator) walk(m protoreflect.Message) error {
	name := string(m.Descriptor().Name())
	if _, ok := structural[name]; !ok {
		if _, ok := allowedNodes[name]; !ok {
			return fmt.Errorf("node type %s is not allowed", name)
		}
		if err := v.check(m.Interface()); err != nil {
			return err
		}
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsList():
			l := val.List()
			for i := 0; i < l.Len() && err == nil; i++ {
				err = v.walk(l.Get(i).Message())
			}
		case fd.IsMap():
			// The parse tree has no map fields; refuse rather than skip.
			err = fmt.Errorf("unexpected map field %s in %s", fd.Name(), name)
		default:
			err = v.walk(val.Message())
		}
		return err == nil
	})
	return err
}

func (v *Validator) check(n any) error {
	switch n := n.(type) {
	case *pgq.A_Expr:
		if n.GetKind() == pgq.A_Expr_Kind_AEXPR_LIKE || n.GetKind() == pgq.A_Expr_Kind_AEXPR_ILIKE {
			c := n.GetRexpr().GetAConst()
			if c == nil || c.GetSval() == nil {
				return errors.New("LIKE pattern must be a constant string")
			}
		}
	case *pgq.FuncCall:
		parts := make([]string, 0, len(n.GetFuncname()))
		for _, p := range n.GetFuncname() {
			parts = append(parts, p.GetString_().GetSval())
		}
		full := strings.Join(parts, ".")
		name := normalizeFunc(full)
		if _, ok := allowedFunctions[name]; ok {
			return nil
		}
		if _, ok := v.extraFunctions[name]; ok {
			return nil
		}
		return fmt.Errorf("function %s is not allowed in restricted mode (add it to allowed_functions in the target config if it is read-only)", strings.ToLower(full))
	case *pgq.SelectStmt:
		if len(n.GetLockingClause()) > 0 {
			return errors.New("locking clause on select is prohibited")
		}
	case *pgq.ExplainStmt:
		for _, o := range n.GetOptions() {
			if o.GetDefElem().GetDefname() == "analyze" {
				return errors.New("EXPLAIN ANALYZE is not supported in restricted mode")
			}
		}
	case *pgq.CreateExtensionStmt:
		if _, ok := allowedExtensions[n.GetExtname()]; !ok {
			return fmt.Errorf("CREATE EXTENSION %s is not supported", n.GetExtname())
		}
	}
	return nil
}

// normalizeFunc lowercases a function name and strips a pg_catalog. prefix.
func normalizeFunc(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimPrefix(name, "pg_catalog.")
}

// nodeMessage unwraps a Node oneof to its concrete message.
func nodeMessage(n *pgq.Node) protoreflect.Message {
	if n == nil {
		return nil
	}
	m := n.ProtoReflect()
	oneof := m.Descriptor().Oneofs().ByName("node")
	if oneof == nil {
		return nil
	}
	fd := m.WhichOneof(oneof)
	if fd == nil {
		return nil
	}
	return m.Get(fd).Message()
}

// CheckIndexDefinition ensures def is exactly one CREATE INDEX statement, so
// table or column names coming from the model can't smuggle other SQL into
// hypopg_create_index.
func CheckIndexDefinition(def string) error {
	tree, err := pgquery.Parse(def)
	if err != nil {
		return err
	}
	if len(tree.GetStmts()) != 1 || tree.GetStmts()[0].GetStmt().GetIndexStmt() == nil {
		return errors.New("not a single CREATE INDEX statement")
	}
	return nil
}

// Warmup compiles the WASM parser (~0.7s, once per process) so the first
// restricted-mode query doesn't pay for it.
func Warmup() { _, _ = pgquery.Parse("SELECT 1") }
