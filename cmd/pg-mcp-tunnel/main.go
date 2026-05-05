package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ulule/pg-mcp-tunnel/internal/config"
	"github.com/ulule/pg-mcp-tunnel/internal/proxy"
	"github.com/ulule/pg-mcp-tunnel/internal/sshtunnel"
)

const (
	maxLogSize = 10 * 1024 * 1024 // 10 MiB
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pg-mcp-tunnel:", err)
		os.Exit(exitCode(err))
	}
}

func run() error {
	var (
		target     = flag.String("target", "", "target name from config (required)")
		configPath = flag.String("config", defaultConfigPath(), "path to config.yaml")
		logPath    = flag.String("log-file", "", "path to log file (default ~/.cache/pg-mcp-tunnel/<target>.log)")
	)
	flag.Parse()

	if *target == "" {
		flag.Usage()
		return errors.New("--target is required")
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		return err
	}
	tg, ok := cfg.Targets[*target]
	if !ok {
		return fmt.Errorf("target %q not found in %s", *target, *configPath)
	}

	if _, err := exec.LookPath("uv"); err != nil {
		return fmt.Errorf("`uv` not found in PATH (install via https://docs.astral.sh/uv/): %w", err)
	}

	logFile, err := openLogFile(*logPath, *target)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("starting pg-mcp-tunnel", "target", *target, "config", *configPath)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var (
		uri       string
		tunnelRef *sshtunnel.Tunnel
	)
	if tg.SSH != nil {
		t, err := sshtunnel.Open(ctx, sshtunnel.Config{
			Host: tg.SSH.Host,
			User: tg.SSH.User,
			Port: tg.SSH.Port,
		}, fmt.Sprintf("%s:%d", tg.Postgres.RemoteHost, tg.Postgres.RemotePort), logger, logFile)
		if err != nil {
			return fmt.Errorf("open ssh tunnel: %w", err)
		}
		defer t.Close()
		tunnelRef = t
		uri = tg.BuildURI(t.LocalPort())
	} else {
		uri = tg.BuildURI(0)
	}
	_ = tunnelRef // kept to make defer chain explicit

	argv := []string{"uv", "run", "postgres-mcp", "--access-mode=" + tg.Mode()}
	env := append(os.Environ(), "DATABASE_URI="+uri)

	logger.Info("launching subprocess", "cmd", argv, "uri_host_only", maskURI(uri))

	if err := proxy.Run(ctx, argv, env, logFile); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			logger.Warn("subprocess exited non-zero", "code", exitErr.ExitCode())
			return err
		}
		return fmt.Errorf("subprocess: %w", err)
	}
	logger.Info("subprocess exited cleanly")
	return nil
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "pg-mcp-tunnel", "config.yaml")
}

func openLogFile(custom, target string) (*os.File, error) {
	path := custom
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(home, ".cache", "pg-mcp-tunnel")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		path = filepath.Join(dir, target+".log")
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		_ = os.Truncate(path, 0)
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// maskURI strips the password from a postgres URI for logging.
func maskURI(uri string) string {
	// Cheap mask: postgres-mcp's URI format is postgresql://user[:pwd]@host:port/db?...
	// We don't parse — slog should not be a hot path. Just blank everything between :// and @.
	atIdx := -1
	schemeIdx := -1
	for i := 0; i < len(uri)-2; i++ {
		if uri[i:i+3] == "://" {
			schemeIdx = i + 3
		}
		if schemeIdx > 0 && uri[i] == '@' {
			atIdx = i
			break
		}
	}
	if schemeIdx < 0 || atIdx < 0 {
		return uri
	}
	return uri[:schemeIdx] + "***" + uri[atIdx:]
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
