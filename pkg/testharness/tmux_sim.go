//go:build rewire

package testharness

import (
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SentKey records one tmux send-keys invocation. Tests assert against
// the recorded slice via WaitForSendKeys / Sent().
type SentKey struct {
	Target string
	Text   string
}

// FakeTmux backs a rewire of clcommon.TmuxCommand. It owns:
//   - a fake "alive sessions" table answered by has-session,
//   - a recorded log of send-keys for assertions.
//
// Tests install the rewire themselves (not the harness) — rewire's
// scanner only walks `_test.go` files for `rewire.Func` calls, so a
// helper that lives in a regular `.go` file would be invisible. The
// test calls `rewire.Func(t, clcommon.TmuxCommand, w.Tmux.Command)`
// once at the top of the scenario.
//
// All fields are guarded by mu so background goroutines (the
// runSpawnPostInit injector loop) can write while tests poll.
type FakeTmux struct {
	mu      sync.Mutex
	alive   map[string]bool
	sentLog []SentKey
}

func newFakeTmux() *FakeTmux {
	return &FakeTmux{alive: map[string]bool{}}
}

// Command is the rewire replacement signature for clcommon.TmuxCommand.
// It records what it saw and returns a no-op *exec.Cmd whose Run()
// exits 0 (or 1 for has-session against an unknown session, so the
// production session.IsTmuxSessionAlive logic continues to work).
func (f *FakeTmux) Command(args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case len(args) >= 3 && args[0] == "has-session" && args[1] == "-t":
		if f.alive[args[2]] {
			return exec.Command("true")
		}
		return exec.Command("false")
	case len(args) >= 4 && args[0] == "send-keys" && args[1] == "-t":
		f.sentLog = append(f.sentLog, SentKey{Target: args[2], Text: args[3]})
	case len(args) >= 3 && args[0] == "kill-session" && args[1] == "-t":
		delete(f.alive, args[2])
	}
	return exec.Command("true")
}

// MarkAlive flags the named tmux session as live so subsequent
// has-session checks report it up.
func (f *FakeTmux) MarkAlive(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[name] = true
}

// MarkOffline flips a previously-alive tmux session off.
func (f *FakeTmux) MarkOffline(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.alive, name)
}

// Sent returns a snapshot of every send-keys recorded so far.
func (f *FakeTmux) Sent() []SentKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentKey, len(f.sentLog))
	copy(out, f.sentLog)
	return out
}

// WaitForSendKeys blocks until at least one send-keys recorded so far
// matches target and contains substring `contains`. Returns true on
// match, false on timeout. Used by flow tests to wait on the
// background runSpawnPostInit goroutine.
func (f *FakeTmux) WaitForSendKeys(target, contains string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, sk := range f.sentLog {
			if sk.Target == target && strings.Contains(sk.Text, contains) {
				f.mu.Unlock()
				return true
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
