package sshtunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
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
// local listener from accepting. This is the property that lets postgres-mcp
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

// TestMarkFailedClosesListenerAndChannel verifies the core failure-detection
// primitive: markFailed closes the Failed() channel, captures the reason,
// closes the listener, and is idempotent (sync.Once).
func TestMarkFailedClosesListenerAndChannel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tun := &Tunnel{
		listener:   listener,
		stderrRing: newRingBuffer(64, nil),
		failedCh:   make(chan struct{}),
	}

	tun.markFailed(errors.New("boom"))

	select {
	case <-tun.Failed():
	case <-time.After(time.Second):
		t.Fatal("Failed() channel was not closed")
	}
	if tun.FailReason() == nil || tun.FailReason().Error() != "boom" {
		t.Errorf("FailReason = %v, want 'boom'", tun.FailReason())
	}
	// Listener should be closed (Accept returns an error immediately).
	if c, err := listener.Accept(); err == nil {
		_ = c.Close()
		t.Error("listener still accepting after markFailed")
	}

	// Second call is a no-op (does not panic, does not overwrite reason).
	tun.markFailed(errors.New("different"))
	if tun.FailReason().Error() != "boom" {
		t.Errorf("FailReason changed after second markFailed: %v", tun.FailReason())
	}
}

// TestOpenFailsWhenShepherdChildExitsImmediately exercises the real Open()
// path with a configuration that causes ssh to fail fast (port 1 = closed),
// and verifies that Failed() fires within a few seconds and FailReason()
// surfaces something useful — without needing a real SSH server.
//
// ssh's own connect to 127.0.0.1:1 returns ECONNREFUSED in milliseconds; with
// ExitOnForwardFailure=yes it exits non-zero almost immediately, the shepherd
// follows, doneCh closes, and the cmd.Wait goroutine calls markFailed.
func TestOpenFailsWhenShepherdChildExitsImmediately(t *testing.T) {
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

	select {
	case <-tun.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed() did not fire within 10s after ssh exited")
	}
	if tun.FailReason() == nil {
		t.Error("FailReason() is nil after Failed() fired")
	}
	t.Logf("FailReason: %v", tun.FailReason())
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
