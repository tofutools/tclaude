package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func mustScope(t *testing.T, raw string) PermissionScope {
	t.Helper()
	scope, err := permissionScopeForEval(raw)
	require.NoError(t, err, "test fixture scope must parse: %s", raw)
	return scope
}

// The cover matrix. Read "granter covers conferred" as "a granter holding
// `granter` may hand out `conferred`" — every false row is an escalation the
// minting surfaces must refuse.
func TestPermissionScopeCovers(t *testing.T) {
	cases := []struct {
		name      string
		granter   string
		conferred string
		want      bool
	}{
		// The unscoped granter — every grant that exists on a pre-scope
		// deployment. Delegation must behave exactly as it did before.
		{"unscoped granter confers unscoped", ``, ``, true},
		{"unscoped granter confers scoped", ``, `{"group":["a"]}`, true},
		{"unscoped granter confers a selector", ``, `{"target_agent":["@descendants"]}`, true},

		// The case this whole mechanism exists for.
		{"scoped granter cannot confer unscoped", `{"group":["a"]}`, ``, false},

		{"equal scope", `{"group":["a"]}`, `{"group":["a"]}`, true},
		{"subset of a value list", `{"group":["a","b"]}`, `{"group":["a"]}`, true},
		{"same value list, reordered", `{"group":["a","b"]}`, `{"group":["b","a"]}`, true},
		{"superset of a value list", `{"group":["a"]}`, `{"group":["a","b"]}`, false},
		{"disjoint value", `{"group":["a"]}`, `{"group":["b"]}`, false},

		// Dimensions AND, so ADDING one only narrows; DROPPING one widens,
		// because the dropped dimension becomes unconstrained.
		{"conferred adds a dimension", `{"group":["a"]}`, `{"group":["a"],"spawn_profile":["p1"]}`, true},
		{"conferred drops a dimension",
			`{"group":["a"],"spawn_profile":["p1"]}`, `{"group":["a"]}`, false},
		{"both dimensions narrowed",
			`{"group":["a","b"],"spawn_profile":["p1","p2"]}`,
			`{"group":["a"],"spawn_profile":["p1"]}`, true},
		{"one dimension narrowed, the other widened",
			`{"group":["a","b"],"spawn_profile":["p1"]}`,
			`{"group":["a"],"spawn_profile":["p1","p2"]}`, false},

		// Selectors compare structurally in BOTH directions: neither "is this
		// concrete agent a descendant" nor "does this value list cover the
		// descendant set" is decidable without the Phase 5 lineage walk, and
		// an unproven answer may never be assumed.
		{"selector covers the same selector",
			`{"target_agent":["@descendants"]}`, `{"target_agent":["@descendants"]}`, true},
		{"selector does not cover a concrete value",
			`{"target_agent":["@descendants"]}`, `{"target_agent":["agt_1"]}`, false},
		{"value list does not cover a selector",
			`{"target_agent":["agt_1"]}`, `{"target_agent":["@descendants"]}`, false},
		{"one selector does not cover another",
			`{"target_agent":["@descendants"]}`, `{"target_agent":["@self-spawned"]}`, false},
		{"selector union covers one of its members",
			`{"target_agent":["@descendants","@self-spawned"]}`,
			`{"target_agent":["@self-spawned"]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permissionScopeCovers(mustScope(t, tc.granter), mustScope(t, tc.conferred))
			assert.Equal(t, tc.want, got)
		})
	}
}

// A human granter (convID "") is unconstrained, and so is every slug the
// granter holds unscoped or does not hold at all — the check engages only on
// the SHAPE of a scoped hold.
func TestCheckGrantAttenuation_NoGranterScopeIsANoOp(t *testing.T) {
	assert.NoError(t, checkGrantAttenuation("", []conferredGrant{
		{Slug: PermGroupsSpawn, Scope: ""},
	}), "the human operator confers freely")
	assert.NoError(t, checkGrantAttenuation("some-conv", nil),
		"a request conferring nothing is never refused")
}

// Denies confer no authority, so they never need cover — only grants do.
func TestConferredGrantsFromOverridesSkipsDenies(t *testing.T) {
	grants := conferredGrantsFromOverrides(map[string]db.PermissionOverride{
		PermGroupsSpawn:      {Effect: db.PermEffectGrant, Scope: `{"group":["a"]}`},
		PermPermissionsGrant: db.Deny(),
		PermGroupsOwn:        db.Grant(),
	})
	require.Len(t, grants, 2)
	// Sorted by slug, so a refusal message is stable across runs.
	assert.Equal(t, PermGroupsOwn, grants[0].Slug)
	assert.Equal(t, "", grants[0].Scope)
	assert.Equal(t, PermGroupsSpawn, grants[1].Slug)
	assert.Equal(t, `{"group":["a"]}`, grants[1].Scope)
}

// The union arm of a birth-time override: an unscoped override still writes
// the bare effect string every pre-scope row holds, and both arms decode.
func TestPermissionOverrideJSONUnion(t *testing.T) {
	unscoped, err := json.Marshal(map[string]db.PermissionOverride{"a": db.Grant()})
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"grant"}`, string(unscoped),
		"an unscoped override must serialize exactly as it did before scopes existed")

	scoped, err := json.Marshal(map[string]db.PermissionOverride{
		"a": {Effect: db.PermEffectGrant, Scope: `{"group":["x"]}`},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":{"effect":"grant","scope":{"group":["x"]}}}`, string(scoped))

	var back map[string]db.PermissionOverride
	require.NoError(t, json.Unmarshal([]byte(`{"legacy":"deny","new":{"effect":"grant","scope":{"group":["x"]}}}`), &back))
	assert.Equal(t, db.Deny(), back["legacy"], "a legacy bare effect decodes as unscoped")
	assert.Equal(t, `{"group":["x"]}`, back["new"].Scope)
}

// The popup's "Always allow for this agent" button persists an UNSCOPED
// grant, and the popup is precisely what fires when a SCOPED grant failed to
// cover an action — so an auto-grantable slug that also declared scope
// dimensions would let one click replace an operator's narrowing with the
// widest possible grant.
//
// persistAlwaysAllowGrant refuses that case at runtime, but refusing means the
// human's "always" silently does not stick. This guard makes the drift a build
// failure instead: whoever adds ScopeDims to an auto-grantable slug (or marks a
// scopeable slug auto-grantable) has to decide what "always" means for a scoped
// action first.
func TestPermissionRegistry_AutoGrantableSlugsAreNotScopeable(t *testing.T) {
	for _, slug := range AutoGrantableSlugs() {
		dims := permissionScopeDimsForSlug(slug)
		assert.Emptyf(t, dims,
			"auto-grantable slug %q declares scope dimensions %v: decide what the popup's "+
				"\"always allow\" should persist for a scoped action before allowing this", slug, dims)
	}
}
