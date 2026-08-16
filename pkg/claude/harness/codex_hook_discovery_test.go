package harness

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
)

func TestSelectCodexHookTrustEntriesUsesAuthoritativeHashes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	_, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:         "session-end-hash",
		TargetKind:   db.StandingTargetGroup,
		GroupID:      1,
		Summary:      "finish",
		TriggerEvent: db.StandingTriggerHookEvent,
		HookSelectors: []hookevents.Selector{{
			Harness: hookevents.HarnessCodex,
			Event:   "SessionEnd",
		}},
		Timing: db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways,
		Enabled: true,
	})
	require.NoError(t, err)

	hooksPath := filepath.Join(t.TempDir(), "hooks.json")
	want := "tclaude session hook-callback"
	hooks := make([]codexDiscoveredHook, 0, len(desiredCodexHookEvents()))
	for i, event := range desiredCodexHookEvents() {
		rpcName := codexHookEventRPCNames[event]
		hash := fmt.Sprintf("sha256:%064x", i+1)
		if event == "SessionEnd" {
			// Codex 0.147 normalizes this event to a one-second timeout, unlike
			// the other events. The authoritative value must pass through
			// untouched rather than being recomputed from tclaude assumptions.
			hash = "sha256:d385ee0c1ef3c51d6ca7e45bff0e99cc3ed590539c831eadcc10ecaa8fa88616"
		}
		hooks = append(hooks, codexDiscoveredHook{
			Key:       fmt.Sprintf("%s:%s:0:0", hooksPath, testCodexHookEventLabels[event]),
			EventName: rpcName, Command: &want, SourcePath: hooksPath, CurrentHash: hash,
		})
	}

	entries, err := selectCodexHookTrustEntries(codexHooksListResult{
		Data: []codexHooksListEntry{{Hooks: hooks}},
	}, hooksPath, want)
	require.NoError(t, err)
	require.Len(t, entries, len(hooks))
	assert.Contains(t, entries, codexHookTrustEntry{
		Key:  hooksPath + ":session_end:0:0",
		Hash: "sha256:d385ee0c1ef3c51d6ca7e45bff0e99cc3ed590539c831eadcc10ecaa8fa88616",
	})
}

func TestSelectCodexHookTrustEntriesFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	hooksPath := filepath.Join(t.TempDir(), "hooks.json")
	want := "tclaude session hook-callback"
	wrong := "other command"

	_, err := selectCodexHookTrustEntries(codexHooksListResult{
		Data: []codexHooksListEntry{{Hooks: []codexDiscoveredHook{{
			Key: hooksPath + ":pre_tool_use:0:0", EventName: "preToolUse",
			Command: &wrong, SourcePath: hooksPath,
			CurrentHash: "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		}}}},
	}, hooksPath, want)
	require.ErrorContains(t, err, "current tclaude hook not returned")
}
