package agentd

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestHangupProcessGroup_ReachesForkedGrandchild pins the fix for the web
// window "Detach" not detaching on the tmux level: runPTYOverWS's child is a
// wrapper that FORKS the tmux client (a grandchild), so signaling only the
// wrapper PID left the client attached. hangupProcessGroup must signal the
// whole Setsid group so the forked grandchild gets it too.
//
// The test reproduces that exact process shape — a Setsid wrapper that forks a
// `sleep` child — and observes the grandchild's death via a pipe rather than a
// PID probe (a killed-but-unreaped zombie still answers `kill(pid, 0)`, which
// would race). Both wrapper and grandchild inherit the pipe's write end, so the
// read end sees EOF only once BOTH have exited. A wrapper-only signal would
// leave the grandchild holding the pipe open → the read would block past the
// deadline → the test fails, which is precisely the regression we're guarding.
func TestHangupProcessGroup_ReachesForkedGrandchild(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()

	// `sleep 60 &` forks the grandchild; `wait` keeps the wrapper alive. Both
	// inherit fd 3 (the pipe write end via ExtraFiles). Setsid mirrors what
	// pty.Start does in production, making the wrapper a group leader.
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	_ = w.Close() // parent drops its write end; the children hold the rest
	t.Cleanup(func() {
		// Never leak the group if the assertion fails before it exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	hangupProcessGroup(cmd.Process)

	done := make(chan struct{})
	go func() {
		// Blocks until EOF — i.e. until every fd-3 holder (wrapper AND the
		// forked grandchild) has exited. Nothing ever writes a byte.
		_, _ = r.Read(make([]byte, 1))
		close(done)
	}()

	select {
	case <-done:
		// EOF: the forked grandchild exited, so the group SIGHUP reached it.
	case <-time.After(3 * time.Second):
		t.Fatal("forked grandchild still holding the pipe 3s after the group hangup — " +
			"SIGHUP didn't reach it; a wrapper-only signal would leave the tmux client attached")
	}
}
