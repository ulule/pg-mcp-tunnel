package sshtunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestShepherdKillsChildOnLifelineEOF verifies the core fix: when the lifeline
// write end is closed (simulating pg-mcp-tunnel dying — gracefully, by panic,
// or by SIGKILL), the shepherd reads EOF on FD 3 and SIGTERMs its child.
func TestShepherdKillsChildOnLifelineEOF(t *testing.T) {
	cmd, lifelineW, err := launchShepherd([]string{"sleep", "30"}, os.Stderr)
	if err != nil {
		t.Fatalf("launchShepherd: %v", err)
	}

	// Let the shepherd start sleep and the watcher subshell.
	time.Sleep(200 * time.Millisecond)

	// Simulate parent death.
	if err := lifelineW.Close(); err != nil {
		t.Fatalf("close lifelineW: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		// Expected: shepherd exits with sleep's terminated status (143 = 128+SIGTERM).
		// We don't assert exact status — only that the shepherd cleaned up promptly.
	case <-time.After(3 * time.Second):
		// Force-kill the pgroup so the test doesn't leak processes.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("shepherd did not exit within 3s after lifeline closed")
	}
}

// TestShepherdExitsCleanlyWhenChildExits verifies the inverse: if the child
// exits on its own (lifeline never closed), the shepherd still exits cleanly
// and doesn't hang forever in the FD 3 read.
func TestShepherdExitsCleanlyWhenChildExits(t *testing.T) {
	cmd, lifelineW, err := launchShepherd([]string{"true"}, os.Stderr)
	if err != nil {
		t.Fatalf("launchShepherd: %v", err)
	}
	defer lifelineW.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shepherd exited with error: %v (want clean exit)", err)
		}
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("shepherd did not exit within 3s after child finished")
	}
}

