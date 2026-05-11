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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// shepherdScript wraps an arbitrary command in a shell that watches a "lifeline"
// pipe on FD 3. When the parent process dies (any cause, including SIGKILL),
// the kernel closes the write end of the pipe and the watcher subshell observes
// EOF on FD 3, SIGTERMs the child, and exits with the child's status.
//
// The script is intentionally command-agnostic: "$@" is the full argv of the
// child. The caller prepends the program name (e.g. "ssh") followed by its args.
//
// macOS /bin/sh (bash 3.2 in posix mode) compatible: no `wait -n`, no `read -t`.
const shepherdScript = `"$@" &
child_pid=$!
(
    IFS= read -r _ <&3
    kill -TERM "$child_pid" 2>/dev/null
) &
watcher_pid=$!
exec 3<&-
wait "$child_pid"
status=$?
kill -TERM "$watcher_pid" 2>/dev/null
wait "$watcher_pid" 2>/dev/null
exit "$status"
`

type Config struct {
	Host string
	User string // optional
	Port int    // optional, ssh's default applies otherwise
}

type Tunnel struct {
	cmd       *exec.Cmd
	localPort int
	logger    *slog.Logger

	// lifelineW is the write end of the lifeline pipe inherited by the shepherd.
	// We keep it open for the lifetime of this process; the OS closes it on any
	// process exit (including SIGKILL), which triggers the shepherd to kill ssh.
	lifelineW *os.File

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

	logger.Info("launching ssh tunnel via shepherd",
		"target", target, "local_port", localPort, "remote", remoteAddr,
		"argv", append([]string{"ssh"}, args...))

	cmd, lifelineW, err := launchShepherd(append([]string{"ssh"}, args...), stderr)
	if err != nil {
		return nil, err
	}

	t := &Tunnel{
		cmd:       cmd,
		localPort: localPort,
		logger:    logger,
		lifelineW: lifelineW,
		doneCh:    make(chan struct{}),
	}

	// Watch for early shepherd/ssh death.
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
		// Graceful: closing the lifeline write end causes the shepherd's watcher
		// to observe EOF on FD 3, SIGTERM ssh, and exit with ssh's status.
		if t.lifelineW != nil {
			_ = t.lifelineW.Close()
		}
		// Defense in depth: SIGTERM the shepherd's process group as well, in case
		// it's wedged (e.g. ssh ignored SIGTERM).
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

// launchShepherd starts `/bin/sh -c shepherdScript shepherd <argv...>` with a
// lifeline pipe inherited as FD 3 in the child. The returned *exec.Cmd is the
// running shepherd (sh + ssh in the same process group). The returned *os.File
// is the write end of the lifeline; keep it open for the lifetime of this
// process. Closing it (or this process dying for any reason) triggers the
// shepherd to kill its child.
//
// Exposed (package-internal) so tests can exercise it with arbitrary argv.
func launchShepherd(argv []string, stderr io.Writer) (*exec.Cmd, *os.File, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("empty argv")
	}

	lifelineR, lifelineW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create lifeline pipe: %w", err)
	}

	// sh -c SCRIPT $0 $1 $2 ...
	//   $0 = "shepherd" (script name, used in error messages)
	//   $@ = argv... (the actual program + args to run)
	shArgs := append([]string{"-c", shepherdScript, "shepherd"}, argv...)
	cmd := exec.Command("/bin/sh", shArgs...)
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{lifelineR} // -> FD 3 in the shepherd
	// Own process group so the SIGKILL fallback in Close() can hit the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = lifelineR.Close()
		_ = lifelineW.Close()
		return nil, nil, fmt.Errorf("start shepherd: %w", err)
	}
	// Parent doesn't need the read end; only the shepherd should hold it.
	_ = lifelineR.Close()

	return cmd, lifelineW, nil
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
