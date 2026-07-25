package agentd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TCL-729 launch-boundary coverage: a spawn must RESOLVE and RECORD the
// OS-sandbox verdict, not merely be able to.
//
// The sibling dashboard_os_sandbox_flow_test.go stamps its rows by hand to
// exercise the read path, which means it would pass even if no launch ever wrote
// a verdict — the whole user-visible point of the feature. These drive a real
// spawn through the daemon and assert on the row it produced.
//
// The flow World points $HOME at a temp dir and (since this feature) the
// managed-policy root at another, so the entire precedence chain is hermetic:
// writing a settings.json below is the only thing that decides the verdict.

// writeClaudeSettings writes a user-tier ~/.claude/settings.json in the flow
// World's isolated home.
func writeClaudeSettings(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600))
}

// rowForConv returns the session row a spawn produced.
func rowForConv(t *testing.T, convID string) *db.SessionRow {
	t.Helper()
	row, err := db.FindSessionByConvID(convID)
	require.NoError(t, err)
	require.NotNil(t, row, "spawn %s produced no session row", convID)
	return row
}

// The reported bug, end to end: a default Claude spawn chooses no sandbox mode,
// but the operator's settings.json enables the OS sandbox. The launch must
// record that, or the dashboard has nothing to badge.
func TestClaudeSpawn_RecordsInheritedOSSandboxVerdict(t *testing.T) {
	f := newFlow(t)
	writeClaudeSettings(t, f.World.HomeDir, `{"sandbox":{"enabled":true}}`)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "confined-worker", "claude")

	row := rowForConv(t, spawn.ConvID)
	assert.Equal(t, "on", row.OSSandboxState,
		"a launch under the operator's sandbox-enabling settings must record that it is confined")
	assert.NotEmpty(t, row.OSSandboxSource, "the deciding settings file must be named")
	assert.False(t, row.OSSandboxUnverified, "every settings tier was readable")
	// The daemon applies the harness default, so the recorded mode is the literal
	// string "inherit" — i.e. "whatever settings.json says". That is precisely the
	// value the badge used to skip, and precisely why it cannot answer the
	// question the verdict above answers.
	assert.Equal(t, "inherit", row.SandboxMode,
		"the mode stays the unchosen request")
}

// The other half of the bug: the same default spawn with nothing configured must
// record "unconfigured", NOT "off" and not an empty verdict. Empty would be
// indistinguishable from a pre-column row, and "off" would blame a settings file
// that does not exist.
func TestClaudeSpawn_RecordsUnconfiguredOSSandboxVerdict(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	row := rowForConv(t, spawn.ConvID)
	assert.Equal(t, "unconfigured", row.OSSandboxState,
		"nothing enabled the sandbox and nothing disabled it")
	assert.Empty(t, row.OSSandboxSource, "nothing decided it, so nothing is named")
}

// An unreadable settings file that OUTRANKS the deciding tier must be recorded
// as doubt. Without this the badge asserts containment in precisely the case
// tclaude could not verify it — the worst failure mode this feature has, since a
// padlock on an unconfined agent is worse than no padlock.
func TestClaudeSpawn_RecordsUnverifiedWhenAHigherTierIsUnreadable(t *testing.T) {
	f := newFlow(t)
	// A user tier that says "sandboxed"...
	writeClaudeSettings(t, f.World.HomeDir, `{"sandbox":{"enabled":true}}`)
	// ...under a project tier that outranks it and cannot be parsed. Claude Code
	// would read this file; tclaude cannot tell what it says.
	cwd := f.TestCwd("proj")
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, ".claude", "settings.json"), []byte("{not json"), 0o600))
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "doubtful-worker", "harness": "claude", "cwd": cwd,
	})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", spawn.Raw)

	row := rowForConv(t, spawn.ConvID)
	assert.Equal(t, "on", row.OSSandboxState, "the readable tier still decides")
	assert.True(t, row.OSSandboxUnverified,
		"a higher-precedence file tclaude could not parse must be recorded as doubt, not silently ignored")
}

// A harness whose recorded mode already IS its posture records no verdict, so a
// Codex spawn is untouched by this feature — even with a Claude settings.json
// sitting in the same home that would otherwise resolve to "on".
func TestCodexSpawn_RecordsNoOSSandboxVerdict(t *testing.T) {
	f := newFlow(t)
	writeClaudeSettings(t, f.World.HomeDir, `{"sandbox":{"enabled":true}}`)
	f.HaveGroup("cdx-crew")

	spawn := f.AsHuman().SpawnHarness("cdx-crew", "codex-worker", "codex")

	row := rowForConv(t, spawn.ConvID)
	assert.Empty(t, row.OSSandboxState,
		"Codex records no verdict — its --sandbox mode is its posture, and it never reads Claude's settings.json")
	assert.Empty(t, row.OSSandboxSource)
}

// A resume is a fresh launch, so it must RE-RESOLVE rather than carry the
// predecessor's verdict. This is the claim the PR description and docs make, and
// it is the one that silently rots: an operator who installs the sandbox
// hardening and resumes an agent should see the agent become badged.
func TestClaudeResume_ReResolvesOSSandboxVerdict(t *testing.T) {
	f := newFlow(t)

	const conv = "05b1e0ff-1111-4222-8333-444444444444"
	const label = "spwn-osbx-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	// The operator installs the sandbox hardening between the two launches.
	writeClaudeSettings(t, f.World.HomeDir, `{"sandbox":{"enabled":true}}`)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	row := rowForConv(t, conv)
	assert.Equal(t, "on", row.OSSandboxState,
		"a resume re-resolves against current settings instead of inheriting the stale verdict")
	assert.NotEmpty(t, row.OSSandboxSource)
}
