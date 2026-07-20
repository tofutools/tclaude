package agentd

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestHangupProcessGroup_ReachesForkedChild pins that hangupProcessGroup signals
// the whole Setsid process group, not just the wrapper PID — the property that
// makes it a sound teardown backstop. runPTYOverWS's wrapper FORKS the tmux
// client as a child, so a SIGHUP to the wrapper alone would miss it.
//
// The test reproduces that shape — a Setsid wrapper that forks a `sleep` — and
// observes the child's death via a pipe rather than a PID probe (a killed-but-
// unreaped zombie still answers `kill(pid, 0)`, which would race). Both wrapper
// and child inherit the "eof" pipe's write end, so its read end sees EOF only
// once BOTH have exited.
//
// The "ready" pipe is a handshake that gates the signal until the wrapper has
// actually forked the child: without it the wrapper could be killed before it
// forks, so even a buggy wrapper-only signal would pass (the child never exists
// to hold the eof pipe). With the handshake, a wrapper-only signal leaves the
// forked child holding the eof pipe → the read blocks past the deadline → the
// test fails, which is the regression this guards.
func TestHangupProcessGroup_ReachesForkedChild(t *testing.T) {
	eofR, eofW, err := os.Pipe()
	if err != nil {
		t.Fatalf("eof pipe: %v", err)
	}
	defer func() { _ = eofR.Close() }()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("ready pipe: %v", err)
	}
	defer func() { _ = readyR.Close() }()

	// `sleep 60 &` forks the child (inheriting fd 3 = eof and fd 4 = ready);
	// `echo R 1>&4` signals readiness only AFTER that fork; `wait` keeps the
	// wrapper alive. Setsid mirrors what pty.Start does in production, making
	// the wrapper a group leader (pgid == pid).
	cmd := exec.Command("sh", "-c", "sleep 60 & echo R 1>&4; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{eofW, readyW} // child fd 3 = eof, fd 4 = ready
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	_ = eofW.Close()
	_ = readyW.Close()
	t.Cleanup(func() {
		// Never leak the group if the assertion fails before it exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Block until the wrapper has forked the child — so a wrapper-only signal
	// would genuinely leave the child behind (the false-pass this avoids).
	if _, err := readyR.Read(make([]byte, 1)); err != nil {
		t.Fatalf("read readiness: %v", err)
	}

	hangupProcessGroup(cmd.Process)

	done := make(chan struct{})
	go func() {
		// Blocks until EOF — until every eof-pipe holder (wrapper AND the
		// forked child) has exited. Nothing ever writes to the eof pipe.
		_, _ = eofR.Read(make([]byte, 1))
		close(done)
	}()

	select {
	case <-done:
		// EOF: the forked child exited, so the group SIGHUP reached it.
	case <-time.After(10 * time.Second):
		t.Fatal("forked child still holding the pipe 10s after the group hangup — " +
			"SIGHUP didn't reach it; a wrapper-only signal would leave the tmux client attached")
	}
}
