package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Codex fires no SessionEnd hook, so a Codex session never reaches
// StatusExited through the event switch. Exit detection instead falls to
// the liveness check the reaper and `session ls` share
// (RefreshSessionStatus: tmux has-session → PID). These assert that path
// works for a harness=codex row in both directions: a dead session is
// marked exited, a live one is left on its hook-driven status.
//
// (FindClaudePID — used elsewhere to refresh a stale PID — only matches
// "claude"/"node", so it can't relocate a Codex process. That's why a
// Codex row's liveness rests on tmux has-session, exercised here via the
// no-tmux/no-PID fallback rather than a process-name match.)
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
