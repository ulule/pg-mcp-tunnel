package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"syscall"

	"github.com/ulule/pg-mcp-tunnel/internal/config"
	"github.com/ulule/pg-mcp-tunnel/internal/webui"
)

// runUI serves the live query dashboard: `pg-mcp-tunnel ui [--addr] [--open]`.
func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	var (
		addr       = fs.String("addr", "127.0.0.1:7878", "listen address (loopback only is recommended: recorded SQL is shown)")
		configPath = fs.String("config", defaultConfigPath(), "path to config.yaml, used to list targets before they emit anything")
		open       = fs.Bool("open", false, "open the dashboard in the default browser")
	)
	_ = fs.Parse(args)

	dir, err := cacheDir()
	if err != nil {
		return err
	}

	var targets []string
	if cfg, err := config.LoadFile(*configPath); err == nil {
		for name := range cfg.Targets {
			targets = append(targets, name)
		}
		sort.Strings(targets)
	}

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	url := webui.URL(l.Addr())
	fmt.Fprintf(os.Stderr, "pg-mcp-tunnel ui: watching %s\n  → %s\n", dir, url)
	if *open {
		openBrowser(url)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return webui.New(dir, targets, logger).Serve(ctx, l)
}

func openBrowser(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	_ = exec.Command(cmd, url).Start()
}
