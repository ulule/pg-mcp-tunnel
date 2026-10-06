package pgdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Result is the last result set of a query. Values are decoded from the
// text wire format: integers as int64, floats as float64, numeric as
// json.Number, bool as bool, json/jsonb as json.RawMessage, NULL as nil and
// everything else as the server's text representation.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Row returns row i as a column-name keyed map.
func (r *Result) Row(i int) map[string]any {
	m := make(map[string]any, len(r.Columns))
	for j, c := range r.Columns {
		m[c] = r.Rows[i][j]
	}
	return m
}

// Maps returns every row as a column-name keyed map.
func (r *Result) Maps() []map[string]any {
	out := make([]map[string]any, len(r.Rows))
	for i := range r.Rows {
		out[i] = r.Row(i)
	}
	return out
}

// Empty reports whether the result has no rows (or no result set at all).
func (r *Result) Empty() bool { return r == nil || len(r.Rows) == 0 }

// MarshalJSON encodes the rows as an array of objects whose keys keep the
// column order of the query.
func (r *Result) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, row := range r.Rows {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('{')
		for j, c := range r.Columns {
			if j > 0 {
				buf.WriteByte(',')
			}
			k, _ := JSON(c)
			buf.Write(k)
			buf.WriteByte(':')
			v, err := JSON(row[j])
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", c, err)
			}
			buf.Write(v)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// JSON is json.Marshal without HTML escaping, so SQL like "a < b" or
// "<insufficient privilege>" stays readable for the model.
func JSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func newResult(fds []pgconn.FieldDescription, rows [][][]byte) *Result {
	res := &Result{Columns: make([]string, len(fds)), Rows: make([][]any, len(rows))}
	for i, fd := range fds {
		res.Columns[i] = fd.Name
	}
	for i, raw := range rows {
		row := make([]any, len(raw))
		for j, v := range raw {
			row[j] = decodeText(fds[j].DataTypeOID, v)
		}
		res.Rows[i] = row
	}
	return res
}

func decodeText(oid uint32, v []byte) any {
	if v == nil {
		return nil
	}
	s := string(v)
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.OIDOID, pgtype.XIDOID:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case pgtype.Float4OID, pgtype.Float8OID:
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	case pgtype.NumericOID:
		if json.Valid(v) {
			return json.Number(s)
		}
	case pgtype.BoolOID:
		return s == "t"
	case pgtype.JSONOID, pgtype.JSONBOID:
		if json.Valid(v) {
			return json.RawMessage(append([]byte(nil), v...))
		}
	}
	return s
}

// Int returns v as an int64, or 0 if it is NULL or not numeric.
func Int(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		f, _ := t.Float64()
		return int64(f)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// Float returns v as a float64, or 0 if it is NULL or not numeric.
func Float(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// Str returns v as a string ("" for NULL).
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.RawMessage:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

// Bool returns v as a bool (false for NULL).
func Bool(v any) bool {
	b, _ := v.(bool)
	return b
}
