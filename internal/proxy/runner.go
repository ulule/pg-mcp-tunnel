package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run executes argv with the given env, wires the binary's stdin/stdout to the
// child, and writes the child's stderr to logStderr. It returns when the child
// exits or ctx is canceled (in which case it sends SIGTERM, then SIGKILL after
// a grace period).
//
// The returned error is nil if the child exited cleanly with code 0; otherwise
// it carries the exit code via *exec.ExitError so the caller can propagate it.
//
// Note: no parent-death shepherd is needed here (contrast with internal/sshtunnel).
// The MCP child (e.g. postgres-mcp) is tethered to the host's stdin pipe via
// cmd.Stdin = os.Stdin: when Claude Desktop quits and closes the pipe, the child
// reads EOF on stdin and shuts down on its own. ssh -N does not read stdin, which
// is why sshtunnel needs an explicit lifeline-pipe shepherd.
func Run(ctx context.Context, argv []string, env []string, logStderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty argv")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return fmt.Errorf("not in PATH: %s (%w)", argv[0], err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = logStderr
	// Put the child in its own process group so SIGTERM doesn't bounce back from the
	// terminal foreground group, and so we can signal the whole subtree if needed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = signalGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
			return <-done
		}
	}
}

func signalGroup(pid int, sig syscall.Signal) error {
	// Negative pid = process group.
	return syscall.Kill(-pid, sig)
}
