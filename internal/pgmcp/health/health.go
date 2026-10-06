package health

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
)

func formatThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	var buf []byte
	for i := 0; i < len(s); i++ {
		if i > start && (len(s)-i)%3 == 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, s[i])
	}
	return string(buf)
}

func Run(ctx context.Context, q pgdb.Querier, healthType string) string {
	types := strings.Split(healthType, ",")
	selectedMap := make(map[string]bool)
	hasAll := false

	validTypes := map[string]bool{
		"index":       true,
		"connection":  true,
		"vacuum":      true,
		"sequence":    true,
		"replication": true,
		"buffer":      true,
		"constraint":  true,
		"all":         true,
	}

	for _, t := range types {
		t = strings.TrimSpace(t)
		if !validTypes[t] {
			return fmt.Sprintf("Invalid health types provided: '%s'. Valid values are: all, buffer, connection, constraint, index, replication, sequence, vacuum. Please try again with a comma-separated list of valid health types.", healthType)
		}
		if t == "all" {
			hasAll = true
		}
		selectedMap[t] = true
	}

	if hasAll {
		selectedMap = map[string]bool{
			"index":       true,
			"connection":  true,
			"vacuum":      true,
			"sequence":    true,
			"replication": true,
			"buffer":      true,
			"constraint":  true,
		}
	}

	var res strings.Builder

	runCheck := func(prefix string, check func(context.Context, pgdb.Querier) (string, error)) {
		val, err := check(ctx, q)
		if err != nil {
			res.WriteString(fmt.Sprintf("%sError: %v\n", prefix, err))
		} else {
			res.WriteString(fmt.Sprintf("%s%s\n", prefix, val))
		}
	}

	if selectedMap["index"] {
		runCheck("Invalid index check: ", invalidIndexCheck)
		runCheck("Duplicate index check: ", duplicateIndexCheck)
		runCheck("Index bloat: ", indexBloatCheck)
		runCheck("Unused index check: ", unusedIndexCheck)
	}

	if selectedMap["connection"] {
		runCheck("Connection health: ", connectionHealthCheck)
	}

	if selectedMap["vacuum"] {
		runCheck("Vacuum health: ", vacuumHealthCheck)
	}

	if selectedMap["sequence"] {
		runCheck("Sequence health: ", sequenceHealthCheck)
	}

	if selectedMap["replication"] {
		runCheck("Replication health: ", replicationHealthCheck)
	}

	if selectedMap["buffer"] {
		runCheck("Buffer health for indexes: ", bufferHealthIndexes)
		runCheck("Buffer health for tables: ", bufferHealthTables)
	}

	if selectedMap["constraint"] {
		runCheck("Constraint health: ", constraintHealthCheck)
	}

	if res.Len() == 0 {
		return "No health checks were performed."
	}
	return res.String()
}

func fetchIndexes(ctx context.Context, q pgdb.Querier) ([]map[string]any, error) {
	query := `SELECT
                schemaname AS schema,
                t.relname AS table,
                ix.relname AS name,
                regexp_replace(pg_get_indexdef(i.indexrelid), '^[^\(]*\((.*)\)$', '\1') AS columns,
                regexp_replace(pg_get_indexdef(i.indexrelid), '.* USING ([^ ]*) \(.*', '\1') AS using,
                indisunique AS unique,
                indisprimary AS primary,
                indisvalid AS valid,
                indexprs::text,
                indpred::text,
                pg_get_indexdef(i.indexrelid) AS definition
            FROM
                pg_index i
            INNER JOIN
                pg_class t ON t.oid = i.indrelid
            INNER JOIN
                pg_class ix ON ix.oid = i.indexrelid
            LEFT JOIN
                pg_stat_user_indexes ui ON ui.indexrelid = i.indexrelid
            WHERE
                schemaname IS NOT NULL
            ORDER BY
                1, 2`
	res, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	indexes := res.Maps()
	for _, idx := range indexes {
		colsStr := pgdb.Str(idx["columns"])
		colsStr = strings.ReplaceAll(colsStr, ") WHERE (", " WHERE ")
		cols := strings.Split(colsStr, ", ")
		for i := range cols {
			cols[i] = strings.Trim(cols[i], "\"")
		}
		idx["columns_arr"] = cols
	}
	return indexes, nil
}

func invalidIndexCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	indexes, err := fetchIndexes(ctx, q)
	if err != nil {
		return "", err
	}
	var invalid []string
	for _, idx := range indexes {
		if !pgdb.Bool(idx["valid"]) {
			invalid = append(invalid, fmt.Sprintf("%s on %s is invalid.", pgdb.Str(idx["name"]), pgdb.Str(idx["table"])))
		}
	}
	if len(invalid) == 0 {
		return "No invalid indexes found.", nil
	}
	return "Invalid indexes found: \n" + strings.Join(invalid, "\n"), nil
}

func indexCovers(indexedCols, cols []string) bool {
	if len(indexedCols) < len(cols) {
		return false
	}
	for i := range cols {
		if indexedCols[i] != cols[i] {
			return false
		}
	}
	return true
}

func duplicateIndexCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	indexes, err := fetchIndexes(ctx, q)
	if err != nil {
		return "", err
	}

	byTable := make(map[string][]map[string]any)
	for _, idx := range indexes {
		key := pgdb.Str(idx["schema"]) + "." + pgdb.Str(idx["table"])
		byTable[key] = append(byTable[key], idx)
	}

	var dups []map[string]map[string]any

	for _, idx := range indexes {
		if pgdb.Bool(idx["valid"]) && !pgdb.Bool(idx["primary"]) && !pgdb.Bool(idx["unique"]) {
			key := pgdb.Str(idx["schema"]) + "." + pgdb.Str(idx["table"])
			tableIndexes := byTable[key]

			for _, covIdx := range tableIndexes {
				idxCols := idx["columns_arr"].([]string)
				covCols := covIdx["columns_arr"].([]string)

				if pgdb.Bool(covIdx["valid"]) && pgdb.Str(covIdx["name"]) != pgdb.Str(idx["name"]) &&
					indexCovers(covCols, idxCols) &&
					pgdb.Str(covIdx["using"]) == pgdb.Str(idx["using"]) &&
					pgdb.Str(covIdx["indexprs"]) == pgdb.Str(idx["indexprs"]) &&
					pgdb.Str(covIdx["indpred"]) == pgdb.Str(idx["indpred"]) {

					colsDiff := false
					if len(covCols) != len(idxCols) {
						colsDiff = true
					} else {
						for i := range covCols {
							if covCols[i] != idxCols[i] {
								colsDiff = true
								break
							}
						}
					}

					if colsDiff || pgdb.Str(idx["name"]) > pgdb.Str(covIdx["name"]) ||
						pgdb.Bool(covIdx["primary"]) || pgdb.Bool(covIdx["unique"]) {
						dups = append(dups, map[string]map[string]any{
							"unneeded": idx,
							"covering": covIdx,
						})
						break
					}
				}
			}
		}
	}

	if len(dups) == 0 {
		return "No duplicate indexes found.", nil
	}

	sort.Slice(dups, func(i, j int) bool {
		t1 := pgdb.Str(dups[i]["unneeded"]["table"])
		t2 := pgdb.Str(dups[j]["unneeded"]["table"])
		if t1 != t2 {
			return t1 < t2
		}
		c1 := dups[i]["unneeded"]["columns_arr"].([]string)
		c2 := dups[j]["unneeded"]["columns_arr"].([]string)
		minLen := len(c1)
		if len(c2) < minLen {
			minLen = len(c2)
		}
		for k := 0; k < minLen; k++ {
			if c1[k] != c2[k] {
				return c1[k] < c2[k]
			}
		}
		return len(c1) < len(c2)
	})

	result := []string{"Duplicate indexes found:"}
	for _, dup := range dups {
		result = append(result, fmt.Sprintf("Index '%s' on table '%s' is covered by index '%s'",
			pgdb.Str(dup["unneeded"]["name"]), pgdb.Str(dup["unneeded"]["table"]), pgdb.Str(dup["covering"]["name"])))
	}
	return strings.Join(result, "\n"), nil
}

func indexBloatCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	minSize := 104857600
	query := `WITH btree_index_atts AS (
                SELECT
                    nspname, relname, reltuples, relpages, indrelid, relam,
                    regexp_split_to_table(indkey::text, ' ')::smallint AS attnum,
                    indexrelid as index_oid
                FROM
                    pg_index
                JOIN
                    pg_class ON pg_class.oid = pg_index.indexrelid
                JOIN
                    pg_namespace ON pg_namespace.oid = pg_class.relnamespace
                JOIN
                    pg_am ON pg_class.relam = pg_am.oid
                WHERE
                    pg_am.amname = 'btree'
            ),
            index_item_sizes AS (
                SELECT
                    i.nspname,
                    i.relname,
                    i.reltuples,
                    i.relpages,
                    i.relam,
                    (quote_ident(s.schemaname) || '.' || quote_ident(s.tablename))::regclass AS starelid,
                    a.attrelid AS table_oid, index_oid,
                    current_setting('block_size')::numeric AS bs,
                    CASE
                        WHEN version() ~ 'mingw32' OR version() ~ '64-bit' THEN 8
                        ELSE 4
                    END AS maxalign,
                    24 AS pagehdr,
                    CASE WHEN max(coalesce(s.null_frac,0)) = 0
                        THEN 2
                        ELSE 6
                    END AS index_tuple_hdr,
                    sum( (1-coalesce(s.null_frac, 0)) * coalesce(s.avg_width, 2048) ) AS nulldatawidth
                FROM
                    pg_attribute AS a
                JOIN
                    pg_stats AS s ON (quote_ident(s.schemaname) || '.' || quote_ident(s.tablename))::regclass=a.attrelid AND s.attname = a.attname
                JOIN
                    btree_index_atts AS i ON i.indrelid = a.attrelid AND a.attnum = i.attnum
                WHERE
                    a.attnum > 0
                GROUP BY
                    1, 2, 3, 4, 5, 6, 7, 8, 9
            ),
            index_aligned AS (
                SELECT
                    maxalign,
                    bs,
                    nspname,
                    relname AS index_name,
                    reltuples,
                    relpages,
                    relam,
                    table_oid,
                    index_oid,
                    ( 2 +
                        maxalign - CASE
                            WHEN index_tuple_hdr%maxalign = 0 THEN maxalign
                            ELSE index_tuple_hdr%maxalign
                        END
                    + nulldatawidth + maxalign - CASE
                            WHEN nulldatawidth::integer%maxalign = 0 THEN maxalign
                            ELSE nulldatawidth::integer%maxalign
                        END
                    )::numeric AS nulldatahdrwidth, pagehdr
                FROM
                    index_item_sizes AS s1
            ),
            otta_calc AS (
                SELECT
                    bs,
                    nspname,
                    table_oid,
                    index_oid,
                    index_name,
                    relpages,
                    coalesce(
                        ceil((reltuples*(4+nulldatahdrwidth))/(bs-pagehdr::float)) +
                        CASE WHEN am.amname IN ('hash','btree') THEN 1 ELSE 0 END , 0
                    ) AS otta
                FROM
                    index_aligned AS s2
                LEFT JOIN
                    pg_am am ON s2.relam = am.oid
            ),
            raw_bloat AS (
                SELECT
                    nspname,
                    c.relname AS table_name,
                    index_name,
                    bs*(sub.relpages)::bigint AS totalbytes,
                    CASE
                        WHEN sub.relpages <= otta THEN 0
                        ELSE bs*(sub.relpages-otta)::bigint END
                        AS wastedbytes,
                    CASE
                        WHEN sub.relpages <= otta
                        THEN 0 ELSE bs*(sub.relpages-otta)::bigint * 100 / (bs*(sub.relpages)::bigint) END
                        AS realbloat,
                    pg_relation_size(sub.table_oid) as table_bytes,
                    stat.idx_scan as index_scans,
                    stat.indexrelid
                FROM
                    otta_calc AS sub
                JOIN
                    pg_class AS c ON c.oid=sub.table_oid
                JOIN
                    pg_stat_user_indexes AS stat ON sub.index_oid = stat.indexrelid
            )
            SELECT
                nspname AS schema,
                table_name AS table,
                index_name AS index,
                wastedbytes AS bloat_bytes,
                totalbytes AS index_bytes,
                pg_get_indexdef(rb.indexrelid) AS definition,
                indisprimary AS primary
            FROM
                raw_bloat rb
            INNER JOIN
                pg_index i ON i.indexrelid = rb.indexrelid
            WHERE
                wastedbytes >= $1
            ORDER BY
                wastedbytes DESC,
                index_name`
	res, err := q.Query(ctx, query, minSize)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No bloated indexes found.", nil
	}

	result := []string{"Bloated indexes found:"}
	for _, row := range res.Maps() {
		bloatMb := float64(pgdb.Int(row["bloat_bytes"])) / (1024 * 1024)
		totalMb := float64(pgdb.Int(row["index_bytes"])) / (1024 * 1024)
		result = append(result, fmt.Sprintf("Index '%s' on table '%s' has %.1fMB bloat out of %.1fMB total size",
			pgdb.Str(row["index"]), pgdb.Str(row["table"]), bloatMb, totalMb))
	}
	return strings.Join(result, "\n"), nil
}

func unusedIndexCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	maxScans := 50
	query := `SELECT
                schemaname AS schema,
                relname AS table,
                indexrelname AS index,
                pg_relation_size(i.indexrelid) AS size_bytes,
                idx_scan as index_scans,
                pg_get_indexdef(i.indexrelid) AS definition,
                indisprimary AS primary
            FROM
                pg_stat_user_indexes ui
            INNER JOIN
                pg_index i ON ui.indexrelid = i.indexrelid
            WHERE
                NOT indisunique
                AND idx_scan <= $1
            ORDER BY
                pg_relation_size(i.indexrelid) DESC,
                relname ASC`
	res, err := q.Query(ctx, query, maxScans)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No unused indexes found.", nil
	}

	result := []string{"Rarely used indexes found:"}
	for _, row := range res.Maps() {
		if pgdb.Bool(row["primary"]) {
			continue
		}
		sizeMb := float64(pgdb.Int(row["size_bytes"])) / (1024 * 1024)
		result = append(result, fmt.Sprintf("Index '%s' on table '%s' has only been scanned %d times and uses %.1fMB of space",
			pgdb.Str(row["index"]), pgdb.Str(row["table"]), pgdb.Int(row["index_scans"]), sizeMb))
	}
	if len(result) == 1 {
		return "No unused indexes found.", nil
	}
	return strings.Join(result, "\n"), nil
}

func connectionHealthCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	maxTotal := int64(500)
	maxIdle := int64(100)

	totalRes, err := q.Query(ctx, "SELECT COUNT(*) as count FROM pg_stat_activity")
	if err != nil {
		return "", err
	}
	total := pgdb.Int(totalRes.Row(0)["count"])

	idleRes, err := q.Query(ctx, "SELECT COUNT(*) as count FROM pg_stat_activity WHERE state = 'idle in transaction'")
	if err != nil {
		return "", err
	}
	idle := pgdb.Int(idleRes.Row(0)["count"])

	if total > maxTotal {
		return fmt.Sprintf("High number of connections: %d", total), nil
	} else if idle > maxIdle {
		return fmt.Sprintf("High number of connections idle in transaction: %d", idle), nil
	}
	return fmt.Sprintf("Connections healthy: %d total, %d idle", total, idle), nil
}

func vacuumHealthCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	threshold := int64(10000000)
	maxValue := int64(2146483648)

	query := `SELECT
                n.nspname AS schema,
                c.relname AS table,
                $1 - GREATEST(AGE(c.relfrozenxid), AGE(t.relfrozenxid)) AS transactions_left
            FROM
                pg_class c
            INNER JOIN
                pg_catalog.pg_namespace n ON n.oid = c.relnamespace
            LEFT JOIN
                pg_class t ON c.reltoastrelid = t.oid
            WHERE
                c.relkind = 'r'
                AND ($2 - GREATEST(AGE(c.relfrozenxid), AGE(t.relfrozenxid))) < $3
            ORDER BY
                3, 1, 2`
	res, err := q.Query(ctx, query, maxValue, maxValue, threshold)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No tables found with transaction ID wraparound danger.", nil
	}

	type metric struct {
		schema string
		table  string
		left   int64
	}
	var metrics []metric
	unhealthy := false
	for _, row := range res.Maps() {
		left := pgdb.Int(row["transactions_left"])
		metrics = append(metrics, metric{
			schema: pgdb.Str(row["schema"]),
			table:  pgdb.Str(row["table"]),
			left:   left,
		})
		if left < threshold {
			unhealthy = true
		}
	}
	if !unhealthy {
		return "All tables have healthy transaction ID age.", nil
	}

	sort.SliceStable(metrics, func(i, j int) bool {
		return metrics[i].left < metrics[j].left
	})

	result := []string{"Tables approaching transaction ID wraparound:"}
	for _, m := range metrics {
		if m.left < threshold {
			result = append(result, fmt.Sprintf("Table '%s.%s' has %s transactions remaining before wraparound (threshold: %s)",
				m.schema, m.table, formatThousands(m.left), formatThousands(threshold)))
		}
	}
	return strings.Join(result, "\n"), nil
}

func sequenceHealthCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	threshold := 0.9

	query := `WITH seqs AS (
    SELECT
        n.nspname AS table_schema,
        c.relname AS table_name,
        a.attname AS column_name,
        format_type(a.atttypid, a.atttypmod) AS column_type,
        pg_get_expr(d.adbin, d.adrelid) AS default_value,
        replace(replace(replace(replace(pg_get_expr(d.adbin, d.adrelid), 'nextval(''', ''), '''::regclass)', ''), '(''', ''), '''::text)', '') as clean_val
    FROM
        pg_catalog.pg_attribute a
    INNER JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
    INNER JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    INNER JOIN pg_catalog.pg_attrdef d ON (a.attrelid, a.attnum) = (d.adrelid, d.adnum)
    WHERE NOT a.attisdropped AND a.attnum > 0 AND pg_get_expr(d.adbin, d.adrelid) LIKE 'nextval%' AND n.nspname NOT LIKE 'pg\_temp\_%'
)
SELECT
    table_schema,
    table_name,
    column_name,
    column_type,
    default_value,
    CASE WHEN position('.' in clean_val) > 0 THEN split_part(clean_val, '.', 1) ELSE 'public' END as seq_schema,
    CASE WHEN position('.' in clean_val) > 0 THEN split_part(clean_val, '.', 2) ELSE clean_val END as seq_name,
    s.last_value
FROM seqs
LEFT JOIN pg_sequences s 
  ON s.schemaname = CASE WHEN position('.' in clean_val) > 0 THEN split_part(clean_val, '.', 1) ELSE 'public' END
  AND s.sequencename = CASE WHEN position('.' in clean_val) > 0 THEN split_part(clean_val, '.', 2) ELSE clean_val END`

	res, err := q.Query(ctx, query)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No sequences found in the database.", nil
	}

	type metric struct {
		schema  string
		table   string
		column  string
		seq     string
		last    int64
		max     int64
		pct     float64
		healthy bool
		remain  int64
	}
	var metrics []metric
	unhealthy := false

	for _, row := range res.Maps() {
		// pg_sequences.last_value is NULL for a never-used sequence (or
		// without privilege): count it as 0, i.e. healthy.
		schema := pgdb.Str(row["seq_schema"])
		seq := pgdb.Str(row["seq_name"])
		if seq == "" {
			continue
		}
		colType := pgdb.Str(row["column_type"])
		maxVal := int64(9223372036854775807)
		if colType == "integer" {
			maxVal = 2147483647
		}
		last := pgdb.Int(row["last_value"])
		healthy := float64(last)/float64(maxVal) <= threshold
		if !healthy {
			unhealthy = true
		}
		metrics = append(metrics, metric{
			schema:  schema,
			table:   pgdb.Str(row["table_name"]),
			column:  pgdb.Str(row["column_name"]),
			seq:     seq,
			last:    last,
			max:     maxVal,
			pct:     (float64(last) / float64(maxVal)) * 100,
			healthy: healthy,
			remain:  maxVal - last,
		})
	}

	if len(metrics) == 0 {
		return "No sequences found in the database.", nil
	}

	if !unhealthy {
		return "All sequences have healthy usage levels.", nil
	}

	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].remain < metrics[j].remain
	})

	result := []string{"Sequences approaching maximum value:"}
	for _, m := range metrics {
		if !m.healthy {
			result = append(result, fmt.Sprintf("Sequence '%s.%s' used for %s.%s has used %.1f%% of available values (%s of %s, %s remaining)",
				m.schema, m.seq, m.table, m.column, m.pct, formatThousands(m.last), formatThousands(m.max), formatThousands(m.remain)))
		}
	}
	return strings.Join(result, "\n"), nil
}

func replicationHealthCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	isReplicaRes, err := q.Query(ctx, "SELECT pg_is_in_recovery()")
	if err != nil {
		return "", err
	}
	isReplica := pgdb.Bool(isReplicaRes.Row(0)["pg_is_in_recovery"])

	version, _ := pgdb.ServerVersionNum(ctx, q)

	lagCondition := "pg_last_xlog_receive_location() = pg_last_xlog_replay_location()"
	if version >= 100000 {
		lagCondition = "pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn()"
	}

	var lagSecs *float64
	lagQuery := fmt.Sprintf(`SELECT
                    CASE
                        WHEN NOT pg_is_in_recovery() OR %s THEN 0
                        ELSE EXTRACT (EPOCH FROM NOW() - pg_last_xact_replay_timestamp())
                    END
                AS replication_lag`, lagCondition)
	lagRes, err := q.Query(ctx, lagQuery)
	if err == nil && !lagRes.Empty() {
		if lagRes.Row(0)["replication_lag"] != nil {
			v := pgdb.Float(lagRes.Row(0)["replication_lag"])
			lagSecs = &v
		}
	}

	isReplicating := false
	replRes, err := q.Query(ctx, "SELECT state FROM pg_stat_replication")
	if err == nil {
		isReplicating = !replRes.Empty()
	}

	var slots []map[string]any
	if version >= 90400 {
		slotsRes, err := q.Query(ctx, "SELECT slot_name, database, active FROM pg_replication_slots")
		if err == nil {
			slots = slotsRes.Maps()
		}
	}

	var result []string
	if isReplica {
		result = append(result, "This is a replica database.")
		if !isReplicating {
			result = append(result, "WARNING: Replica is not actively replicating from primary!")
		} else {
			result = append(result, "Replica is actively replicating from primary.")
		}
		if lagSecs != nil {
			if *lagSecs == 0 {
				result = append(result, "No replication lag detected.")
			} else {
				result = append(result, fmt.Sprintf("Replication lag: %.1f seconds", *lagSecs))
			}
		}
	} else {
		result = append(result, "This is a primary database.")
		if isReplicating {
			result = append(result, "Has active replicas connected.")
		} else {
			result = append(result, "No active replicas connected.")
		}
	}

	if len(slots) > 0 {
		var activeSlots, inactiveSlots []map[string]any
		for _, s := range slots {
			if pgdb.Bool(s["active"]) {
				activeSlots = append(activeSlots, s)
			} else {
				inactiveSlots = append(inactiveSlots, s)
			}
		}
		if len(activeSlots) > 0 {
			result = append(result, "\nActive replication slots:")
			for _, s := range activeSlots {
				result = append(result, fmt.Sprintf("- %s (database: %s)", pgdb.Str(s["slot_name"]), pgdb.Str(s["database"])))
			}
		}
		if len(inactiveSlots) > 0 {
			result = append(result, "\nInactive replication slots:")
			for _, s := range inactiveSlots {
				result = append(result, fmt.Sprintf("- %s (database: %s)", pgdb.Str(s["slot_name"]), pgdb.Str(s["database"])))
			}
		}
	} else {
		result = append(result, "\nNo replication slots found.")
	}

	return strings.Join(result, "\n"), nil
}

