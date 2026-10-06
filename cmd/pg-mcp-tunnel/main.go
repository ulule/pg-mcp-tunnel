package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ulule/pg-mcp-tunnel/internal/config"
	"github.com/ulule/pg-mcp-tunnel/internal/pgmcp"
	"github.com/ulule/pg-mcp-tunnel/internal/querylog"
	"github.com/ulule/pg-mcp-tunnel/internal/sshtunnel"
	"github.com/ulule/pg-mcp-tunnel/internal/tcpproxy"
)

const (
	maxLogSize = 10 * 1024 * 1024 // 10 MiB
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ui" {
		if err := runUI(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "pg-mcp-tunnel ui:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pg-mcp-tunnel:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		target     = flag.String("target", "", "target name from config (required)")
		configPath = flag.String("config", defaultConfigPath(), "path to config.yaml")
		logPath    = flag.String("log-file", "", "path to log file (default ~/.cache/pg-mcp-tunnel/<target>.log)")
		record     = flag.Bool("record-queries", true, "record observed SQL to /tmp/pg-mcp-tunnel-<uid>/<target>"+querylog.FileSuffix+" for `pg-mcp-tunnel ui`")
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
		tap        tcpproxy.TapFactory
		onToolCall func(pgmcp.ToolCall)
	)
	if *record {
		rec, err := openRecorder(*target)
		if err != nil {
			// Observability is best-effort: never refuse to start over it.
			logger.Warn("query recording disabled", "err", err)
		} else {
			defer rec.Close()
			tap = func() tcpproxy.Tap { return rec.NewSniffer() }
			onToolCall = func(c pgmcp.ToolCall) { rec.Record(toolEvent(c)) }
		}
	}

	var (
		uri       string
		tunnelRef *sshtunnel.Tunnel
	)
	if tg.SSH != nil {
		t, err := sshtunnel.Open(ctx, sshtunnel.Config{
			Host: tg.SSH.Host,
			User: tg.SSH.User,
			Port: tg.SSH.Port,
			Tap:  tap,
		}, fmt.Sprintf("%s:%d", tg.Postgres.RemoteHost, tg.Postgres.RemotePort), logger, logFile)
		if err != nil {
			return fmt.Errorf("open ssh tunnel: %w", err)
		}
		defer t.Close()
		tunnelRef = t
		uri = tg.BuildURI(t.LocalPort())

		// If the tunnel permanently fails (ssh died, or never came up), keep
		// serving: every tool call then answers "ssh tunnel failed: <reason>",
		// which the model can read, instead of a bare "Server disconnected".
		go func() {
			select {
			case <-t.Failed():
				logger.Error("ssh tunnel failed", "err", t.FailReason())
				fmt.Fprintln(os.Stderr, "pg-mcp-tunnel: ssh tunnel failed:", t.FailReason())
			case <-ctx.Done():
			}
		}()
	} else {
		uri = tg.BuildURI(0)
		if tap != nil {
			// Relay direct connections through a local port so they can be
			// observed too. Targets that can't be relayed safely connect
			// directly, as before.
			if addr, err := tg.DirectAddr(); err != nil {
				logger.Info("direct target not relayed, queries won't be recorded", "reason", err)
			} else if relay, err := tcpproxy.Listen(addr, tap, logger); err != nil {
				logger.Warn("open local relay failed, connecting directly", "err", err)
			} else {
				defer relay.Close()
				uri = tg.BuildURI(relay.LocalPort())
			}
		}
	}

	var tunnelErr func() error
	if tunnelRef != nil {
		tunnelErr = tunnelRef.FailReason
	}
	srv, err := pgmcp.New(pgmcp.Options{
		URI:            uri,
		Mode:           tg.Mode(),
		ExtraFunctions: tg.AllowedFunctions,
		Logger:         logger,
		TunnelErr:      tunnelErr,
		OnToolCall:     onToolCall,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	logger.Info("serving MCP on stdio", "mode", tg.Mode(), "uri_host_only", maskURI(uri))
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		logger.Error("mcp server stopped", "err", err)
		return err
	}
	logger.Info("mcp client disconnected")
	return nil
}

// toolEvent turns an answered MCP tool call into a query-log event. SQL holds
// the "sql" argument when the tool has one, the JSON arguments otherwise.
func toolEvent(c pgmcp.ToolCall) querylog.Event {
	ev := querylog.Event{
		Kind:       querylog.KindTool,
		Tool:       c.Name,
		DurationMS: float64(c.Duration.Microseconds()) / 1000,
		TokensIn:   pgmcp.EstimateTokens(string(c.Args)),
		Tokens:     pgmcp.EstimateTokens(c.Output),
		Bytes:      len(c.Output),
	}
	var args struct {
		SQL string `json:"sql"`
	}
	if json.Unmarshal(c.Args, &args) == nil && args.SQL != "" {
		ev.SQL = args.SQL
	} else if string(c.Args) != "{}" {
		ev.SQL = string(c.Args)
	}
	if c.IsError {
		ev.Error = strings.TrimPrefix(c.Output, "Error: ")
	}
	return ev
}

func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "pg-mcp-tunnel")
	return dir, os.MkdirAll(dir, 0o700)
}

func openRecorder(target string) (*querylog.Recorder, error) {
	dir, err := querylog.DefaultDir()
	if err != nil {
		return nil, err
	}
	return querylog.OpenRecorder(filepath.Join(dir, target+querylog.FileSuffix), target)
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
		dir, err := cacheDir()
		if err != nil {
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
	// Cheap mask: the URI format is postgresql://user[:pwd]@host:port/db?...
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
