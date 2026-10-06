// Package sshtunnel opens a local TCP forward to a remote address through an
// SSH tunnel, using the system `ssh` binary as the transport.
//
// We deliberately do NOT reimplement an SSH client: OpenSSH already handles
// ProxyCommand, ProxyJump, certificate auth, FIDO keys, the macOS Keychain,
// and ssh-agent forwarding correctly. Our job is just to orchestrate.
//
// Architecture: pg-mcp-tunnel binds the user-visible local port itself with a
// Go net.Listen, and ssh -L binds a different internal port. Per inbound
// connection we wait for ssh to come up and then proxy bytes through. This
// means Open() returns within milliseconds (the listener is already up), so
// the MCP server can start serving in parallel with ssh tunnel
// setup, and Claude Desktop doesn't time out on the MCP initialize while ssh
// is still negotiating with the remote.
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

	"github.com/ulule/pg-mcp-tunnel/internal/tcpproxy"
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

// Time budget for ssh -L to bind its internal port after we launch it. After
// this window, accepted client connections start being dropped with a clear
// log line rather than hanging.
const sshReadyTimeout = 30 * time.Second

// Time budget for a single accepted connection to wait for ssh to come up
// before being dropped. Should be ~equal to the connect_timeout we set
// in the DSN so the pool gets a clean error rather than a deadlock.
const perConnWaitForReady = 30 * time.Second

// Size of the in-memory ring that captures the tail of ssh's stderr. Enough
// to hold a couple of "Permission denied (publickey)" lines plus context.
const stderrRingSize = 8192

type Config struct {
	Host string
	User string // optional
	Port int    // optional, ssh's default applies otherwise

	// Tap, if set, observes the bytes of every proxied connection.
	Tap tcpproxy.TapFactory
}

type Tunnel struct {
	shepherd  *exec.Cmd
	localPort int // user-visible port we listen on (the pool connects here)
	sshPort   int // internal port ssh -L binds to
	listener  net.Listener
	logger    *slog.Logger
	tap       tcpproxy.TapFactory

	// lifelineW is the write end of the lifeline pipe inherited by the shepherd.
	// We keep it open for the lifetime of this process; the OS closes it on any
	// process exit (including SIGKILL), which triggers the shepherd to kill ssh.
	lifelineW *os.File

	stderrRing *ringBuffer // captures the tail of ssh's stderr for error surfacing

	sshReady chan struct{} // closed when ssh -L's internal port is listening
	closeCh  chan struct{} // closed by Close() to short-circuit waits

	failedCh   chan struct{} // closed when the tunnel has permanently failed
	failOnce   sync.Once
	failReason error

	closeOnce sync.Once
	closeErr  error
	doneCh    chan struct{} // closed when the shepherd process exits
}

// Open spawns an ssh -L tunnel and opens the local user-visible TCP listener
// immediately. It returns as soon as the listener is bound (typically < 5ms),
// without waiting for ssh to negotiate with the remote. Connections accepted
// on the local port wait (up to ~30s) for ssh to come up, then are proxied.
//
// The caller MUST call Close() when done, which tears down the listener, the
// ssh shepherd, and signals any in-flight goroutines.
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

	// Listener for the user-visible port: opened synchronously so the pool's
	// first TCP connect always succeeds.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open local listener: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port

	// Pick a separate ephemeral port for ssh -L's internal bind. There's an
	// unavoidable race here (the port could be claimed by another process
	// between pick and bind), but in practice it's small and ssh will fail
	// fast with "address already in use" which we'd see in stderr/log.
	sshPort, err := pickFreePort()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("pick ssh internal port: %w", err)
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
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%s", sshPort, remoteHost, remotePort),
	}
	if cfg.Port > 0 {
		args = append(args, "-p", strconv.Itoa(cfg.Port))
	}
	args = append(args, target)

	if _, err := exec.LookPath("ssh"); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("`ssh` not found in PATH: %w", err)
	}

	logger.Info("launching ssh tunnel via shepherd (async)",
		"target", target, "local_port", localPort, "ssh_port", sshPort,
		"remote", remoteAddr, "argv", append([]string{"ssh"}, args...))

	// Tee ssh's stderr into a ring buffer so we can surface the tail in error
	// messages (e.g. "Permission denied (publickey)") when ssh fails.
	stderrRing := newRingBuffer(stderrRingSize, stderr)

	cmd, lifelineW, err := launchShepherd(append([]string{"ssh"}, args...), stderrRing)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	t := &Tunnel{
		shepherd:   cmd,
		localPort:  localPort,
		sshPort:    sshPort,
		listener:   listener,
		logger:     logger,
		tap:        cfg.Tap,
		lifelineW:  lifelineW,
		stderrRing: stderrRing,
		sshReady:   make(chan struct{}),
		closeCh:    make(chan struct{}),
		failedCh:   make(chan struct{}),
		doneCh:     make(chan struct{}),
	}

	// Watch for shepherd death. Closing the listener unblocks acceptLoop. If
	// the shepherd died spontaneously (not via Close()), mark the tunnel as
	// failed so the host process can surface a clean error and exit.
	go func() {
		_ = cmd.Wait()
		close(t.doneCh)
		_ = t.listener.Close()

		select {
		case <-t.closeCh:
			// Intentional Close() — not a failure.
			return
		default:
		}
		t.markFailed(fmt.Errorf("ssh process exited unexpectedly: %s", t.stderrRing.tail()))
	}()

	// Background: probe ssh's internal port and close sshReady once it's up.
	go t.probeSSHReady(ctx)

	// Accept loop: per-connection handler waits for sshReady then proxies.
	go t.acceptLoop()

	return t, nil
}

