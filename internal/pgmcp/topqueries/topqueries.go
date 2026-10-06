package topqueries

import (
	"context"
	"errors"
	"fmt"

	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp/pgdb"
)

const installPgStatStatementsMessage = "The pg_stat_statements extension is required to report slow queries, but it is not currently installed.\n\n" +
	"You can install it by running: `CREATE EXTENSION pg_stat_statements;`\n\n" +
	"**What does it do?** It records statistics (like execution time, number of calls, rows returned) for every query executed against the database.\n\n" +
	"**Is it safe?** Installing 'pg_stat_statements' is generally safe and a standard practice for performance monitoring. It adds overhead by tracking statistics, but this is usually negligible unless under extreme load."

func Run(ctx context.Context, q pgdb.Querier, sortBy string, limit int) (string, error) {
	if sortBy != "resources" && sortBy != "mean_time" && sortBy != "total_time" {
		return "", errors.New("Invalid sort criteria. Please use 'resources' or 'mean_time' or 'total_time'.")
	}

	ext, err := pgdb.CheckExtension(ctx, q, "pg_stat_statements")
	if err != nil {
		return "", err
	}

	if !ext.Installed {
		return installPgStatStatementsMessage, nil
	}

	pgVersion, err := pgdb.ServerMajorVersion(ctx, q)
	if err != nil {
		return "", err
	}

	totalTimeCol := "total_time"
	meanTimeCol := "mean_time"
	if pgVersion >= 13 {
		totalTimeCol = "total_exec_time"
		meanTimeCol = "mean_exec_time"
	}

	if sortBy == "resources" {
		if pgVersion < 13 {
			return "", errors.New("resource queries require PostgreSQL 13 or newer (columns wal_bytes and stddev_exec_time are missing in older versions)")
		}

		query := `
			WITH resource_fractions AS (
				SELECT
					query,
					calls,
					rows,
					total_exec_time,
					mean_exec_time,
					stddev_exec_time,
					shared_blks_hit,
					shared_blks_read,
					shared_blks_dirtied,
					wal_bytes,
					total_exec_time / NULLIF(SUM(total_exec_time) OVER (), 0) AS total_exec_time_frac,
					(shared_blks_hit + shared_blks_read) / NULLIF(SUM(shared_blks_hit + shared_blks_read) OVER (), 0) AS shared_blks_accessed_frac,
					shared_blks_read / NULLIF(SUM(shared_blks_read) OVER (), 0) AS shared_blks_read_frac,
					shared_blks_dirtied / NULLIF(SUM(shared_blks_dirtied) OVER (), 0) AS shared_blks_dirtied_frac,
					wal_bytes / NULLIF(SUM(wal_bytes) OVER (), 0) AS total_wal_bytes_frac
				FROM pg_stat_statements
			)
			SELECT
				query,
				calls,
				rows,
				total_exec_time,
				mean_exec_time,
				stddev_exec_time,
				total_exec_time_frac,
				shared_blks_accessed_frac,
				shared_blks_read_frac,
				shared_blks_dirtied_frac,
				total_wal_bytes_frac,
				shared_blks_hit,
				shared_blks_read,
				shared_blks_dirtied,
				wal_bytes
			FROM resource_fractions
			WHERE
				total_exec_time_frac > 0.05
				OR shared_blks_accessed_frac > 0.05
				OR shared_blks_read_frac > 0.05
				OR shared_blks_dirtied_frac > 0.05
				OR total_wal_bytes_frac > 0.05
			ORDER BY total_exec_time DESC
		`

		res, err := q.Query(ctx, query)
		if err != nil {
			return "", err
		}

		b, err := pgdb.JSON(res)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	orderByColumn := meanTimeCol
	if sortBy == "total_time" {
		orderByColumn = totalTimeCol
	}

	query := fmt.Sprintf(`
		SELECT
			query,
			calls,
			%s,
			%s,
			rows
		FROM pg_stat_statements
		ORDER BY %s DESC
		LIMIT $1;
	`, totalTimeCol, meanTimeCol, orderByColumn)

	res, err := q.Query(ctx, query, limit)
	if err != nil {
		return "", err
	}

	b, err := pgdb.JSON(res)
	if err != nil {
		return "", err
	}

	criteria := "mean execution time per call"
	if sortBy == "total_time" {
		criteria = "total execution time"
	}
	return fmt.Sprintf("Top %d slowest queries by %s:\n%s", len(res.Rows), criteria, b), nil
}
