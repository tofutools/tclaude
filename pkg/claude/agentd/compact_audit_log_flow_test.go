package agentd_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A "spontaneous /compact" landing in an agent's pane must always be
// traceable to its source. send-keys is the one channel through which
// tclaude can make a pane run a command the agent did not type itself, so
// every lifecycle-slash injection now logs a line to ~/.tclaude/output.log
// naming the conv and what caused it. This drives the dashboard/human
// compact path (no calling conv → reason "human/dashboard") end to end
// through the daemon mux against a CC pane and asserts BOTH surfaces: the
// /compact reaching the pane AND the audit line that records why.
func TestCompact_LogsInjectionWithReason(t *testing.T) {
	// Capture slog for the duration of this test; restore on cleanup.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusOK, res.Code,
		"human compact on a CC agent should succeed; body=%s", res.Raw)

	// Surface 1: /compact actually reached the pane.
	f.AssertSentContains("tmux-cc-1:0.0", "/compact", 10*time.Second)

	// Surface 2: the injection is recorded with the affected conv and its cause.
	logs := buf.String()
	assert.Contains(t, logs, "slash-command injected via send-keys",
		"the compaction injection must be logged for after-the-fact diagnosis")
	assert.Contains(t, logs, `reason="compact (human/dashboard)"`,
		"the audit line must record what caused the compaction")
	assert.Contains(t, logs, conv,
		"the audit line must name the affected conversation")
}

// Scenario: the operator picks compact in an agent's ⚙ menu. The dashboard route
// is the same compact path, so the command reaches the pane and the audit line
// names the human.
func TestCompact_DashboardRouteInjectsAsHuman(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-5555-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-2", "tmux-cc-2", "/work")
	f.HaveMember("crew", conv)

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/compact", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.AssertSentContains("tmux-cc-2:0.0", "/compact", 10*time.Second)
	assert.Contains(t, buf.String(), `reason="compact (human/dashboard)"`)

	get := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/compact", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, get.Code)
}

// Scenario: a harness with no compact command. The menu hides the item, and a
// stale page that still posts is refused rather than typing into the pane.
func TestCompact_DashboardRouteRefusesHarnessWithoutCompact(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-6666-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-3", "tmux-cc-3", "/work")
	f.HaveMember("crew", conv)
	row, err := db.LoadSession("cc-3")
	require.NoError(t, err)
	row.Harness = harness.ShellName
	require.NoError(t, db.SaveSession(row))

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/compact", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "does not support compact")
}
