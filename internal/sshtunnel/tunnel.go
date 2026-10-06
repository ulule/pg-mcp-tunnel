// Package sshtunnel opens a local TCP forward to a remote address through an
// SSH tunnel, using the system `ssh` binary as the transport.
//
// We deliberately do NOT reimplement an SSH client: OpenSSH already handles
// ProxyCommand, ProxyJump, certificate auth, FIDO keys, the macOS Keychain,
// and ssh-agent forwarding correctly. Our job is just to orchestrate.
//
// Architecture: pg-mcp-tunnel binds the user-visible local port itself with a
// Go net.Listen, and ssh -L binds a different internal port. Per inbound
// connection we wait for ssh to come up and then proxy bytes through; if ssh
// has exited since (dropped link), the connection launches a new ssh first. This
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
// this window, the ssh session is killed and counted as a failed dial.
const sshReadyTimeout = 30 * time.Second

// Time budget for a single accepted connection to wait for ssh to come up
// before being dropped. Should be ~equal to the connect_timeout we set
// in the DSN so the pool gets a clean error rather than a deadlock.
const perConnWaitForReady = 30 * time.Second

// After a failed dial (ssh exited before its port was up), connections
// arriving within this window are refused with the last error instead of
// launching ssh again, so a pool retrying its connects against a host that
// rejects us doesn't spawn ssh in a tight loop.
const redialCooldown = 5 * time.Second

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

// Tunnel owns the user-visible local port for the whole process lifetime and
// runs ssh -L sessions behind it. ssh exits whenever the link drops (laptop
// sleep, VPN or Wi-Fi change, sshd restart, bastion timeout); the next inbound
// connection then launches a fresh session, so a dropped link never needs a
// restart of the MCP server.
type Tunnel struct {
	target     string   // [user@]host
	sshArgs    []string // ssh options, without -L and the target
	remoteAddr string   // host:port forwarded on the remote side
	localPort  int      // user-visible port we listen on (the pool connects here)
	listener   net.Listener
	logger     *slog.Logger
	stderr     io.Writer
	tap        tcpproxy.TapFactory
	ctx        context.Context

	mu           sync.Mutex
	cur          *session  // latest ssh session, possibly dead
	lastErr      error     // why the tunnel is down; nil while up or never failed
	dialFailedAt time.Time // when a session last failed to come up

	drops chan struct{} // signaled (non-blocking) each time a ready session dies

	closeCh   chan struct{} // closed by Close() to short-circuit waits
	closeOnce sync.Once
}

// session is one ssh -L process, run through the shepherd.
type session struct {
	shepherd *exec.Cmd
	port     int // internal port ssh -L binds to

	// lifelineW is the write end of the lifeline pipe inherited by the
	// shepherd. The OS closes it on any process exit (including SIGKILL),
	// which triggers the shepherd to kill ssh.
	lifelineW *os.File

	stderrRing *ringBuffer // tail of this session's ssh stderr

	ready chan struct{} // closed when ssh -L's internal port is listening
	done  chan struct{} // closed when the shepherd exits

	reason error // why the session ended, if killed by us; guarded by Tunnel.mu
}

// Open opens the local user-visible TCP listener and launches a first ssh -L
// session in the background. It returns as soon as the listener is bound
// (typically < 5ms), without waiting for ssh to negotiate with the remote.
// Connections accepted on the local port wait (up to ~30s) for ssh to come
// up, then are proxied; if ssh has died meanwhile, a new session is launched.
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
	if _, _, err := net.SplitHostPort(remoteAddr); err != nil {
		return nil, fmt.Errorf("parse remote addr %q: %w", remoteAddr, err)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("`ssh` not found in PATH: %w", err)
	}

	// Listener for the user-visible port: opened synchronously so the pool's
	// first TCP connect always succeeds.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open local listener: %w", err)
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
	}
	if cfg.Port > 0 {
		args = append(args, "-p", strconv.Itoa(cfg.Port))
	}

	t := &Tunnel{
		target:     target,
		sshArgs:    args,
		remoteAddr: remoteAddr,
		localPort:  listener.Addr().(*net.TCPAddr).Port,
		listener:   listener,
		logger:     logger,
		stderr:     stderr,
		tap:        cfg.Tap,
		ctx:        ctx,
		drops:      make(chan struct{}, 1),
		closeCh:    make(chan struct{}),
	}

	// Dial eagerly so the tunnel is usually up before the first tool call.
	// A failure here is not fatal: it is reported by FailReason and retried
	// by the next inbound connection.
	if _, err := t.session(); err != nil {
		logger.Warn("ssh tunnel launch failed", "err", err)
	}

	go t.acceptLoop()

	return t, nil
}

func (t *Tunnel) LocalPort() int { return t.localPort }

// FailReason returns why the tunnel is currently down (ssh exited, or did not
// come up in time), including the tail of ssh's stderr; nil while it is up or
// still connecting for the first time. A down tunnel is redialed by the next
// inbound connection.
func (t *Tunnel) FailReason() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// Drops is signaled each time an established ssh session dies on its own.
// Connections opened through it are dead: a pool should drop them.
func (t *Tunnel) Drops() <-chan struct{} { return t.drops }

