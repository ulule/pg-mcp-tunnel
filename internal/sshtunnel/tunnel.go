// Package sshtunnel opens a local TCP forward to a remote address through an
// SSH tunnel, using the system `ssh` binary as the transport.
//
// We deliberately do NOT reimplement an SSH client: OpenSSH already handles
// ProxyCommand, ProxyJump, certificate auth, FIDO keys, the macOS Keychain,
// and ssh-agent forwarding correctly. Our job is just to orchestrate.
package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Host string
	User string // optional
	Port int    // optional, ssh's default applies otherwise
}

type Tunnel struct {
	cmd       *exec.Cmd
	localPort int
	logger    *slog.Logger

	closeOnce sync.Once
	closeErr  error
	doneCh    chan struct{}
}

// Open opens an SSH-tunneled local TCP listener forwarding to remoteAddr
// (e.g. "localhost:5432"). It blocks until the local port is ready to accept
// connections, or returns an error if the tunnel fails to come up.
func Open(ctx context.Context, cfg Config, remoteAddr string, logger *slog.Logger, stderr io.Writer) (*Tunnel, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Host == "" {
		return nil, errors.New("ssh host is required")
	}
	remoteHost, remotePort, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("parse remote addr %q: %w", remoteAddr, err)
	}

	localPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick local port: %w", err)
	}

	target := cfg.Host
	if cfg.User != "" {
		target = cfg.User + "@" + cfg.Host
	}

	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=60",
		"-o", "ServerAliveCountMax=3",
		"-o", "StrictHostKeyChecking=accept-new",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%s", localPort, remoteHost, remotePort),
	}
	if cfg.Port > 0 {
		args = append(args, "-p", strconv.Itoa(cfg.Port))
	}
	args = append(args, target)

	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("`ssh` not found in PATH: %w", err)
	}

	cmd := exec.Command("ssh", args...)
	cmd.Stderr = stderr
	// Own process group so we can SIGTERM the tree on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logger.Info("launching ssh tunnel", "target", target, "local_port", localPort, "remote", remoteAddr, "argv", append([]string{"ssh"}, args...))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}

	t := &Tunnel{
		cmd:       cmd,
		localPort: localPort,
		logger:    logger,
		doneCh:    make(chan struct{}),
	}

	// Watch for early ssh death.
	go func() {
		_ = cmd.Wait()
		close(t.doneCh)
	}()

	if err := waitListening(ctx, localPort, t.doneCh, 20*time.Second); err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("tunnel did not come up: %w", err)
	}

	logger.Info("ssh tunnel established", "local_port", localPort)
	return t, nil
}

func (t *Tunnel) LocalPort() int { return t.localPort }

// Done returns a channel that is closed when the underlying ssh process exits.
func (t *Tunnel) Done() <-chan struct{} { return t.doneCh }

func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		if t.cmd.Process == nil {
			return
		}
		// SIGTERM the whole group, then SIGKILL after grace period.
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-t.doneCh:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
			<-t.doneCh
		}
	})
	return t.closeErr
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func waitListening(ctx context.Context, port int, doneCh <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-doneCh:
			return errors.New("ssh process exited before tunnel came up")
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if !isConnRefused(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", addr)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection refused")
}
