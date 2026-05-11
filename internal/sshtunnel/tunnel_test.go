package sshtunnel

import (
	"os"
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
