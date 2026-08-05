//go:build unix

package agentd

import (
	"fmt"
	"syscall"
)

// softExitSignalLadder is the signal half of the escalation, politest first.
//
// SIGTERM is a harness's last chance to shut itself down in an orderly way
// (flush its session state, write its end-of-session event). SIGKILL cannot be
// caught, so it takes those with it — which is exactly why it is only reached
// after SIGTERM has been sent and its grace has expired.
var softExitSignalLadder = []struct {
	name   string
	signal syscall.Signal
}{
	{"SIGTERM", syscall.SIGTERM},
	{"SIGKILL", syscall.SIGKILL},
}

// signalLifecycleProcessGroup signals the pane process's whole group.
//
// The group, not the pid: tmux starts the harness as a process-group leader
// and a harness that is being held open by a child (a shell tool, an indexer)
// is precisely the case the ladder exists for. Signalling only the leader
// would leave that child owning the terminal.
var signalLifecycleProcessGroup = signalProcessGroup

// signalProcessGroup is the production implementation, named rather than
// anonymous so a unit test can exercise the REAL refusals directly. The var
// above is swapped binary-wide by TestMain (TCL-1035), so a test that went
// through it would be asserting against a no-op stub — which is how the first
// version of that test managed to pass and then fail for the right reason.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return err
	}
	// A pane process that somehow resolved to the daemon's OWN group would
	// make the last rung of this ladder SIGKILL agentd. tmux gives every pane
	// its own session and group, so this should be unreachable — which is
	// exactly why it is worth failing closed on rather than trusting.
	if pgid == syscall.Getpgrp() {
		return fmt.Errorf("refusing to signal the daemon's own process group (pgid %d)", pgid)
	}
	// The second instance of the same guard, for a case that is worse.
	//
	// kill(2) reads a negative pid as a group, and two spellings are not group
	// sends at all: -1 means EVERY process the caller may signal, and -0 means
	// the caller's own group. So a pane process whose group resolved to 1 would
	// make this ladder SIGKILL the user's whole session, and one that resolved
	// to 0 would make it kill the daemon's group by another route.
	//
	// I cannot demonstrate either happening in production, and this is not a
	// claim that they do — tmux gives every pane its own session and group,
	// which should keep both unreachable. It is guarded for the same reason the
	// line above it is: the cost of the guard is a logged refusal, no
	// legitimate call is ever spelled this way, and the cost of being wrong is
	// every process the user owns.
	//
	// Tests can no longer produce it — TestMain neutralizes these rungs for the
	// whole test binary (TCL-1035) — which removes the only party that was ever
	// observed doing it, not the exposure itself.
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d: not a pane's group", pgid)
	}
	return syscall.Kill(-pgid, sig)
}

// lifecycleProcessAlive reports whether pid still exists. Signal 0 performs
// the permission and existence checks without delivering anything.
var lifecycleProcessAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
