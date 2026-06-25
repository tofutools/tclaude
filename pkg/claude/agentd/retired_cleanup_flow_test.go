package agentd_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// writeRetiredCleanupConfig persists an agent.retired_cleanup block to the
// test HOME's config.json so runRetiredAgentCleanup's config.Load picks it
// up — the same path production reads each sweep.
func writeRetiredCleanupConfig(t *testing.T, enabled bool, afterDays int) {
	t.Helper()
	cfg := &config.Config{Agent: &config.AgentConfig{
		RetiredCleanup: &config.RetiredCleanupConfig{Enabled: enabled, AfterDays: afterDays},
	}}
	require.NoError(t, config.Save(cfg))
}

// With the sweep enabled, a conversation retired longer than the window
// is fully torn down — its enrollment row AND its conv_index row are
// purged via the same conv.DeleteConvByID path the dashboard/CLI delete
// uses. The sweep `now` is pushed past the window so a just-retired agent
// reads as long-retired without sleeping.
func TestRetiredCleanup_DeletesLongRetired(t *testing.T) {
	f := newFlow(t)
	writeRetiredCleanupConfig(t, true, 365)

	const convID = "11111111-1111-1111-1111-111111111111"
	f.HaveConvWithTitle(convID, "Old retired agent")
	f.HaveRetiredAgent(convID)

	// Sweep 400 days in the future: cutoff = now+400d-365d = now+35d, so a
	// conv retired ~now is comfortably before the cutoff and eligible.
	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	assert.Nil(t, enr, "long-retired enrollment should be deleted")

	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	assert.Nil(t, row, "long-retired conv_index row should be purged too")
}

// A conversation retired more recently than the window survives the sweep
// — only the long-retired tail is reaped.
func TestRetiredCleanup_KeepsRecentlyRetired(t *testing.T) {
	f := newFlow(t)
	writeRetiredCleanupConfig(t, true, 365)

	const convID = "22222222-2222-2222-2222-222222222222"
	f.HaveConvWithTitle(convID, "Recently retired agent")
	f.HaveRetiredAgent(convID)

	// Sweep at real now: cutoff = now-365d, so a conv retired ~now is AFTER
	// the cutoff and must be kept.
	agentd.RunRetiredAgentCleanupForTest(time.Now())

	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	require.NotNil(t, enr, "a recently-retired agent must not be deleted")
	assert.False(t, enr.Active(), "and it stays retired")
}

// Active (non-retired) agents are never touched, no matter how old — the
// sweep only ever considers retired enrollments.
func TestRetiredCleanup_SkipsActiveAgents(t *testing.T) {
	f := newFlow(t)
	writeRetiredCleanupConfig(t, true, 365)

	const convID = "33333333-3333-3333-3333-333333333333"
	f.HaveConvWithTitle(convID, "Live agent")
	f.HaveEnrolledAgent(convID)

	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	state, err := db.EnrollmentState(convID)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, state, "an active agent must never be reaped")
}

// End-to-end on-disk + cost behaviour: a long-retired conversation with a
// real .jsonl on disk and a recorded daily cost is reaped — its .jsonl is
// removed (it no longer lists via `conv ls`) and its enrollment purged —
// while its session_cost_daily row SURVIVES with conv_id intact, so spend
// totals are never lost (conv_id is denormalised at write time). This is
// the headline safety claim of the feature.
func TestRetiredCleanup_RemovesJSONLButKeepsCost(t *testing.T) {
	f := newFlow(t)
	writeRetiredCleanupConfig(t, true, 365)

	const convID = "77777777-7777-7777-7777-777777777777"
	const label = "cost-conv-session"
	cwd := t.TempDir()
	// A real .jsonl on disk (CCSim) + a sessions row, so DeleteConvByID's
	// file-removal branch is genuinely exercised and a cost row can be
	// attributed to the conv.
	f.HaveAliveSession(convID, label, "tmux-cost-conv", cwd)
	require.NoError(t, db.UpdateSessionCost(label, 2.50)) // denormalises conv_id onto today's cost row
	f.MarkOffline("tmux-cost-conv")                       // offline so the online-skip guard doesn't fire
	f.HaveRetiredAgent(convID)

	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	// Enrollment purged and the .jsonl gone from disk (conv ls can't see it).
	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	assert.Nil(t, enr, "long-retired enrollment should be deleted")
	f.AssertConvNotListed(convID, cwd)

	// The daily cost row survives deletion, still attributed to the conv.
	rows, err := db.AllCostDailyRows()
	require.NoError(t, err)
	var found bool
	for _, r := range rows {
		if r.ConvID == convID {
			found = true
			assert.InDelta(t, 2.50, r.CostUSD, 1e-9, "recorded spend must survive deletion")
		}
	}
	assert.True(t, found, "session_cost_daily row for the deleted conv must survive (cost totals never lost)")
}

// A retired conversation whose tmux pane is somehow still alive is
// skipped even past the window — the sweep never races a live pane's
// writes to its own .jsonl during teardown (mirrors handleAgentDelete's
// refuse-while-alive guard). Near-impossible for a year-retired agent,
// but the guard is cheap and this locks it down.
func TestRetiredCleanup_SkipsOnlineRetired(t *testing.T) {
	f := newFlow(t)
	writeRetiredCleanupConfig(t, true, 365)

	const convID = "66666666-6666-6666-6666-666666666666"
	// A live session (registered in the tmux sim, so isConvOnline sees it),
	// then retire the enrollment without killing the pane.
	f.HaveAliveSession(convID, "online-retired", "tmux-online-retired", t.TempDir())
	f.HaveRetiredAgent(convID)

	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	assert.NotNil(t, enr, "a still-online retired conv must be skipped, not deleted")
}

// The feature is OPT-IN: with no config (the out-of-box default, sweep
// disabled), even a very old retired conversation is kept — today's
// keep-retired-forever behaviour is preserved until the human opts in.
func TestRetiredCleanup_DisabledByDefault(t *testing.T) {
	f := newFlow(t) // no config file → ResolvedRetiredCleanup() returns off

	const convID = "44444444-4444-4444-4444-444444444444"
	f.HaveConvWithTitle(convID, "Retired but feature off")
	f.HaveRetiredAgent(convID)

	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	assert.NotNil(t, enr, "with the sweep disabled, retired entities are kept forever")
}

// A genuine config-load failure (corrupt config.json) must SKIP the sweep,
// not fall back to defaults — but the default is "off" anyway, and more
// importantly deleting against a guessed policy is unrecoverable. Mirrors
// the audit-cleanup broken-config guard.
func TestRetiredCleanup_SkipsOnBrokenConfig(t *testing.T) {
	f := newFlow(t)

	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{ not valid json"), 0o600))

	const convID = "55555555-5555-5555-5555-555555555555"
	f.HaveConvWithTitle(convID, "Retired, broken config")
	f.HaveRetiredAgent(convID)

	agentd.RunRetiredAgentCleanupForTest(time.Now().AddDate(0, 0, 400))

	enr, err := db.GetEnrollment(convID)
	require.NoError(t, err)
	assert.NotNil(t, enr, "a broken config must skip the sweep, not delete against a guess")
}
