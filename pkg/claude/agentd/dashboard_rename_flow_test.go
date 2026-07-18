package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the dashboard's agent-rename endpoint —
// POST /api/agents/{conv}/rename, the cookie-authenticated twin of
// POST /v1/agent/{conv}/rename.
//
// Two dashboard surfaces drive renaming, and BOTH issue the exact same
// request to this endpoint:
//   - the per-agent edit panel (editMemberModal) — its Save POSTs
//     {title: "..."} for an explicit rename, or {auto: true} to ask
//     the agent to pick its own title;
//   - the click-to-edit agent-name cell (the rename-name handler) —
//     its Enter POSTs {title: "..."}.
//
// The UI difference between the two is purely client-side JS, so a
// single endpoint test covers both paths; splitting it into two
// byte-identical Go tests would add no coverage. The scenarios below
// cover the explicit-title path, the auto path, and the charset-gate
// rejection.

// renameDashMux sets a popup base URL so the dashboard auth's Origin
// pin is satisfiable, and returns the dashboard mux.
func renameDashMux(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return agentd.BuildDashboardHandlerForTest()
}

// postAgentRename POSTs /api/agents/{conv}/rename through the
// dashboard mux. (groups_rename_flow_test.go owns the group-rename
// `postRename`; this is the per-agent twin.)
func postAgentRename(t *testing.T, mux http.Handler, conv string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+conv+"/rename", body))
}

// sentRenameTo reports whether any recorded send-keys delivered a
// `/rename` slash command to the given tmux pane.
func sentRenameTo(sent []testharness.SentKey, pane string) bool {
	for _, sk := range sent {
		if sk.Target == pane && strings.Contains(sk.Text, "/rename") {
			return true
		}
	}
	return false
}

// dashMemberTitle pulls one member's title out of the /api/snapshot
// group list — the surface the dashboard's group rows render the agent
// name from.
func dashMemberTitle(t *testing.T, group, conv string) string {
	t.Helper()
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, m := range g.Members {
			if m.ConvID == conv {
				return m.Title
			}
		}
	}
	t.Fatalf("member %s missing from group %q in /api/snapshot", conv, group)
	return ""
}

// Scenario: the explicit-title rename. The edit panel's Save and the
// click-to-edit name cell both POST {title} to /api/agents/{conv}/
// rename; the daemon injects `/rename <title>` into the live pane and
// the new name lands on every read surface — `tclaude agent groups
// members` and the dashboard's own /api/snapshot group rows.
func TestDashboardRename_SetsTitle(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-bbbb-cccc-dddd-000000000001"
	const tmux = "tclaude-spwn-rena"
	f.HaveAliveSession(conv, "spwn-rena", tmux, f.TestCwd("work"))
	// Give the agent a starting name so this is a genuine rename, not
	// a first-naming — the .jsonl scan resolves it the way production
	// resolves an agent's title.
	require.NoError(t, f.World.CCs.GetByConvID(conv).WriteCustomTitle("worker-old"))
	g := f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.AssertGroupMember(g.Name, conv, "worker-old", 5*time.Second)

	mux := renameDashMux(t)
	rec := postAgentRename(t, mux, conv, map[string]any{"title": "worker-new"})
	require.Equalf(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())

	// `/rename` was injected into the agent's pane.
	f.AssertSentContains(tmux+":0.0", "/rename worker-new", 5*time.Second)

	// The new title converges on the members surface (refreshed from
	// the .jsonl) once CC processes the injected /rename.
	f.AssertGroupMember(g.Name, conv, "worker-new", 5*time.Second)

	// ...and the dashboard's own /api/snapshot group row shows it too.
	assert.Equal(t, "worker-new", dashMemberTitle(t, g.Name, conv),
		"the renamed title must surface on the dashboard snapshot")
}

