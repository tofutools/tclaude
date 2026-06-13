package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// harnessUsesSlashContextControls gates the stopped-hook path's CC slash
// injections (/compact + the nudge naming /compact·/reincarnate) on the
// harness understanding them. Claude Code does; Codex (no registered
// Lifecycle) does not; an empty/unknown harness falls back to the legacy CC
// behaviour so the common path is never accidentally muted.
func TestHarnessUsesSlashContextControls(t *testing.T) {
	cases := []struct {
		harness string
		want    bool
	}{
		{"", true},                         // untagged ⇒ legacy CC behaviour
		{"claude", true},                   // CC has /compact
		{"codex", false},                   // Codex has no compact command
		{"definitely-not-a-harness", true}, // unknown ⇒ safe CC default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, harnessUsesSlashContextControls(c.harness),
			"harnessUsesSlashContextControls(%q)", c.harness)
	}
}

// Regression for the gate JOH-170 had to add: populating context_pct for a
// Codex session must NOT re-arm the CC auto-compact /compact injection
// against a Codex pane. With context_pct over the threshold and
// auto-compact enabled, a Stop hook on a Codex session must leave
// compact_pending unclaimed; the identical Claude Code session must claim
// it (proving the test would catch a removed gate, not just an inert path).
func TestApplyHook_StopGatesAutoCompactByHarness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("TCLAUDE_AUTO_COMPACT", "1") // 1% threshold
	db.ResetForTest()

	mk := func(id, harness string) {
		t.Helper()
		require.NoError(t, SaveSessionState(&SessionState{
			ID:      id,
			ConvID:  id + "-conv",
			Status:  StatusWorking,
			Harness: harness,
		}))
		// Put it well over the auto-compact threshold, as the statusbar /
		// rollout telemetry would.
		require.NoError(t, db.UpdateContextSnapshot(id, 80, 10000, 2000, 200000))
	}

	mk("cdx", "codex")
	mk("cld", "claude")

	stop := func(id string) {
		t.Helper()
		require.NoError(t, ApplyHook(HookCallbackInput{
			HookEventName: "Stop",
			ConvID:        id + "-conv",
		}, id))
	}
	stop("cdx")
	stop("cld")

	_, codexPending, err := db.GetCompactState("cdx")
	require.NoError(t, err)
	assert.Zero(t, codexPending, "Codex Stop must NOT claim auto-compact (no /compact injection)")

	_, claudePending, err := db.GetCompactState("cld")
	require.NoError(t, err)
	assert.Greater(t, claudePending, 0.0, "Claude Code Stop claims auto-compact as before")
}
