package pgdb

import (
	"context"
	"fmt"
)

// ServerVersionNum returns server_version_num (e.g. 170002).
func ServerVersionNum(ctx context.Context, q Querier) (int, error) {
	res, err := q.Query(ctx, "SHOW server_version_num")
	if err != nil {
		return 0, err
	}
	if res.Empty() {
		return 0, fmt.Errorf("empty server_version_num")
	}
	return int(Int(res.Rows[0][0])), nil
}

// ServerMajorVersion returns the major version (e.g. 17).
func ServerMajorVersion(ctx context.Context, q Querier) (int, error) {
	n, err := ServerVersionNum(ctx, q)
	return n / 10000, err
}

// Extension describes an extension's state in the current database.
type Extension struct {
	Name             string
	Installed        bool
	InstalledVersion string
	Available        bool
	DefaultVersion   string
}

// CheckExtension reports whether name is installed and/or available.
func CheckExtension(ctx context.Context, q Querier, name string) (Extension, error) {
	ext := Extension{Name: name}
	res, err := q.Query(ctx, `
		SELECT
			(SELECT extversion FROM pg_extension WHERE extname = $1) AS installed_version,
			(SELECT default_version FROM pg_available_extensions WHERE name = $1) AS default_version`, name)
	if err != nil {
		return ext, err
	}
	if !res.Empty() {
		row := res.Rows[0]
		ext.Installed = row[0] != nil
		ext.InstalledVersion = Str(row[0])
		ext.Available = row[1] != nil
		ext.DefaultVersion = Str(row[1])
	}
	return ext, nil
}
