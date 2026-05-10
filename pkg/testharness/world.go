//go:build rewire

package testharness

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// World is the per-test scaffolding bundle. Construction is via New(t)
// and cleanup is auto-registered through t.Cleanup, so individual
// tests don't need a Close call. The FakeTmux / CCSimulator are public
// so flow tests can directly drive them.
//
// Deliberately *no* http.Handler / agentd reference: the daemon's
// package owns the mux, and importing it from here would create a
// cycle when flow tests in `package agentd` import testharness back.
// Instead, http.go provides handler-agnostic Serve / JSONRequest
// helpers that the test wires to its own mux.
type World struct {
	HomeDir string
	Tmux    *FakeTmux
	CC      *CCSimulator
}

// New builds a World wired to a fresh tmpdir HOME, a clean test DB,
// and a FakeTmux ready to be rewired into clcommon.TmuxCommand.
//
// The harness does NOT install the rewire itself: rewire's scanner
// walks `_test.go` files for `rewire.Func` calls, so the test must
// own the install. One line at the top of the scenario:
//
//	rewire.Func(t, clcommon.TmuxCommand, w.Tmux.Command)
//
// Test code creates the agent groups / messages / etc. it needs
// after New returns. The harness deliberately does not pre-populate
// fixtures; scenarios stay readable when their setup is local.
func New(t *testing.T) *World {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	db.ResetForTest()

	w := &World{
		HomeDir: home,
		Tmux:    newFakeTmux(),
	}
	w.CC = newCCSimulator(w)
	return w
}
