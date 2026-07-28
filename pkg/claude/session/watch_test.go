package session

import (
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A freshly spawned Codex agent can have no conv_index title row yet. The
// dashboard and conv list already fall back to agents.pending_name; the live
// sessions view must show the same name instead of "-".
func TestWatchView_UsesPendingAgentName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	const convID = "11111111-1111-1111-1111-111111111111"
	m := initialModel(false, nil, nil)
	m.width = 160
	m.height = 40
	m.allSessions = []*SessionState{{
		ID:      "spwn-123456",
		ConvID:  convID,
		Cwd:     "/repo",
		Status:  StatusIdle,
		Updated: time.Now(),
	}}
	m.sessions = m.allSessions
	m.pendingNames = map[string]string{convID: "codex-worker"}

	if got := m.View().Content; !strings.Contains(got, "codex-worker") {
		t.Fatalf("sessions view did not render the pending agent name:\n%s", got)
	}
}

// A selection-bound confirm dialog is pinned to the session it was opened
// for and must not outlive it: the watch TUI's 500ms tick re-sorts and
// re-filters rows under an open dialog, so (a) the dialog acts on
// confirmTarget rather than whatever drifted onto the cursor row, and (b)
// a refresh that drops the target from the visible list clears the dialog
// instead of leaving a blank prompt that swallows keys. confirmQuit is
// selection-independent and survives.
func TestWatchConfirm_ClearedWhenTargetVanishes(t *testing.T) {
	s1 := &SessionState{ID: "aaaa-1111", TmuxSession: "one"}
	s2 := &SessionState{ID: "bbbb-2222", TmuxSession: "two"}

	m := model{allSessions: []*SessionState{s1, s2}}
	m.confirmMode = confirmKill
	m.confirmTarget = s1

	// Target still visible (any row) → dialog survives the refresh.
	m = m.applySearchFilter()
	if m.confirmMode != confirmKill || m.confirmTarget == nil {
		t.Fatalf("dialog must survive while its target is listed; mode=%v target=%v", m.confirmMode, m.confirmTarget)
	}

	// Target vanishes from the list (exited row pruned / filtered away) →
	// dialog clears rather than rendering blank and eating keys.
	m.allSessions = []*SessionState{s2}
	m = m.applySearchFilter()
	if m.confirmMode != confirmNone || m.confirmTarget != nil {
		t.Fatalf("dialog must clear when its target vanishes; mode=%v target=%v", m.confirmMode, m.confirmTarget)
	}

	// confirmQuit has no selection to go stale — an empty list keeps it.
	q := model{}
	q.confirmMode = confirmQuit
	q = q.applySearchFilter()
	if q.confirmMode != confirmQuit {
		t.Fatalf("confirmQuit must survive refreshes; mode=%v", q.confirmMode)
	}
}