func bufferHealthIndexes(ctx context.Context, q pgdb.Querier) (string, error) {
	query := `SELECT
                (sum(idx_blks_hit)) / nullif(sum(idx_blks_hit + idx_blks_read), 0) AS rate
            FROM
                pg_statio_user_indexes`
	res, err := q.Query(ctx, query)
	if err != nil {
		return "", err
	}
	if res.Empty() || res.Row(0)["rate"] == nil {
		return "No index cache statistics available.", nil
	}
	rate := pgdb.Float(res.Row(0)["rate"]) * 100
	threshold := 95.0
	if rate >= threshold {
		return fmt.Sprintf("Index cache hit rate: %.1f%% (above %.1f%% threshold)", rate, threshold), nil
	}
	return fmt.Sprintf("Index cache hit rate: %.1f%% (below %.1f%% threshold)", rate, threshold), nil
}

func bufferHealthTables(ctx context.Context, q pgdb.Querier) (string, error) {
	query := `SELECT
                sum(heap_blks_hit) / nullif(sum(heap_blks_hit + heap_blks_read), 0) AS rate
            FROM
                pg_statio_user_tables`
	res, err := q.Query(ctx, query)
	if err != nil {
		return "", err
	}
	if res.Empty() || res.Row(0)["rate"] == nil {
		return "No table cache statistics available.", nil
	}
	rate := pgdb.Float(res.Row(0)["rate"]) * 100
	threshold := 95.0
	if rate >= threshold {
		return fmt.Sprintf("Table cache hit rate: %.1f%% (above %.1f%% threshold)", rate, threshold), nil
	}
	return fmt.Sprintf("Table cache hit rate: %.1f%% (below %.1f%% threshold)", rate, threshold), nil
}

func constraintHealthCheck(ctx context.Context, q pgdb.Querier) (string, error) {
	query := `SELECT
                nsp.nspname AS schema,
                rel.relname AS table,
                con.conname AS name,
                fnsp.nspname AS referenced_schema,
                frel.relname AS referenced_table
            FROM
                pg_catalog.pg_constraint con
            INNER JOIN
                pg_catalog.pg_class rel ON rel.oid = con.conrelid
            LEFT JOIN
                pg_catalog.pg_class frel ON frel.oid = con.confrelid
            LEFT JOIN
                pg_catalog.pg_namespace nsp ON nsp.oid = con.connamespace
            LEFT JOIN
                pg_catalog.pg_namespace fnsp ON fnsp.oid = frel.relnamespace
            WHERE
                con.convalidated = 'f'`
	res, err := q.Query(ctx, query)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No invalid constraints found.", nil
	}
	result := []string{"Invalid constraints found:"}
	for _, row := range res.Maps() {
		refTable := pgdb.Str(row["referenced_table"])
		if refTable != "" {
			result = append(result, fmt.Sprintf("Constraint '%s' on table '%s.%s' referencing '%s.%s' is invalid",
				pgdb.Str(row["name"]), pgdb.Str(row["schema"]), pgdb.Str(row["table"]),
				pgdb.Str(row["referenced_schema"]), refTable))
		} else {
			result = append(result, fmt.Sprintf("Constraint '%s' on table '%s.%s' is invalid",
				pgdb.Str(row["name"]), pgdb.Str(row["schema"]), pgdb.Str(row["table"])))
		}
	}
	return strings.Join(result, "\n"), nil
}
