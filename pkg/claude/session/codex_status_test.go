package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Codex fires no SessionEnd hook, so a Codex session never reaches
// StatusExited through the event switch. Exit detection instead falls to
// the liveness check the reaper and `session ls` share
// (RefreshSessionStatus: tmux has-session → PID). That liveness path is
// itself harness-agnostic — it never reads state.Harness — so this pins
// the generic no-SessionEnd fallback, with Codex as the motivating case
// (Claude Code still gets a SessionEnd; Codex relies solely on this).
// Both directions: a dead row is marked exited, a live one is left on its
// hook-driven status.
//
// FindClaudePID — used elsewhere to refresh a stale PID — only matches
// "claude"/"node", so it can't relocate a Codex process. A Codex row's
// liveness therefore rests on tmux has-session (exercised here via the
// no-tmux/no-PID fallback). NOTE for the future Codex spawn slice: because
// the PID can't be re-found, a Codex row must be created with either a
// live tmux session or a real PID — a non-tmux row left at PID 0 would be
// reaped as a false-positive on the first sweep.
func TestRefreshSessionStatus_CodexNoSessionEndExitDetection(t *testing.T) {
	t.Run("dead session (no tmux, no live process) → exited", func(t *testing.T) {
		st := &SessionState{
			ID:          "codex-dead",
			Harness:     "codex",
			Status:      StatusWorking,
			TmuxSession: "", // tmux pane gone
			PID:         0,  // no tracked process
		}
		RefreshSessionStatus(st)
		assert.Equal(t, StatusExited, st.Status,
			"a Codex session with no tmux + no process is exited (the SessionEnd substitute)")
	})

	t.Run("live process is not reaped", func(t *testing.T) {
		st := &SessionState{
			ID:          "codex-live",
			Harness:     "codex",
			Status:      StatusAwaitingPermission,
			TmuxSession: "",
			PID:         os.Getpid(), // this test process is alive
		}
		RefreshSessionStatus(st)
		assert.Equal(t, StatusAwaitingPermission, st.Status,
			"a Codex session whose process is alive keeps its hook-driven status")
	})
}
