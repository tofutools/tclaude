//go:build rewire

package testharness

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// CCSimulator drives the synthetic Claude Code side of the agentd
// flows: writes the session row that handleGroupSpawn polls for,
// marks the new tmux pane alive, and (in later phases) will fire
// hook callbacks + write .jsonl turns.
//
// It holds a back-reference to the World so handlers can flip
// FakeTmux state without the test plumbing it through every call.
type CCSimulator struct {
	w *World
}

func newCCSimulator(w *World) *CCSimulator {
	return &CCSimulator{w: w}
}

// MaterializeSpawn synthesises the side effect that, in production,
// the freshly-spawned `tclaude session new` would have produced: a
// SessionRow keyed by label with a generated conv-id + tmux session
// name, and a FakeTmux entry marking that session alive.
//
// Returns (convID, tmuxSession). Use from inside a rewire of
// spawnDetachedTclaudeNew so the handler's poll loop finds the row
// on its first iteration. Tests that want to drive the timing
// themselves can call this manually after a delayed-mock setup.
func (s *CCSimulator) MaterializeSpawn(t *testing.T, label, cwd string) (convID, tmuxSession string) {
	t.Helper()
	convID = "synth-" + label + "-" + randomHex(8)
	tmuxSession = "tclaude-" + label
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Cwd:         cwd,
		Status:      "running",
	}); err != nil {
		t.Fatalf("CCSimulator.MaterializeSpawn: SaveSession: %v", err)
	}
	s.w.Tmux.MarkAlive(tmuxSession)
	return convID, tmuxSession
}

// MaterializeConvID writes a SessionRow that maps an existing label
// to a caller-chosen conv-id. Lower-level than MaterializeSpawn —
// tests use it when they want to script the conv-id explicitly.
func (s *CCSimulator) MaterializeConvID(t *testing.T, label, convID, tmuxSession string) {
	t.Helper()
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Status:      "running",
	}); err != nil {
		t.Fatalf("CCSimulator.MaterializeConvID: SaveSession: %v", err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n/2+1)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