// TestOpenReturnsImmediatelyAndListens verifies the asynchronous-startup
// contract: Open() returns within milliseconds, the user-visible local port
// is already accepting TCP connections, and Close() tears everything down.
//
// It uses an SSH config that points at a black hole — ssh will never establish
// the tunnel, but that must not prevent Open() from returning quickly or the
// local listener from accepting. This is the property that lets the MCP server
// start in parallel with ssh tunnel setup.
func TestOpenReturnsImmediatelyAndListens(t *testing.T) {
	// Pick a port that is definitely closed locally so ssh's TCP connect spins.
	// Using port 1 to a non-routable address would be ideal, but on macOS
	// ssh's connect timeouts vary. The important thing is just that ssh is
	// not "ready" within the first 200ms — which is always true since ssh
	// hasn't even printed its banner yet.
	cfg := Config{Host: "127.0.0.1", User: "nobody", Port: 1}

	start := time.Now()
	tun, err := Open(context.Background(),
		cfg,
		"localhost:5432",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		io.Discard,
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer tun.Close()

	if elapsed > 500*time.Millisecond {
		t.Errorf("Open took %v; expected fast async return (<500ms)", elapsed)
	}

	// Local listener must accept right away.
	addr := "127.0.0.1:" + strconv.Itoa(tun.LocalPort())
	conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err != nil {
		t.Fatalf("dial local port %d failed: %v", tun.LocalPort(), err)
	}
	_ = conn.Close()

	// After Close, port should stop accepting.
	if err := tun.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	// Give the kernel a beat to actually free the port.
	time.Sleep(100 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("port %d still accepting after Close()", tun.LocalPort())
	}
}

// TestOpenReportsFailureWhenSSHExitsImmediately exercises the real Open()
// path with a configuration that causes ssh to fail fast (port 1 = closed),
// and verifies that FailReason() surfaces something useful within a few
// seconds — without needing a real SSH server.
func TestOpenReportsFailureWhenSSHExitsImmediately(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", User: "nobody", Port: 1}
	tun, err := Open(context.Background(),
		cfg,
		"localhost:5432",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tun.Close()

	deadline := time.Now().Add(10 * time.Second)
	for tun.FailReason() == nil {
		if time.Now().After(deadline) {
			t.Fatal("FailReason() still nil 10s after ssh exited")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("FailReason: %v", tun.FailReason())

	// Within the cooldown, a new connection is refused without relaunching ssh.
	tun.mu.Lock()
	before := tun.cur
	tun.mu.Unlock()
	if _, err := tun.session(); err == nil {
		t.Error("session() succeeded during redial cooldown")
	}
	tun.mu.Lock()
	after := tun.cur
	tun.mu.Unlock()
	if before != after {
		t.Error("ssh relaunched during redial cooldown")
	}
}

// TestRedialAfterSSHDrop simulates a dropped link: the fake ssh is killed
// after the tunnel is up, and the next connection must go through a freshly
// launched ssh on the same local port.
func TestRedialAfterSSHDrop(t *testing.T) {
	installFakeSSH(t)
	remote := startEchoServer(t)

	tun, err := Open(context.Background(), Config{Host: "fake"}, remote,
		slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tun.Close()

	echo(t, tun.LocalPort(), "first")

	tun.mu.Lock()
	first := tun.cur
	tun.mu.Unlock()
	_ = syscall.Kill(-first.shepherd.Process.Pid, syscall.SIGKILL)

	select {
	case <-tun.Drops():
	case <-time.After(5 * time.Second):
		t.Fatal("Drops() not signaled after ssh died")
	}
	if tun.FailReason() == nil {
		t.Error("FailReason() nil while the tunnel is down")
	}

	echo(t, tun.LocalPort(), "second")
	if tun.FailReason() != nil {
		t.Errorf("FailReason() = %v after redial, want nil", tun.FailReason())
	}
	tun.mu.Lock()
	second := tun.cur
	tun.mu.Unlock()
	if second == first {
		t.Error("connection reused the dead ssh session")
	}
}

// echo sends msg through the tunnel's local port and checks it comes back.
func echo(t *testing.T, port int, msg string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %q back: %v", msg, err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

// installFakeSSH puts an `ssh` first in PATH that runs this test binary as a
// plain TCP forwarder for its -L argument (see TestMain).
func installFakeSSH(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nPGMCP_FAKE_SSH=1 exec " + strconv.Quote(self) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMain(m *testing.M) {
	if os.Getenv("PGMCP_FAKE_SSH") == "1" {
		fakeSSH(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// fakeSSH honors "-L 127.0.0.1:port:host:hostport" by forwarding that local
// port to host:hostport, and runs until killed.
func fakeSSH(args []string) {
	var bind, remote string
	for i, a := range args {
		if a == "-L" && i+1 < len(args) {
			parts := strings.SplitN(args[i+1], ":", 3) // 127.0.0.1, port, host:hostport
			bind, remote = parts[0]+":"+parts[1], parts[2]
		}
	}
	l, err := net.Listen("tcp", bind)
	if err != nil {
		os.Exit(255)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(255)
		}
		go func() {
			defer c.Close()
			up, err := net.Dial("tcp", remote)
			if err != nil {
				return
			}
			defer up.Close()
			go func() { _, _ = io.Copy(up, c) }()
			_, _ = io.Copy(c, up)
		}()
	}
}

// TestShepherdSurvivesParentProcessExitSimulation is a slightly heavier check
// that the shepherd is robust even if the lifeline write end was the *only*
// writer in our process — confirming the pipe-EOF mechanism works without any
// explicit signal handling on our side.
func TestShepherdSurvivesParentProcessExitSimulation(t *testing.T) {
	cmd, lifelineW, err := launchShepherd([]string{"sleep", "10"}, os.Stderr)
	if err != nil {
		t.Fatalf("launchShepherd: %v", err)
	}

	// Confirm the shepherd is alive and running its child.
	time.Sleep(150 * time.Millisecond)
	if cmd.ProcessState != nil {
		t.Fatalf("shepherd exited prematurely: %v", cmd.ProcessState)
	}

	// "Simulate" the parent dying by dropping the only reference to the write end.
	// In real life, the kernel closes lifelineW automatically on process exit.
	_ = lifelineW.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("shepherd did not react to lifeline EOF within 3s")
	}
}