// Scenario: Codex renames are out-of-band writes to Codex's native
// threads.title store, not `/rename` text typed into the pane. The dashboard
// snapshot is cache-only, so the rename path must refresh conv_index after
// the native write; otherwise the UI keeps rendering the old cached title
// until some unrelated full conversation scan happens.
func TestDashboardRename_CodexUpdatesCachedTitle(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-bbbb-cccc-dddd-000000000004"
	const tmux = "tclaude-spwn-rencdx"
	cx := f.HaveAliveCodexSession(conv, "spwn-rencdx", tmux, f.TestCwd("work"))
	require.NoError(t, cx.WriteThreadRow(testharness.CodexThreadSeed{
		Title:            "codex-old",
		FirstUserMessage: "hello from codex",
		Cwd:              f.TestCwd("work"),
	}))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      conv,
		CustomTitle: "codex-old",
		FirstPrompt: "hello from codex",
		ProjectPath: f.TestCwd("work"),
		Harness:     "codex",
	}), "seed stale dashboard title cache")
	mux := renameDashMux(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", conv)
	assert.Equal(t, "codex-old", dashMemberTitle(t, g.Name, conv),
		"precondition: dashboard reads the cached Codex title")

	rec := postAgentRename(t, mux, conv, map[string]any{"title": "codex-new"})
	require.Equalf(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())

	got, err := cx.ThreadTitle()
	require.NoError(t, err)
	assert.Equal(t, "codex-new", got, "Codex native title store updated")
	assert.False(t, sentRenameTo(f.World.Tmux.Sent(), tmux+":0.0"),
		"Codex rename must not inject /rename; sent=%+v", f.World.Tmux.Sent())
	assert.Equal(t, "codex-new", dashMemberTitle(t, g.Name, conv),
		"dashboard cache must reflect the out-of-band Codex rename")
}

// Scenario: the edit panel's "auto" checkbox POSTs {auto: true}. The
// daemon does NOT set a title — it injects a [system: …] nudge asking
// the agent to rename itself. The conversation title is untouched
// until the agent acts on the nudge on its own next turn.
func TestDashboardRename_AutoNudge(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-bbbb-cccc-dddd-000000000002"
	const tmux = "tclaude-spwn-renc"
	f.HaveAliveSession(conv, "spwn-renc", tmux, f.TestCwd("work"))
	require.NoError(t, f.World.CCs.GetByConvID(conv).WriteCustomTitle("worker-keepme"))
	g := f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.AssertGroupMember(g.Name, conv, "worker-keepme", 5*time.Second)

	mux := renameDashMux(t)
	rec := postAgentRename(t, mux, conv, map[string]any{"auto": true})
	require.Equalf(t, http.StatusOK, rec.Code, "auto-rename body=%s", rec.Body.String())

	// A self-rename nudge — not a /rename — was injected.
	f.AssertSentContains(tmux+":0.0", "rename yourself", 5*time.Second)
	assert.False(t, sentRenameTo(f.World.Tmux.Sent(), tmux+":0.0"),
		"auto mode must not inject a /rename; sent=%+v", f.World.Tmux.Sent())

	// The title is unchanged — the agent picks its own on a later turn.
	// dashMemberTitle fatals if the member is missing, so a vanished
	// member can't let this assertion silently pass.
	assert.Equal(t, "worker-keepme", dashMemberTitle(t, g.Name, conv),
		"auto mode must not change the title itself")
}

// Scenario: a title that fails the rename charset gate is rejected with
// 400 — the daemon's isValidRenameTitle guard — and nothing is injected
// into the pane. The slash in the title is the keystroke-injection
// vector the gate exists to block.
func TestDashboardRename_InvalidTitleRejected(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-bbbb-cccc-dddd-000000000003"
	const tmux = "tclaude-spwn-rend"
	f.HaveAliveSession(conv, "spwn-rend", tmux, f.TestCwd("work"))
	require.NoError(t, f.World.CCs.GetByConvID(conv).WriteCustomTitle("worker-safe"))
	g := f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.AssertGroupMember(g.Name, conv, "worker-safe", 5*time.Second)

	mux := renameDashMux(t)
	rec := postAgentRename(t, mux, conv, map[string]any{"title": "bad/slashed"})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"a slash in the title must be rejected; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_title",
		"the rejection must be the title charset gate, not some other 400")

	// A rejected rename never reaches the pane.
	assert.False(t, sentRenameTo(f.World.Tmux.Sent(), tmux+":0.0"),
		"a rejected rename must inject nothing; sent=%+v", f.World.Tmux.Sent())

	// The title is left exactly as it was. dashMemberTitle fatals if
	// the member is missing, so this can't silently pass.
	assert.Equal(t, "worker-safe", dashMemberTitle(t, g.Name, conv),
		"a rejected rename must not touch the title")
}
