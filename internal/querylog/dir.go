package querylog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultDir returns the directory holding the events files,
// /tmp/pg-mcp-tunnel-<uid>, creating it if needed. /tmp is wiped at reboot so
// recordings start fresh, and unlike $TMPDIR the path is the same for
// Claude-spawned tunnels and a terminal running `pg-mcp-tunnel ui`.
func DefaultDir() (string, error) {
	dir := filepath.Join("/tmp", fmt.Sprintf("pg-mcp-tunnel-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	// /tmp is shared: refuse a directory or symlink planted by another user.
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory owned by the current user", dir)
	}
	return dir, nil
}
