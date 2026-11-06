package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The umbrellas are privileged: no structural source confers them, they take
// a group= or resource= scope, and a use limit is refused (one grant covers
// many actions).
func TestResourceUmbrellasAreExplicitOnly(t *testing.T) {
	for _, slug := range []string{PermResourcesRead, PermResourcesWrite} {
		require.True(t, IsKnownPermSlug(slug), slug)
		assert.False(t, IsMemberImpliedSlug(slug), "%s must never be member-implied", slug)
		assert.False(t, IsOwnerImpliedSlug(slug), "%s must never be owner-implied", slug)
		assert.ElementsMatch(t, []ScopeDim{ScopeDimGroup, ScopeDimResource}, permissionScopeDimsForSlug(slug), slug)
		assert.NotEmpty(t, sudoUseLimitRefusal(slug), slug)
		assert.Empty(t, permissionUmbrellaFor(slug), "an umbrella implies no umbrella")
	}
}

// What each umbrella implies, and what it does not: run mutations stay on
// process.runs.manage, locks on groups.resources.lock, and the group slugs
// stay membership's.
func TestResourceUmbrellaImplications(t *testing.T) {
	for slug, want := range map[string]string{
		PermBoardsRead:             PermResourcesRead,
		PermProcessTemplatesRead:   PermResourcesRead,
		PermProcessRunsRead:        PermResourcesRead,
		PermBoardsWrite:            PermResourcesWrite,
		PermProcessTemplatesManage: PermResourcesWrite,
		PermProcessRunsManage:      "",
		PermProcessRunsStart:       "",
		PermGroupsResourcesLock:    "",
		PermGroupsResourcesRead:    PermGroupsAdmin,
	} {
		assert.Equal(t, want, permissionUmbrellaFor(slug), slug)
	}

	src := permSources{
		resolvable: true,
		override:   map[string]overridePermSource{PermResourcesWrite: {Effect: "grant"}, PermResourcesRead: {Effect: "grant"}},
		group:      map[string][]string{},
		sudo:       map[string]sudoPermSource{},
	}
	for _, slug := range []string{PermProcessRunsManage, PermProcessRunsStart, PermGroupsResourcesLock} {
		assert.Equal(t, permUndecided, resolveEffectivePermissionVerdictFrom(src, slug, false, false).Resolution, slug)
	}
	assert.Equal(t, permAllow, resolveEffectivePermissionVerdictFrom(src, PermBoardsWrite, false, false).Resolution)
}

func TestResourceScopeShape(t *testing.T) {
	for _, raw := range []string{
		`{"resource":["board:plan"]}`,
		`{"resource":["process_template:release","board:b_12"]}`,
		`{"group":["crew"],"resource":["board:plan"]}`,
	} {
		_, err := canonicalPermissionScopeForSlug(PermResourcesRead, raw)
		assert.NoError(t, err, raw)
	}
	for _, raw := range []string{
		`{"resource":["plan"]}`,
		`{"resource":["page:sketch"]}`,
		`{"resource":["board:"]}`,
		`{"resource":["board: plan"]}`,
		`{"resource":["process:release"]}`,
	} {
		_, err := canonicalPermissionScopeForSlug(PermResourcesWrite, raw)
		assert.Error(t, err, raw)
	}
	_, err := canonicalPermissionScopeForSlug(PermBoardsRead, `{"group":["crew"]}`)
	assert.Error(t, err, "the per-kind slugs stay unscoped")
}
