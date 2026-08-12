package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The offered scope is a projection of the gate's ActionContext onto the
// dimensions the slug DECLARES — never wider, and never a dimension the slug
// does not admit. groups.members.spawn declares group + spawn_profile, so a context
// that names only the group must yield only the group: a scope naming an
// undescribed dimension could not be satisfied, and one naming nothing at all
// would be a blanket grant wearing the narrow button's label.
func TestApprovalScopeForSlug_ProjectsOnlyDescribedDeclaredDims(t *testing.T) {
	scopeJSON, display := approvalScopeForSlug(PermGroupsMembersSpawn, ActionContext{Group: "alpha"})
	assert.JSONEq(t, `{"group":["alpha"]}`, scopeJSON)
	assert.Equal(t, "group=alpha", display)

	both, _ := approvalScopeForSlug(PermGroupsMembersSpawn, ActionContext{Group: "alpha", SpawnProfile: "p1"})
	assert.JSONEq(t, `{"group":["alpha"],"spawn_profile":["p1"]}`, both)

	// A dimension the slug does not declare is dropped, not smuggled in: the
	// scope writer would reject it, and offering nothing is the right answer.
	undeclared, _ := approvalScopeForSlug(PermGroupsMembersSpawn, ActionContext{ProcessTemplate: "tmpl"})
	assert.Empty(t, undeclared, "a context the slug cannot be scoped by offers no scoped button")
}

// A slug with no dimensions, and a dimensioned slug at a gate site that
// described nothing, both offer nothing — which is what leaves the blanket
// button as the only choice.
func TestApprovalScopeForSlug_EmptyWhenNothingToNarrowTo(t *testing.T) {
	noDims, display := approvalScopeForSlug(PermHumanClipboard, ActionContext{Group: "alpha"})
	assert.Empty(t, noDims)
	assert.Empty(t, display)

	noContext, _ := approvalScopeForSlug(PermGroupsMembersSpawn, ActionContext{})
	assert.Empty(t, noContext)

	unknownSlug, _ := approvalScopeForSlug("no.such.slug", ActionContext{Group: "alpha"})
	assert.Empty(t, unknownSlug)
}

func TestMergeApprovalScope(t *testing.T) {
	merged := func(existing, added string) (string, bool) {
		return mergeApprovalScope(PermGroupsMembersSpawn, existing, added)
	}

	// One dimension widening is a true union, and is what makes a second
	// narrow approval additive rather than a replacement.
	scope, ok := merged(`{"group":["alpha"]}`, `{"group":["beta"]}`)
	assert.True(t, ok)
	assert.JSONEq(t, `{"group":["alpha","beta"]}`, scope)

	// Re-approving the same context changes nothing.
	scope, ok = merged(`{"group":["alpha"]}`, `{"group":["alpha"]}`)
	assert.True(t, ok)
	assert.JSONEq(t, `{"group":["alpha"]}`, scope)

	// Widening one dimension of a two-dimension scope is still a union: the
	// other dimension is identical on both sides, so no unapproved
	// combination can appear.
	scope, ok = merged(`{"group":["alpha"],"spawn_profile":["p1"]}`, `{"group":["beta"],"spawn_profile":["p1"]}`)
	assert.True(t, ok)
	assert.JSONEq(t, `{"group":["alpha","beta"],"spawn_profile":["p1"]}`, scope)

	// Two dimensions differing at once is the CROSS PRODUCT, not the union:
	// merging would admit {beta, p1} and {alpha, p2}, which nobody approved.
	_, ok = merged(`{"group":["alpha"],"spawn_profile":["p1"]}`, `{"group":["beta"],"spawn_profile":["p2"]}`)
	assert.False(t, ok, "a two-dimension divergence must not be folded into one scope")

	// Different dimension SETS cannot be folded either: adding a dimension
	// constrains one that was unconstrained, revoking part of the stored
	// grant; dropping one widens it.
	_, ok = merged(`{"group":["alpha"]}`, `{"group":["alpha"],"spawn_profile":["p1"]}`)
	assert.False(t, ok)
	_, ok = merged(`{"spawn_profile":["p1"]}`, `{"group":["alpha"]}`)
	assert.False(t, ok)

	// An unreadable or absent stored scope is never treated as unscoped: the
	// new narrow scope stands alone rather than the merge silently widening.
	scope, ok = merged("not json", `{"group":["beta"]}`)
	assert.True(t, ok)
	assert.JSONEq(t, `{"group":["beta"]}`, scope)
	scope, ok = merged("", `{"group":["beta"]}`)
	assert.True(t, ok)
	assert.JSONEq(t, `{"group":["beta"]}`, scope)
}

// isScopedAutoGrantableSlug must never admit a slug the blanket button
// refuses: the scoped persist is a narrowing of that button, not a second
// route around its allowlist.
func TestScopedAutoGrantableNeverExceedsAutoGrantable(t *testing.T) {
	for _, p := range permissionRegistry {
		if isScopedAutoGrantableSlug(p.Slug) {
			assert.True(t, IsAutoGrantableSlug(p.Slug),
				"%s is scope-auto-grantable but not auto-grantable", p.Slug)
		}
	}
	assert.False(t, isScopedAutoGrantableSlug(PermAgentDelete))
	assert.False(t, isScopedAutoGrantableSlug("no.such.slug"))
}
