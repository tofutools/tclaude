package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestCompleteDirPath(t *testing.T) {
	root := t.TempDir()
	mustMkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("project-alpha")
	mustMkdir("project-beta")
	mustMkdir("project-alpha/sub")
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("unambiguous match completes with trailing slash", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "project-b"))
		want := filepath.Join(root, "project-beta") + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("ambiguous match extends to common prefix and lists candidates", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "project-"))
		want := filepath.Join(root, "project-")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		wantCandidates := []string{"project-alpha", "project-beta"}
		if len(candidates) != len(wantCandidates) || candidates[0] != wantCandidates[0] || candidates[1] != wantCandidates[1] {
			t.Errorf("candidates = %v, want %v", candidates, wantCandidates)
		}
	})

	t.Run("no match leaves input unchanged", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "nope"))
		want := filepath.Join(root, "nope")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("files are not offered as directory completions", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "not-a"))
		want := filepath.Join(root, "not-a")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("trailing slash lists all subdirectories", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "project-alpha") + "/")
		want := filepath.Join(root, "project-alpha") + "/sub/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("bare tilde completes to home with trailing slash", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory available")
		}
		completed, candidates := completeDirPath("~")
		want := home + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})
}

func TestExpandHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}

	cases := map[string]string{
		"~":        home,
		"~/foo":    filepath.Join(home, "foo"),
		"/etc":     "/etc",
		"relative": "relative",
		"~foo/bar": "~foo/bar", // not a home-relative path (no leading "~/")
	}
	for in, want := range cases {
		if got := expandHomePrefix(in); got != want {
			t.Errorf("expandHomePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

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
