package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// mergeSnapshotInlineProfile splits a template agent's template-local profile
// into OBSERVABLE fields (the live group wins) and curated non-observable ones
// (the stored template carries forward). The startup-context trims are
// observable — recorded on the session row — which puts them in the first group
// and means an observed "trims nothing" has to be able to CLEAR the template's
// previous trims.
//
// The trap: harness.ResolveContextFeatures returns nil for an all-default map, so
// "traced, no trims" and "could not trace it" both arrive as an empty map. Keying
// the carry-forward on len()==0 would conflate them and make a template's trims
// permanently unclearable. The snapshot pass therefore writes a non-nil empty map
// for a member it observed, and these pin that distinction.

func TestMergeSnapshotInlineProfile_ObservedEmptyTrimsClearsStoredTrims(t *testing.T) {
	prev := &db.SpawnProfile{
		Model:           "opus",
		ContextFeatures: map[string]string{"bundled-skills": "off"},
	}
	// Traced, and the live member trims nothing — a non-nil empty map.
	traced := &db.SpawnProfile{Model: "sonnet", ContextFeatures: map[string]string{}}

	out := mergeSnapshotInlineProfile(prev, traced)
	if assert.NotNil(t, out) {
		assert.Empty(t, out.ContextFeatures,
			"un-trimming the live roster and re-snapshotting must clear the template's trims")
		assert.Equal(t, "sonnet", out.Model, "observable fields still take the live value")
	}
}

func TestMergeSnapshotInlineProfile_UnobservedTrimsKeepStoredTrims(t *testing.T) {
	prev := &db.SpawnProfile{
		Model:           "opus",
		ContextFeatures: map[string]string{"bundled-skills": "off"},
	}
	// Could not observe the member (a pruned session row) — nil, not empty. The
	// curated template value must survive rather than being silently widened.
	traced := &db.SpawnProfile{Model: "sonnet"}

	out := mergeSnapshotInlineProfile(prev, traced)
	if assert.NotNil(t, out) {
		assert.Equal(t, map[string]string{"bundled-skills": "off"}, out.ContextFeatures,
			"an unobservable member must not drop the template's curated trims")
	}
}

func TestMergeSnapshotInlineProfile_ObservedTrimsWin(t *testing.T) {
	prev := &db.SpawnProfile{ContextFeatures: map[string]string{"bundled-skills": "off"}}
	traced := &db.SpawnProfile{ContextFeatures: map[string]string{"artifact": "off"}}

	out := mergeSnapshotInlineProfile(prev, traced)
	if assert.NotNil(t, out) {
		assert.Equal(t, map[string]string{"artifact": "off"}, out.ContextFeatures,
			"the live member's trims replace the template's — no union of the two")
	}
}

func TestMergeSnapshotInlineProfile_PreservesStartupContext(t *testing.T) {
	prev := &db.SpawnProfile{StartupContext: "Keep the curated model guidance."}
	out := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Model: "sonnet"})
	require.NotNil(t, out)
	assert.Equal(t, "Keep the curated model guidance.", out.StartupContext)
}

// The Copilot context cap is observable, but 0 is ambiguous — a member pinning
// no cap and an untraceable one both read 0 — so the merge splits it: a traced
// cap wins, a traced zero falls back to the template's curated one. The fallback
// half is pinned structurally by
// TestMergeSnapshotInlineProfileCarriesEveryCuratedField; this is the half a
// structural guard cannot see, because it is about which side wins rather than
// about the field being handled at all.
func TestMergeSnapshotInlineProfile_TracedContextWindowMaxWins(t *testing.T) {
	prev := &db.SpawnProfile{ContextWindowMax: 100_000}
	out := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{ContextWindowMax: 272_000})
	require.NotNil(t, out)
	assert.Equal(t, int64(272_000), out.ContextWindowMax,
		"a member relaunched with a different cap must re-snapshot with the live value")
}

// The ticket's own case: a member still traced as Copilot that does not
// currently pin a cap must not cost the template its curated one.
func TestMergeSnapshotInlineProfile_ContextWindowMaxCarriesForACopilotMember(t *testing.T) {
	prev := &db.SpawnProfile{Harness: harness.CopilotName, ContextWindowMax: 272_000}
	out := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Harness: harness.CopilotName, Model: "gpt-5"})
	require.NotNil(t, out, "a profile pinning only a Copilot cap must survive the merge")
	assert.Equal(t, int64(272_000), out.ContextWindowMax)
}

// ...but the cap travels with its harness. Harness is traced-wins, so an
// untraceable member (a pruned session row) merges to a BLANK harness, which
// resolves to Claude — and resolveIntLaunchField treats a blank-harness inline
// profile as a matching tier, so a cap riding across would 400 the whole agent at
// deploy with invalid_context_window_max rather than being ignored.
//
// This is a deliberately chosen residual, not an oversight: an untraceable
// Copilot member still loses its curated cap, which is the original TCL-1062
// defect in the population that motivated the gate. Losing a meter denominator
// is preferred to emitting a template-local profile that cannot deploy at all.
func TestMergeSnapshotInlineProfile_ContextWindowMaxDoesNotOutliveItsHarness(t *testing.T) {
	prev := &db.SpawnProfile{
		Harness:          harness.CopilotName,
		ContextWindowMax: 272_000,
		StartupContext:   "curated guidance",
	}
	out := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{})
	require.NotNil(t, out)
	assert.Zero(t, out.ContextWindowMax,
		"a Copilot cap must not ride across onto a profile whose harness pin was just dropped")
	assert.Equal(t, "curated guidance", out.StartupContext,
		"the gate is specific to the cap — harness-agnostic curated fields still carry")
}

func TestTraceMemberLaunchMarksAnObservedEmptyTrimSet(t *testing.T) {
	// The Set bit is what lets the snapshot pass write a non-nil empty map. It is
	// only meaningful alongside a real DB, so this asserts the zero value: an
	// untraceable member leaves it false, which is what preserves prev above.
	var launch templateAgentLaunch
	assert.False(t, launch.ContextFeaturesSet,
		"a launch that was never traced must not claim to have observed a trim set")
	assert.Empty(t, launch.ContextFeatures)
}
