package pgmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/explain"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/health"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/topqueries"
)

// Descriptions are copied from postgres-mcp's server.py.

type noInput struct{}

type listObjectsInput struct {
	SchemaName string `json:"schema_name" jsonschema:"Schema name"`
	ObjectType string `json:"object_type,omitempty" jsonschema:"Object type: 'table', 'view', 'sequence', or 'extension'"`
}

type objectDetailsInput struct {
	SchemaName string `json:"schema_name" jsonschema:"Schema name"`
	ObjectName string `json:"object_name" jsonschema:"Object name"`
	ObjectType string `json:"object_type,omitempty" jsonschema:"Object type: 'table', 'view', 'sequence', or 'extension'"`
}

type executeSQLInput struct {
	SQL string `json:"sql,omitempty" jsonschema:"SQL to run"`
}

type explainInput struct {
	SQL                 string                      `json:"sql" jsonschema:"SQL query to explain"`
	Analyze             bool                        `json:"analyze,omitempty" jsonschema:"When True, actually runs the query to show real execution statistics instead of estimates. Takes longer but provides more accurate information."`
	HypotheticalIndexes []explain.HypotheticalIndex `json:"hypothetical_indexes,omitempty"`
}

const hypotheticalIndexesDescription = `A list of hypothetical indexes to simulate. Each index must be a dictionary with these keys:
    - 'table': The table name to add the index to (e.g., 'users')
    - 'columns': List of column names to include in the index (e.g., ['email'] or ['last_name', 'first_name'])
    - 'using': Optional index method (default: 'btree', other options include 'hash', 'gist', etc.)

Examples: [
    {"table": "users", "columns": ["email"], "using": "btree"},
    {"table": "orders", "columns": ["user_id", "created_at"]}
]
If there is no hypothetical index, you can pass an empty list.`

type topQueriesInput struct {
	SortBy string `json:"sort_by,omitempty" jsonschema:"Ranking criteria: 'total_time' for total execution time or 'mean_time' for mean execution time per call, or 'resources' for resource-intensive queries"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Number of queries to return when ranking based on mean_time or total_time"`
}

type dbHealthInput struct {
	HealthType string `json:"health_type,omitempty" jsonschema:"Optional. Valid values are: all, buffer, connection, constraint, index, replication, sequence, vacuum."`
}

func (s *Server) registerTools() {
	restricted := s.db.Restricted()

	addTool(s, "list_schemas", "List all schemas in the database", true, nil, s.listSchemas)
	addTool(s, "list_objects", "List objects in a schema", true,
		map[string]any{"object_type": "table"}, s.listObjects)
	addTool(s, "get_object_details", "Show detailed information about a database object", true,
		map[string]any{"object_type": "table"}, s.getObjectDetails)

	execDesc := "Execute any SQL query"
	if restricted {
		execDesc = "Execute a read-only SQL query"
	}
	addTool(s, "execute_sql", execDesc, restricted,
		map[string]any{"sql": "all"}, s.executeSQL)

	addToolSchema(s, "explain_query",
		"Explains the execution plan for a SQL query, showing how the database will execute it and provides detailed cost estimates.",
		restricted, map[string]any{"analyze": false, "hypothetical_indexes": []any{}},
		func(schema *jsonschema.Schema) {
			schema.Properties["hypothetical_indexes"].Description = hypotheticalIndexesDescription
		},
		s.explainQuery)

	addTool(s, "analyze_db_health",
		"Analyzes database health. Here are the available health checks:\n"+
			"- index - checks for invalid, duplicate, and bloated indexes\n"+
			"- connection - checks the number of connection and their utilization\n"+
			"- vacuum - checks vacuum health for transaction id wraparound\n"+
			"- sequence - checks sequences at risk of exceeding their maximum value\n"+
			"- replication - checks replication health including lag and slots\n"+
			"- buffer - checks for buffer cache hit rates for indexes and tables\n"+
			"- constraint - checks for invalid constraints\n"+
			"- all - runs all checks\n"+
			"You can optionally specify a single health check or a comma-separated list of health checks. The default is 'all' checks.",
		true, map[string]any{"health_type": "all"}, s.analyzeDBHealth)

	addTool(s, "get_top_queries",
		"Reports the slowest or most resource-intensive queries using data from the 'pg_stat_statements' extension.",
		true, map[string]any{"sort_by": "resources", "limit": 10}, s.getTopQueries)
}

func addTool[In any](s *Server, name, desc string, readOnly bool, defaults map[string]any, h func(context.Context, In) (string, error)) {
	addToolSchema(s, name, desc, readOnly, defaults, nil, h)
}

// addToolSchema registers a tool whose input schema is inferred from In,
// with postgres-mcp's defaults injected (the SDK applies them before
// unmarshaling). Handler errors become "Error: ..." text results, as in
// postgres-mcp's format_error_response.
func addToolSchema[In any](s *Server, name, desc string, readOnly bool, defaults map[string]any, tweak func(*jsonschema.Schema), h func(context.Context, In) (string, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", name, err))
	}
	for prop, v := range defaults {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		schema.Properties[prop].Default = raw
	}
	if tweak != nil {
		tweak(schema)
	}
	tool := &mcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}
	mcp.AddTool(s.mcp, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		text, err := h(ctx, in)
		duration := time.Since(start)
		isError := err != nil
		if isError {
			text = "Error: " + err.Error()
			s.logger.Warn("tool call failed", "tool", name, "duration", duration, "err", err)
		} else {
			s.logger.Info("tool call", "tool", name, "duration", duration, "tokens", EstimateTokens(text))
		}
		if s.onToolCall != nil {
			args, _ := json.Marshal(in)
			s.onToolCall(ToolCall{Name: name, Args: args, Duration: duration, Output: text, IsError: isError})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}, nil, nil
	})
}

func toJSON(v any) (string, error) {
	b, err := pgdb.JSON(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) executeSQL(ctx context.Context, in executeSQLInput) (string, error) {
	if s.db.Restricted() {
		if err := s.validator.Validate(in.SQL); err != nil {
			return "", err
		}
	}
	res, err := s.db.Query(ctx, in.SQL)
	if err != nil {
		return "", err
	}
	if len(res.Columns) == 0 {
		return "No results", nil
	}
	return toJSON(res)
}

func (s *Server) explainQuery(ctx context.Context, in explainInput) (string, error) {
	return explain.Run(ctx, s.db, s.validator, in.SQL, in.Analyze, in.HypotheticalIndexes)
}

func (s *Server) analyzeDBHealth(ctx context.Context, in dbHealthInput) (string, error) {
	return health.Run(ctx, s.db, in.HealthType), nil
}

func (s *Server) getTopQueries(ctx context.Context, in topQueriesInput) (string, error) {
	return topqueries.Run(ctx, s.db, in.SortBy, in.Limit)
}
