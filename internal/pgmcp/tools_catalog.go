package pgmcp

import (
	"context"
	"fmt"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
)

func (s *Server) listSchemas(ctx context.Context, _ noInput) (string, error) {
	res, err := s.db.Query(ctx, `
		SELECT
			schema_name,
			schema_owner,
			CASE
				WHEN schema_name LIKE 'pg_%' THEN 'System Schema'
				WHEN schema_name = 'information_schema' THEN 'System Information Schema'
				ELSE 'User Schema'
			END as schema_type
		FROM information_schema.schemata
		ORDER BY schema_type, schema_name`)
	if err != nil {
		return "", err
	}
	return toJSON(res)
}

type objectEntry struct {
	Schema   string `json:"schema"`
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	DataType string `json:"data_type,omitempty"`
}

type extensionEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Relocatable bool   `json:"relocatable"`
}

func (s *Server) listObjects(ctx context.Context, in listObjectsInput) (string, error) {
	switch in.ObjectType {
	case "table", "view":
		tableType := "BASE TABLE"
		if in.ObjectType == "view" {
			tableType = "VIEW"
		}
		res, err := s.db.Query(ctx, `
			SELECT table_schema, table_name, table_type
			FROM information_schema.tables
			WHERE table_schema = $1 AND table_type = $2
			ORDER BY table_name`, in.SchemaName, tableType)
		if err != nil {
			return "", err
		}
		out := make([]objectEntry, len(res.Rows))
		for i, r := range res.Rows {
			out[i] = objectEntry{Schema: pgdb.Str(r[0]), Name: pgdb.Str(r[1]), Type: pgdb.Str(r[2])}
		}
		return toJSON(out)
	case "sequence":
		res, err := s.db.Query(ctx, `
			SELECT sequence_schema, sequence_name, data_type
			FROM information_schema.sequences
			WHERE sequence_schema = $1
			ORDER BY sequence_name`, in.SchemaName)
		if err != nil {
			return "", err
		}
		out := make([]objectEntry, len(res.Rows))
		for i, r := range res.Rows {
			out[i] = objectEntry{Schema: pgdb.Str(r[0]), Name: pgdb.Str(r[1]), DataType: pgdb.Str(r[2])}
		}
		return toJSON(out)
	case "extension":
		// Extensions are not schema-specific.
		res, err := s.db.Query(ctx, `
			SELECT extname, extversion, extrelocatable
			FROM pg_extension
			ORDER BY extname`)
		if err != nil {
			return "", err
		}
		return toJSON(extensions(res))
	default:
		return "", fmt.Errorf("Unsupported object type: %s", in.ObjectType)
	}
}

func extensions(res *pgdb.Result) []extensionEntry {
	out := make([]extensionEntry, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = extensionEntry{Name: pgdb.Str(r[0]), Version: pgdb.Str(r[1]), Relocatable: pgdb.Bool(r[2])}
	}
	return out
}

type columnDetail struct {
	Column     string `json:"column"`
	DataType   string `json:"data_type"`
	IsNullable string `json:"is_nullable"`
	Default    any    `json:"default"`
}

type constraintDetail struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Columns []string `json:"columns"`
}

type indexDetail struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type tableDetails struct {
	Basic struct {
		Schema string `json:"schema"`
		Name   string `json:"name"`
		Type   string `json:"type"`
	} `json:"basic"`
	Columns     []columnDetail     `json:"columns"`
	Constraints []constraintDetail `json:"constraints"`
	Indexes     []indexDetail      `json:"indexes"`
}

func (s *Server) getObjectDetails(ctx context.Context, in objectDetailsInput) (string, error) {
	switch in.ObjectType {
	case "table", "view":
		return s.tableDetails(ctx, in)
	case "sequence":
		res, err := s.db.Query(ctx, `
			SELECT sequence_schema AS schema, sequence_name AS name, data_type, start_value, increment
			FROM information_schema.sequences
			WHERE sequence_schema = $1 AND sequence_name = $2`, in.SchemaName, in.ObjectName)
		if err != nil {
			return "", err
		}
		if res.Empty() {
			return "{}", nil
		}
		return toJSON(res.Row(0))
	case "extension":
		res, err := s.db.Query(ctx, `
			SELECT extname, extversion, extrelocatable
			FROM pg_extension
			WHERE extname = $1`, in.ObjectName)
		if err != nil {
			return "", err
		}
		if res.Empty() {
			return "{}", nil
		}
		return toJSON(extensions(res)[0])
	default:
		return "", fmt.Errorf("Unsupported object type: %s", in.ObjectType)
	}
}

func (s *Server) tableDetails(ctx context.Context, in objectDetailsInput) (string, error) {
	var out tableDetails
	out.Basic.Schema, out.Basic.Name, out.Basic.Type = in.SchemaName, in.ObjectName, in.ObjectType

	cols, err := s.db.Query(ctx, `
		SELECT column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, in.SchemaName, in.ObjectName)
	if err != nil {
		return "", err
	}
	out.Columns = make([]columnDetail, len(cols.Rows))
	for i, r := range cols.Rows {
		out.Columns[i] = columnDetail{Column: pgdb.Str(r[0]), DataType: pgdb.Str(r[1]), IsNullable: pgdb.Str(r[2]), Default: r[3]}
	}

	// Unlike postgres-mcp, also join on table_name: constraint names are only
	// unique per table, so same-named constraints elsewhere would leak in.
	cons, err := s.db.Query(ctx, `
		SELECT tc.constraint_name, tc.constraint_type, kcu.column_name
		FROM information_schema.table_constraints AS tc
		LEFT JOIN information_schema.key_column_usage AS kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		 AND tc.table_name = kcu.table_name
		WHERE tc.table_schema = $1 AND tc.table_name = $2`, in.SchemaName, in.ObjectName)
	if err != nil {
		return "", err
	}
	out.Constraints = []constraintDetail{}
	byName := map[string]int{}
	for _, r := range cons.Rows {
		name := pgdb.Str(r[0])
		i, ok := byName[name]
		if !ok {
			i = len(out.Constraints)
			byName[name] = i
			out.Constraints = append(out.Constraints, constraintDetail{Name: name, Type: pgdb.Str(r[1]), Columns: []string{}})
		}
		if col := pgdb.Str(r[2]); col != "" {
			out.Constraints[i].Columns = append(out.Constraints[i].Columns, col)
		}
	}

	idx, err := s.db.Query(ctx, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2`, in.SchemaName, in.ObjectName)
	if err != nil {
		return "", err
	}
	out.Indexes = make([]indexDetail, len(idx.Rows))
	for i, r := range idx.Rows {
		out.Indexes[i] = indexDetail{Name: pgdb.Str(r[0]), Definition: pgdb.Str(r[1])}
	}
	return toJSON(out)
}
