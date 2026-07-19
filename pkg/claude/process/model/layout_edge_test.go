package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const layoutEdgePinnedYAML = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: pinning
name: Pinning
start: build
nodes:
  build:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: build it
    next:
      pass: ship
      fail: stop
  ship:
    type: end
    result: success
  stop:
    type: end
    result: failure
layout:
  nodes:
    build: {x: 10, y: 20}
  edges:
    build:
      pass:
        pinned: true
      fail:
        pinned: false
`

// Pin state is authoring metadata: it must survive a round trip, and it must
// never reach the semantic hash -- otherwise decluttering a label would
// invalidate every pinned template ref for a purely cosmetic edit.
func TestLayoutEdgePinnedRoundTripsOutsideTheSemanticHash(t *testing.T) {
	parsed, err := Parse([]byte(layoutEdgePinnedYAML))
	require.NoError(t, err)
	require.Empty(t, parsed.Diagnostics.Errors(), "unexpected diagnostics: %v", parsed.Diagnostics)
	require.NotNil(t, parsed.Template.Layout)

	pinned := parsed.Template.Layout.Edges["build"]["pass"].Pinned
	require.NotNil(t, pinned)
	assert.True(t, *pinned)

	unpinned := parsed.Template.Layout.Edges["build"]["fail"].Pinned
	require.NotNil(t, unpinned, "an explicit false must survive as false, not collapse to absent")
	assert.False(t, *unpinned)

	// Absent stays absent, which is what leaves the editor's default in charge.
	_, ok := parsed.Template.Layout.Edges["ship"]
	assert.False(t, ok)

	// Flipping a pin must not move the semantic hash.
	flipped := strings.Replace(layoutEdgePinnedYAML, "        pinned: true", "        pinned: false", 1)
	require.NotEqual(t, layoutEdgePinnedYAML, flipped)
	other, err := Parse([]byte(flipped))
	require.NoError(t, err)
	require.Empty(t, other.Diagnostics.Errors())
	assert.Equal(t, parsed.SemanticHash, other.SemanticHash,
		"pin state is cosmetic and must not change the semantic hash")
	assert.NotEqual(t, parsed.SourceHash, other.SourceHash,
		"the source hash should still notice the edit")
}

// The clone must not alias the caller's inner maps.
func TestLayoutEdgeCloneIsDeep(t *testing.T) {
	parsed, err := Parse([]byte(layoutEdgePinnedYAML))
	require.NoError(t, err)
	require.Empty(t, parsed.Diagnostics.Errors())
	clone := cloneTemplate(parsed.Template)
	no := false
	clone.Layout.Edges["build"]["pass"] = LayoutEdge{Pinned: &no}
	assert.True(t, *parsed.Template.Layout.Edges["build"]["pass"].Pinned,
		"mutating the clone must not reach the original")
}

// An unknown key under a layout edge should be reported, not silently kept.
func TestLayoutEdgeRejectsUnknownFields(t *testing.T) {
	bad := strings.Replace(layoutEdgePinnedYAML, "        pinned: true", "        bogus: true", 1)
	parsed, err := Parse([]byte(bad))
	require.NoError(t, err)
	assert.NotEmpty(t, parsed.Diagnostics, "unknown layout edge field should produce a diagnostic")
}

// An orphaned pin is not merely dead weight: the editor reuses outcome names as
// soon as they are free, so a stale entry is an opinion waiting to be inherited.
func TestLayoutEdgeStaleEntriesAreReported(t *testing.T) {
	ghostNode := strings.Replace(layoutEdgePinnedYAML, "    build:\n      pass:", "    ghost:\n      pass:", 1)
	parsed, err := Parse([]byte(ghostNode))
	require.NoError(t, err)
	assert.True(t, hasRule(parsed.Diagnostics, "stale_layout_edge"),
		"a pin on an undeclared node should be reported")

	ghostOutcome := strings.Replace(layoutEdgePinnedYAML, "      pass:\n        pinned: true", "      nowhere:\n        pinned: true", 1)
	parsed, err = Parse([]byte(ghostOutcome))
	require.NoError(t, err)
	assert.True(t, hasRule(parsed.Diagnostics, "stale_layout_edge"),
		"a pin on an outcome the node does not have should be reported")

	// A warning, never an error: a stale pin is cosmetic and must not block a save.
	assert.Empty(t, parsed.Diagnostics.Errors())

	// And a live pin stays quiet.
	parsed, err = Parse([]byte(layoutEdgePinnedYAML))
	require.NoError(t, err)
	assert.False(t, hasRule(parsed.Diagnostics, "stale_layout_edge"))
}

func hasRule(diagnostics Diagnostics, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
