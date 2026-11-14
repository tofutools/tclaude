package agentd_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The CLI reads the way a person reads the board: rows numbered as the
// dashboard numbers them, agents by name in the lock and the history, the
// needs-you count broken down, a no-group listing that covers every board,
// and a commented lines file.
func TestTaskboardCLIReadsLikeTheDashboard(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rename := func(conv, name string) {
		h.f.HaveConvWithTitle(conv, name)
	}
	rename(h.worker, "builder")
	rename(h.lead, "lead")

	// A commented lines file: blank lines and '#' lines are skipped.
	file := filepath.Join(testutil.CanonicalTempDir(t), "lines.txt")
	require.NoError(t, os.WriteFile(file, []byte("# the next few\nfirst idea\n\n  # indented comment\nsecond idea\n"), 0o600))
	rc, out, errOut := run("add", "crew", "--lines-file", file)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "2 added to crew")
	assert.NotContains(t, out, "#")

	q1 := h.add("queued one", "next")
	q2 := h.add("queued two", "next")
	code, body := h.post("", q2.ID, "status", map[string]any{"status": "blocked", "reason": "waits"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.lead, q1.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.worker, q1.ID, "report", map[string]any{"outcome": "failed", "note": "CI image lacks bwrap"})
	require.Equal(t, http.StatusOK, code, body)

	// Numbered as one sequence, the queue then the ideas.
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, errOut)
	lines := strings.Split(out, "\n")
	var numbered []string
	for _, l := range lines {
		if n := strings.TrimSpace(l); len(n) > 2 && n[1] == '.' {
			numbered = append(numbered, n[:2])
		}
	}
	assert.Equal(t, []string{"1.", "2.", "3.", "4."}, numbered, out)

	// The peer's own task: taskboard.own assigns it to the peer as it is
	// created.
	own := h.task(h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "the peer's own"}), http.StatusCreated)

	// The lock names its writers.
	rc, _, errOut = run("set-taskboard-writers", "crew", tbAgentID(t, h.worker))
	require.Equal(t, 0, rc, errOut)
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "(locked to writers: owners, builder)")
	// A member locked out is told who can let it in.
	code, body = h.post(h.peer, own.ID, "status", map[string]any{"status": "blocked", "reason": "waiting"})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "ask a group owner to add you")
	code, body = h.post("", own.ID, "status", map[string]any{"status": "dropped"})
	require.Equal(t, http.StatusOK, code, body)

	// History names agents.
	rc, out, errOut = run("history", q1.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "assigned    lead  → builder", out)
	assert.NotContains(t, out, "agt_", "agents are named, not numbered")

	// show: a ref with no URL ends cleanly, and the report reads with a dash.
	rc, _, errOut = run("edit", q1.ID, "--ref", "tcl-abcdef")
	require.Equal(t, 0, rc, errOut)
	rc, out, errOut = run("show", q1.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "  ref:     awb tcl-abcdef\n")
	// --resolve-ref with no awb server or Linear proxy configured leaves it
	// as it is, and is refused beside --clear-ref.
	rc, _, errOut = run("edit", q1.ID, "--resolve-ref")
	require.Equal(t, 0, rc, errOut)
	rc, _, _ = run("edit", q1.ID, "--resolve-ref", "--clear-ref")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, out, "  last report: failed — CI image lacks bwrap\n")

	// The summary counts blocked tasks plainly (tk_3cb51ce0).
	stdout := new(bytes.Buffer)
	require.Equal(t, 0, agent.RunTaskboardSummary(false, stdout, new(bytes.Buffer)))
	assert.Contains(t, stdout.String(), "crew: 4 · 1 blocked · 1 next")
	assert.NotContains(t, stdout.String(), "needs you")

	// No group, as the operator: every board, its counts and its first rows.
	h.f.HaveGroup("ops")
	rc, out, errOut = run("ls")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "crew: 4 · 1 blocked")
	assert.Contains(t, out, "ops: 0")
	assert.Contains(t, out, "One board in full: tclaude agent taskboard ls <group>")

	// Only a plain ls falls back: asked for one board's rows or JSON, it
	// still says to name the group.
	for _, args := range [][]string{{"ls", "--json"}, {"ls", "--stage", "next"}, {"ls", "--all"}} {
		rc, out, errOut = run(args...)
		assert.NotEqual(t, 0, rc, "%v: %s", args, out)
		assert.Contains(t, errOut, "name a group", "%v", args)
	}

	// A note is the writer's own text: an agent id in it is left alone.
	worker := tbAgentID(t, h.worker)
	code, body = h.post("", q2.ID, "status", map[string]any{"status": "blocked", "reason": "ask " + worker})
	require.Equal(t, http.StatusOK, code, body)
	rc, out, errOut = run("history", q2.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "ask "+worker)

}
