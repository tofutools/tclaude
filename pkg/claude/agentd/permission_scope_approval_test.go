package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The offered scope is a projection of the gate's ActionContext onto the
// dimensions the slug DECLARES — never wider, and never a dimension the slug
// does not admit. groups.spawn declares group + spawn_profile, so a context
// that names only the group must yield only the group: a scope naming an
// undescribed dimension could not be satisfied, and one naming nothing at all
// would be a blanket grant wearing the narrow button's label.
func TestApprovalScopeForSlug_ProjectsOnlyDescribedDeclaredDims(t *testing.T) {
	scopeJSON, display := approvalScopeForSlug(PermGroupsSpawn, ActionContext{Group: "alpha"})
	assert.JSONEq(t, `{"group":["alpha"]}`, scopeJSON)
	assert.Equal(t, "group=alpha", display)

	both, _ := approvalScopeForSlug(PermGroupsSpawn, ActionContext{Group: "alpha", SpawnProfile: "p1"})
	assert.JSONEq(t, `{"group":["alpha"],"spawn_profile":["p1"]}`, both)

	// A dimension the slug does not declare is dropped, not smuggled in: the
	// scope writer would reject it, and offering nothing is the right answer.
	undeclared, _ := approvalScopeForSlug(PermGroupsSpawn, ActionContext{ProcessTemplate: "tmpl"})
	assert.Empty(t, undeclared, "a context the slug cannot be scoped by offers no scoped button")
}

// A slug with no dimensions, and a dimensioned slug at a gate site that
// described nothing, both offer nothing — which is what leaves the blanket
// button as the only choice.
func TestApprovalScopeForSlug_EmptyWhenNothingToNarrowTo(t *testing.T) {
	noDims, display := approvalScopeForSlug(PermHumanClipboard, ActionContext{Group: "alpha"})
	assert.Empty(t, noDims)
	assert.Empty(t, display)

	noContext, _ := approvalScopeForSlug(PermGroupsSpawn, ActionContext{})
	assert.Empty(t, noContext)

	unknownSlug, _ := approvalScopeForSlug("no.such.slug", ActionContext{Group: "alpha"})
	assert.Empty(t, unknownSlug)
}

func TestMergeApprovalScope(t *testing.T) {
	// The union is what makes a second narrow approval additive.
	assert.JSONEq(t, `{"group":["alpha","beta"]}`,
		mergeApprovalScope(PermGroupsSpawn, `{"group":["alpha"]}`, `{"group":["beta"]}`))
	// Re-approving the same context changes nothing.
	assert.JSONEq(t, `{"group":["alpha"]}`,
		mergeApprovalScope(PermGroupsSpawn, `{"group":["alpha"]}`, `{"group":["alpha"]}`))
	// A dimension present in only one side stays a constraint. Dropping it
	// would widen the grant to every value of that dimension.
	assert.JSONEq(t, `{"group":["alpha"],"spawn_profile":["p1"]}`,
		mergeApprovalScope(PermGroupsSpawn, `{"spawn_profile":["p1"]}`, `{"group":["alpha"]}`))
	// An unreadable or absent stored scope is never treated as unscoped: the
	// new narrow scope stands alone rather than the merge silently widening.
	assert.JSONEq(t, `{"group":["beta"]}`,
		mergeApprovalScope(PermGroupsSpawn, "not json", `{"group":["beta"]}`))
	assert.JSONEq(t, `{"group":["beta"]}`,
		mergeApprovalScope(PermGroupsSpawn, "", `{"group":["beta"]}`))
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
