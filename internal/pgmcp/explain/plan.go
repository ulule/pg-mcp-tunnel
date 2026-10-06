package explain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PlanNode is one node of EXPLAIN (FORMAT JSON). Counts are json.Number
// because PostgreSQL 18 reports fractional "Actual Rows" (e.g. 1.00).
type PlanNode struct {
	NodeType            string       `json:"Node Type"`
	StartupCost         float64      `json:"Startup Cost"`
	TotalCost           float64      `json:"Total Cost"`
	PlanRows            json.Number  `json:"Plan Rows"`
	PlanWidth           json.Number  `json:"Plan Width"`
	ActualStartupTime   *float64     `json:"Actual Startup Time,omitempty"`
	ActualTotalTime     *float64     `json:"Actual Total Time,omitempty"`
	ActualRows          *json.Number `json:"Actual Rows,omitempty"`
	ActualLoops         *json.Number `json:"Actual Loops,omitempty"`
	RelationName        *string      `json:"Relation Name,omitempty"`
	Alias               *string      `json:"Alias,omitempty"`
	IndexName           *string      `json:"Index Name,omitempty"`
	Filter              *string      `json:"Filter,omitempty"`
	IndexCond           *string      `json:"Index Cond,omitempty"`
	JoinType            *string      `json:"Join Type,omitempty"`
	HashCond            *string      `json:"Hash Cond,omitempty"`
	MergeCond           *string      `json:"Merge Cond,omitempty"`
	RowsRemovedByFilter *json.Number `json:"Rows Removed by Filter,omitempty"`
	SharedHitBlocks     *json.Number `json:"Shared Hit Blocks,omitempty"`
	SharedReadBlocks    *json.Number `json:"Shared Read Blocks,omitempty"`
	SharedWrittenBlocks *json.Number `json:"Shared Written Blocks,omitempty"`
	Plans               []PlanNode   `json:"Plans,omitempty"`
}

type explainResult struct {
	Plan          PlanNode `json:"Plan"`
	PlanningTime  *float64 `json:"Planning Time,omitempty"`
	ExecutionTime *float64 `json:"Execution Time,omitempty"`
}

func FormatPlan(explainJSON []byte) (string, error) {
	var results []explainResult
	if err := json.Unmarshal(explainJSON, &results); err != nil {
		return "", err
	}

	if len(results) == 0 {
		return "", fmt.Errorf("invalid explain plan format")
	}

	artifact := results[0]
	var result []string

	if artifact.PlanningTime != nil {
		result = append(result, fmt.Sprintf("Planning Time: %.3f ms", *artifact.PlanningTime))
	}
	if artifact.ExecutionTime != nil {
		result = append(result, fmt.Sprintf("Execution Time: %.3f ms", *artifact.ExecutionTime))
	}

	result = append(result, formatPlanNode(&artifact.Plan, 0))

	return strings.Join(result, "\n"), nil
}

func formatPlanNode(node *PlanNode, level int) string {
	indent := strings.Repeat("  ", level)
	output := fmt.Sprintf("%s→ %s (Cost: %.2f..%.2f)", indent, node.NodeType, node.StartupCost, node.TotalCost)

	if node.RelationName != nil {
		output += fmt.Sprintf(" on %s", *node.RelationName)
	}

	output += fmt.Sprintf(" [Rows: %s]", node.PlanRows)

	if node.ActualTotalTime != nil {
		var startupTime float64
		if node.ActualStartupTime != nil {
			startupTime = *node.ActualStartupTime
		}
		rows, loops := json.Number("0"), json.Number("0")
		if node.ActualRows != nil {
			rows = *node.ActualRows
		}
		if node.ActualLoops != nil {
			loops = *node.ActualLoops
		}
		output += fmt.Sprintf(" [Actual: %.2f..%.2f ms, Rows: %s, Loops: %s]", startupTime, *node.ActualTotalTime, rows, loops)
	}

	if node.Filter != nil {
		filterText := *node.Filter
		runes := []rune(filterText)
		if len(runes) > 100 {
			filterText = string(runes[:97]) + "..."
		}
		output += fmt.Sprintf("\n%s  Filter: %s", indent, filterText)
	}

	if node.SharedHitBlocks != nil {
		readBlocks, writtenBlocks := json.Number("0"), json.Number("0")
		if node.SharedReadBlocks != nil {
			readBlocks = *node.SharedReadBlocks
		}
		if node.SharedWrittenBlocks != nil {
			writtenBlocks = *node.SharedWrittenBlocks
		}
		output += fmt.Sprintf("\n%s  Buffers - hit: %s, read: %s, written: %s", indent, *node.SharedHitBlocks, readBlocks, writtenBlocks)
	}

	for i := range node.Plans {
		output += "\n" + formatPlanNode(&node.Plans[i], level+1)
	}

	return output
}
