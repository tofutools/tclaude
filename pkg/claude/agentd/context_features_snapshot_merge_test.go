package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

func TestTraceMemberLaunchMarksAnObservedEmptyTrimSet(t *testing.T) {
	// The Set bit is what lets the snapshot pass write a non-nil empty map. It is
	// only meaningful alongside a real DB, so this asserts the zero value: an
	// untraceable member leaves it false, which is what preserves prev above.
	var launch templateAgentLaunch
	assert.False(t, launch.ContextFeaturesSet,
		"a launch that was never traced must not claim to have observed a trim set")
	assert.Empty(t, launch.ContextFeatures)
}
