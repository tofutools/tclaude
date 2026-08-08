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

	out, _ := mergeSnapshotInlineProfile(prev, traced, true)
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

	out, _ := mergeSnapshotInlineProfile(prev, traced, true)
	if assert.NotNil(t, out) {
		assert.Equal(t, map[string]string{"bundled-skills": "off"}, out.ContextFeatures,
			"an unobservable member must not drop the template's curated trims")
	}
}

func TestMergeSnapshotInlineProfile_ObservedTrimsWin(t *testing.T) {
	prev := &db.SpawnProfile{ContextFeatures: map[string]string{"bundled-skills": "off"}}
	traced := &db.SpawnProfile{ContextFeatures: map[string]string{"artifact": "off"}}

	out, _ := mergeSnapshotInlineProfile(prev, traced, true)
	if assert.NotNil(t, out) {
		assert.Equal(t, map[string]string{"artifact": "off"}, out.ContextFeatures,
			"the live member's trims replace the template's — no union of the two")
	}
}

func TestMergeSnapshotInlineProfile_PreservesStartupContext(t *testing.T) {
	prev := &db.SpawnProfile{StartupContext: "Keep the curated model guidance."}
	out, _ := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Model: "sonnet"}, true)
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
	// Both sides Copilot: the cap is only legal there, and since TCL-1083 the
	// merge takes a foreign one away rather than storing a profile that cannot
	// deploy. Which side WINS is what this pins, so keep the gate out of it.
	prev := &db.SpawnProfile{Harness: harness.CopilotName, ContextWindowMax: 100_000}
	out, _ := mergeSnapshotInlineProfile(prev,
		&db.SpawnProfile{Harness: harness.CopilotName, ContextWindowMax: 272_000}, true)
	require.NotNil(t, out)
	assert.Equal(t, int64(272_000), out.ContextWindowMax,
		"a member relaunched with a different cap must re-snapshot with the live value")
}

// The ticket's own case: a member still traced as Copilot that does not
// currently pin a cap must not cost the template its curated one.
func TestMergeSnapshotInlineProfile_ContextWindowMaxCarriesForACopilotMember(t *testing.T) {
	prev := &db.SpawnProfile{Harness: harness.CopilotName, ContextWindowMax: 272_000}
	out, _ := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Harness: harness.CopilotName, Model: "gpt-5"}, true)
	require.NotNil(t, out, "a profile pinning only a Copilot cap must survive the merge")
	assert.Equal(t, int64(272_000), out.ContextWindowMax)
}

// TCL-1062 shipped a RESIDUAL here, and TCL-1083's root fix retires it. Both
// halves are pinned below, because the residual is the more interesting history.
//
// TCL-1062 gated the cap's carry-forward on the merged profile resolving to
// Copilot. An untraceable member (a pruned session row) merged to a BLANK
// harness, which resolves to Claude, so the gate dropped the cap — deliberately,
// because a cap riding onto a Claude-resolving profile does not merely sit there
// unused: resolveIntLaunchField treats a blank-harness inline profile as a
// MATCHING tier and fails it, so the member 400s at deploy and never spawns.
// Losing a meter denominator beat emitting a profile that cannot deploy. It was
// a chosen loss, but still a loss, and still silent.
//
// The root fix removes the choice rather than disclosing it: an unobserved
// member no longer has its harness pin blanked, because "we learned nothing"
// must not be written down as "this member pins no harness". The profile stays
// Copilot, so the cap is not foreign, so nothing is dropped. The residual is
// gone rather than merely visible.
func TestMergeSnapshotInlineProfile_UnobservedMemberKeepsItsHarnessAndCap(t *testing.T) {
	prev := &db.SpawnProfile{
		Harness:          harness.CopilotName,
		Model:            "gpt-5",
		ContextWindowMax: 272_000,
		StartupContext:   "curated guidance",
	}
	// Not observed: traceMemberLaunch got nothing, so every observable field it
	// would have filled is blank. That is an absence, not a finding.
	out, drop := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{}, false)
	require.NotNil(t, out)
	assert.Equal(t, harness.CopilotName, out.Harness,
		"an unobserved member must not have its stored harness pin blanked — that writes an assertion out of an absence")
	assert.Equal(t, int64(272_000), out.ContextWindowMax,
		"with the harness preserved the cap is not foreign, so the TCL-1062 residual no longer arises")
	assert.Equal(t, "gpt-5", out.Model, "the same reasoning covers every observable field, not just the harness")
	assert.Nil(t, drop, "nothing was dropped, so there is nothing to disclose")
}

// The mirror direction, which the root fix does NOT cover and the gate does: the
// member was genuinely observed and is genuinely no longer Copilot. There is no
// absence to preserve here — the operator really did move this agent — so the
// Copilot-only fields go, and the point of TCL-1083 is that the operator is TOLD.
func TestMergeSnapshotInlineProfile_ReTracedHarnessDropsForeignFieldsAndDiscloses(t *testing.T) {
	prev := &db.SpawnProfile{
		Harness:          harness.CopilotName,
		ContextWindowMax: 272_000,
		StartupContext:   "curated guidance",
	}
	// Observed, and this member is Claude now. A blank Harness is how a Claude
	// member is spelled (the default is deliberately not stamped), which is
	// exactly why `observed` has to be passed rather than inferred from the blank.
	out, drop := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Model: "opus"}, true)
	require.NotNil(t, out)
	assert.Zero(t, out.ContextWindowMax,
		"a Copilot cap must not ride onto a member that is now Claude — it would 400 the agent at deploy")
	assert.Equal(t, "curated guidance", out.StartupContext,
		"the gate takes only what the harness cannot accept; harness-agnostic curation still carries")
	require.NotNil(t, drop, "a drop the operator cannot see is the bug this ticket exists to fix")
	assert.Equal(t, []string{"context_window_max"}, drop.Fields)
	assert.Contains(t, drop.Reason, harness.DefaultName,
		"the reason must name the harness the fields were judged against, so the operator can act on it")
	assert.NotContains(t, drop.Reason, "changed",
		"the reason must not assert an event nobody performed — see dropLaunchFieldsForeignToHarness")
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
