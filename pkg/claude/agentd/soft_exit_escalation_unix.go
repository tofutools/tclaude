//go:build unix

package agentd

import "syscall"

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
var signalLifecycleProcessGroup = func(pid int, sig syscall.Signal) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return err
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