func (t *Tunnel) LocalPort() int { return t.localPort }

// Done returns a channel that is closed when the underlying ssh shepherd exits.
func (t *Tunnel) Done() <-chan struct{} { return t.doneCh }

// Failed returns a channel that is closed when the tunnel has permanently
// failed (ssh died unexpectedly, or did not come up within the timeout).
// Callers should also consult FailReason() to retrieve a human-readable error.
func (t *Tunnel) Failed() <-chan struct{} { return t.failedCh }

// FailReason returns the error explaining why the tunnel failed, or nil if it
// hasn't failed yet. Includes the tail of ssh's stderr when available.
func (t *Tunnel) FailReason() error { return t.failReason }

// markFailed records a permanent tunnel failure exactly once, closes the
// listener and the shepherd's lifeline (which SIGTERMs ssh), and signals
// Failed() subscribers. Subsequent calls are no-ops.
func (t *Tunnel) markFailed(err error) {
	t.failOnce.Do(func() {
		t.failReason = err
		// Tear down the listener so further pool reconnects get
		// ECONNREFUSED immediately instead of waiting perConnWaitForReady.
		if t.listener != nil {
			_ = t.listener.Close()
		}
		// Tell the shepherd to terminate ssh (graceful path).
		if t.lifelineW != nil {
			_ = t.lifelineW.Close()
		}
		close(t.failedCh)
	})
}

// probeSSHReady polls ssh's internal port until it accepts connections or
// gives up. On success it closes t.sshReady.
func (t *Tunnel) probeSSHReady(ctx context.Context) {
	deadline := time.Now().Add(sshReadyTimeout)
	addr := "127.0.0.1:" + strconv.Itoa(t.sshPort)
	for {
		select {
		case <-t.doneCh:
			return
		case <-t.closeCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.logger.Info("ssh tunnel established", "local_port", t.localPort, "ssh_port", t.sshPort)
			close(t.sshReady)
			return
		}
		if !isConnRefused(err) {
			t.logger.Warn("ssh tunnel probe error", "err", err)
			return
		}
		if time.Now().After(deadline) {
			t.logger.Warn("ssh tunnel did not come up within timeout",
				"ssh_port", t.sshPort, "timeout", sshReadyTimeout)
			t.markFailed(fmt.Errorf("ssh tunnel did not come up within %v: %s",
				sshReadyTimeout, t.stderrRing.tail()))
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// acceptLoop accepts client connections on the user-visible port and spawns a
// handler per connection. Exits when the listener is closed.
func (t *Tunnel) acceptLoop() {
	for {
		client, err := t.listener.Accept()
		if err != nil {
			return // listener closed (Close() or shepherd death)
		}
		go t.handleConn(client)
	}
}

// handleConn waits for ssh to be ready then proxies bytes between the client
// and ssh's internal port in both directions until either side closes.
func (t *Tunnel) handleConn(client net.Conn) {
	defer client.Close()

	select {
	case <-t.sshReady:
	case <-t.doneCh:
		return
	case <-t.closeCh:
		return
	case <-time.After(perConnWaitForReady):
		t.logger.Warn("dropping client connection: ssh tunnel not ready in time",
			"remote", client.RemoteAddr(), "timeout", perConnWaitForReady)
		return
	}

	upstream, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(t.sshPort), 5*time.Second)
	if err != nil {
		t.logger.Warn("dial ssh internal port failed", "err", err)
		return
	}
	defer upstream.Close()

	var tap tcpproxy.Tap
	if t.tap != nil {
		tap = t.tap()
	}
	tcpproxy.Pipe(client, upstream, tap)
}

func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		close(t.closeCh)
		if t.listener != nil {
			_ = t.listener.Close()
		}
		if t.shepherd == nil || t.shepherd.Process == nil {
			return
		}
		// Graceful: closing the lifeline write end causes the shepherd's watcher
		// to observe EOF on FD 3, SIGTERM ssh, and exit with ssh's status.
		if t.lifelineW != nil {
			_ = t.lifelineW.Close()
		}
		// Defense in depth: SIGTERM the shepherd's process group as well, in case
		// it's wedged (e.g. ssh ignored SIGTERM).
		_ = syscall.Kill(-t.shepherd.Process.Pid, syscall.SIGTERM)
		select {
		case <-t.doneCh:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-t.shepherd.Process.Pid, syscall.SIGKILL)
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

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection refused")
}