// session returns the live ssh session, launching a new one if the previous
// one exited.
func (t *Tunnel) session() (*session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.closeCh:
		return nil, net.ErrClosed
	default:
	}
	if s := t.cur; s != nil {
		select {
		case <-s.done:
		default:
			return s, nil
		}
	}
	if t.lastErr != nil && time.Since(t.dialFailedAt) < redialCooldown {
		return nil, t.lastErr
	}
	s, err := t.launch()
	if err != nil {
		t.lastErr = err
		t.dialFailedAt = time.Now()
		return nil, err
	}
	t.cur = s
	return s, nil
}

// launch starts an ssh -L session on a fresh internal port. Called with t.mu held.
func (t *Tunnel) launch() (*session, error) {
	// There's an unavoidable race here (the port could be claimed by another
	// process between pick and bind), but in practice it's small and ssh will
	// fail fast with "address already in use", which we'd see in stderr/log.
	port, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick ssh internal port: %w", err)
	}
	argv := append([]string{"ssh"}, t.sshArgs...)
	argv = append(argv, "-L", fmt.Sprintf("127.0.0.1:%d:%s", port, t.remoteAddr), t.target)

	redial := t.cur != nil
	t.logger.Info("launching ssh tunnel via shepherd (async)",
		"target", t.target, "local_port", t.localPort, "ssh_port", port,
		"remote", t.remoteAddr, "redial", redial, "argv", argv)

	// Tee ssh's stderr into a ring buffer so we can surface the tail in error
	// messages (e.g. "Permission denied (publickey)") when ssh fails.
	ring := newRingBuffer(stderrRingSize, t.stderr)
	cmd, lifelineW, err := launchShepherd(argv, ring)
	if err != nil {
		return nil, err
	}
	s := &session{
		shepherd:   cmd,
		port:       port,
		lifelineW:  lifelineW,
		stderrRing: ring,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
	}
	go t.watch(s)
	go t.probeReady(s)
	return s, nil
}

// watch waits for the session's shepherd to exit and records why.
func (t *Tunnel) watch(s *session) {
	_ = s.shepherd.Wait()
	_ = s.lifelineW.Close()
	close(s.done)

	select {
	case <-t.closeCh:
		return // intentional Close(), not a failure
	default:
	}

	wasReady := false
	select {
	case <-s.ready:
		wasReady = true
	default:
	}

	t.mu.Lock()
	if s.reason == nil {
		s.reason = fmt.Errorf("ssh process exited: %s", s.stderrRing.tail())
	}
	t.lastErr = s.reason
	if !wasReady {
		t.dialFailedAt = time.Now()
	}
	t.mu.Unlock()

	t.logger.Warn("ssh tunnel down, will redial on next connection", "err", s.reason)
	if wasReady {
		select {
		case t.drops <- struct{}{}:
		default:
		}
	}
}

// probeReady polls the session's internal port until ssh accepts connections,
// then closes s.ready. A session that doesn't come up in time is killed.
func (t *Tunnel) probeReady(s *session) {
	deadline := time.Now().Add(sshReadyTimeout)
	addr := "127.0.0.1:" + strconv.Itoa(s.port)
	for {
		select {
		case <-s.done:
			return
		case <-t.closeCh:
			return
		case <-t.ctx.Done():
			return
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.mu.Lock()
			t.lastErr = nil
			t.mu.Unlock()
			t.logger.Info("ssh tunnel established", "local_port", t.localPort, "ssh_port", s.port)
			close(s.ready)
			return
		}
		if !isConnRefused(err) {
			t.logger.Warn("ssh tunnel probe error", "err", err)
		}
		if time.Now().After(deadline) {
			t.logger.Warn("ssh tunnel did not come up within timeout",
				"ssh_port", s.port, "timeout", sshReadyTimeout)
			t.mu.Lock()
			s.reason = fmt.Errorf("ssh tunnel did not come up within %v: %s",
				sshReadyTimeout, s.stderrRing.tail())
			t.mu.Unlock()
			s.kill()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// kill asks the shepherd to terminate ssh, and SIGTERMs the whole process
// group in case it's wedged (e.g. ssh ignored SIGTERM).
func (s *session) kill() {
	_ = s.lifelineW.Close()
	_ = syscall.Kill(-s.shepherd.Process.Pid, syscall.SIGTERM)
}

// acceptLoop accepts client connections on the user-visible port and spawns a
// handler per connection. Exits when the listener is closed.
func (t *Tunnel) acceptLoop() {
	for {
		client, err := t.listener.Accept()
		if err != nil {
			return // listener closed by Close()
		}
		go t.handleConn(client)
	}
}

// handleConn waits for ssh to be ready (launching it again if it died), then
// proxies bytes between the client and ssh's internal port in both
// directions until either side closes.
func (t *Tunnel) handleConn(client net.Conn) {
	defer client.Close()

	s, err := t.session()
	if err != nil {
		t.logger.Warn("dropping client connection: ssh tunnel down", "err", err)
		return
	}
	select {
	case <-s.ready:
	case <-s.done:
		return
	case <-t.closeCh:
		return
	case <-time.After(perConnWaitForReady):
		t.logger.Warn("dropping client connection: ssh tunnel not ready in time",
			"remote", client.RemoteAddr(), "timeout", perConnWaitForReady)
		return
	}

	upstream, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(s.port), 5*time.Second)
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
		t.mu.Lock()
		close(t.closeCh)
		s := t.cur
		t.mu.Unlock()
		_ = t.listener.Close()
		if s == nil {
			return
		}
		s.kill()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-s.shepherd.Process.Pid, syscall.SIGKILL)
			<-s.done
		}
	})
	return nil
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
