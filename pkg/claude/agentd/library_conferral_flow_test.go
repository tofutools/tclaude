package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Since default profiles reach every spawn path, unattended workers included,
// a profile or role write that adds birth-time access, and setting the global
// default profile, take the same conferral authority as setting a group's
// default (tcl-wxxsvh): permissions.grant plus attenuation for grants, and
// groups.owners.manage over every group for is_owner. An edit that adds no
// access needs none, and the operator always may.

const librarian = "libr-1111-2222-3333-4444"

// libraryAgent is an agent holding the library slugs, and nothing that would
// let it confer access.
func libraryAgent(t *testing.T) *testharness.Flow {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", librarian)
	for _, slug := range []string{agentd.PermProfilesManage, agentd.PermRolesManage} {
		require.NoError(t, db.GrantAgentPermission(librarian, slug, "test"))
	}
	return f
}

// mayConfer gives the librarian the authority to confer reviewerSlug.
func mayConfer(t *testing.T) {
	t.Helper()
	require.NoError(t, db.GrantAgentPermission(librarian, agentd.PermPermissionsGrant, "test"))
}

func profileBody(name string, extra map[string]any) map[string]any {
	body := map[string]any{"name": name, "harness": "claude"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

var (
	grantsReviewer = map[string]any{"permission_overrides": map[string]string{reviewerSlug: "grant"}}
	deniesReviewer = map[string]any{"permission_overrides": map[string]string{reviewerSlug: "deny"}}
	makesOwners    = map[string]any{"is_owner": true}
)

func TestLibraryConferral_ProfileCreate(t *testing.T) {
	f := libraryAgent(t)
	create := func(name string, extra map[string]any) int {
		return agentReq(t, f, librarian, http.MethodPost, "/v1/spawn-profiles", profileBody(name, extra)).Code
	}
	assert.Equal(t, http.StatusForbidden, create("granting", grantsReviewer), "a grant without permissions.grant")
	assert.Equal(t, http.StatusForbidden, create("owning", makesOwners), "is_owner without groups.owners.manage")
	assert.Equal(t, http.StatusCreated, create("denying", deniesReviewer), "a deny only narrows")
	assert.Equal(t, http.StatusCreated, create("plain", map[string]any{"descr": "no access"}), "a profile conferring nothing")

	mayConfer(t)
	assert.Equal(t, http.StatusCreated, create("granting", grantsReviewer), "with the authority to confer it")
	require.NoError(t, db.GrantAgentPermissionWithScope(librarian, agentd.PermGroupsOwnersManage, `{"group":["alpha"]}`, "test"))
	assert.Equal(t, http.StatusForbidden, create("owning", makesOwners),
		"a library profile can reach any group, so a grant scoped to one does not cover it")
	require.NoError(t, db.GrantAgentPermission(librarian, agentd.PermGroupsOwnersManage, "test"))
	assert.Equal(t, http.StatusCreated, create("owning", makesOwners), "an unscoped groups.owners.manage covers it")

	assert.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		profileBody("operator-owning", map[string]any{"is_owner": true, "permission_overrides": map[string]string{reviewerSlug: "grant"}})).Code,
		"the operator always may")
}

func TestLibraryConferral_ProfileUpdate(t *testing.T) {
	f := libraryAgent(t)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
		map[string]any{"name": "conferral-reviewer", "permissions": []string{reviewerSlug}}).Code)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		profileBody("worker", grantsReviewer)).Code)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		profileBody("bare", nil)).Code)
	patch := func(name string, body map[string]any) int {
		return agentReq(t, f, librarian, http.MethodPatch, "/v1/spawn-profiles/"+name, body).Code
	}
	namesRole := map[string]any{"role_refs": []string{"conferral-reviewer"}}

	// The librarian holds no permissions.grant, so it cannot confer anything,
	// yet an edit that keeps the profile's access as it is needs no authority.
	assert.Equal(t, http.StatusOK, patch("worker", profileBody("worker", map[string]any{
		"descr": "reworded", "model": "opus", "permission_overrides": map[string]string{reviewerSlug: "grant"}})),
		"an edit that adds no access")
	assert.Equal(t, http.StatusForbidden, patch("bare", profileBody("bare", grantsReviewer)), "adding a grant")
	assert.Equal(t, http.StatusForbidden, patch("bare", profileBody("bare", namesRole)), "naming a role that grants")
	assert.Equal(t, http.StatusForbidden, patch("bare", profileBody("bare", makesOwners)), "adding is_owner")
	assert.Equal(t, http.StatusOK, patch("bare", profileBody("bare", deniesReviewer)), "adding a deny")

	mayConfer(t)
	assert.Equal(t, http.StatusOK, patch("bare", profileBody("bare", namesRole)), "with the authority to confer it")
	assert.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/bare",
		profileBody("bare", makesOwners)).Code, "the operator always may")
	got, err := db.ResolveSpawnProfile("bare")
	require.NoError(t, err)
	require.NotNil(t, got.IsOwner)
	assert.True(t, *got.IsOwner)
}

func TestLibraryConferral_Role(t *testing.T) {
	f := libraryAgent(t)
	create := func(body map[string]any) int {
		return agentReq(t, f, librarian, http.MethodPost, "/v1/roles", body).Code
	}
	patch := func(name string, body map[string]any) int {
		return agentReq(t, f, librarian, http.MethodPatch, "/v1/roles/"+name, body).Code
	}
	grants := func(name string) map[string]any {
		return map[string]any{"name": name, "permissions": []string{reviewerSlug}}
	}
	assert.Equal(t, http.StatusForbidden, create(grants("conferral-granting")), "a grant without permissions.grant")
	assert.Equal(t, http.StatusCreated, create(map[string]any{"name": "conferral-plain", "brief": "Review."}),
		"a role granting nothing")

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
		map[string]any{"name": "conferral-reviewer", "brief": "Review.", "permissions": []string{reviewerSlug}}).Code)
	assert.Equal(t, http.StatusOK, patch("conferral-reviewer", map[string]any{
		"name": "conferral-reviewer", "brief": "Review closely.", "permissions": []string{reviewerSlug}}),
		"an edit that adds no grant")
	assert.Equal(t, http.StatusForbidden, patch("conferral-plain", grants("conferral-plain")), "adding a grant")

	// Attenuation: holding the slug only within alpha, the librarian cannot
	// put it on a role unscoped.
	mayConfer(t)
	require.NoError(t, db.GrantAgentPermissionWithScope(librarian, reviewerSlug, `{"group":["alpha"]}`, "test"))
	assert.Equal(t, http.StatusForbidden, patch("conferral-plain", grants("conferral-plain")),
		"a grant wider than the librarian's own")
	_, err := db.RevokeAgentPermission(librarian, reviewerSlug)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, patch("conferral-plain", grants("conferral-plain")), "with the authority to confer it")
	assert.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
		map[string]any{"name": "conferral-operator", "permissions": []string{agentd.PermGroupsAdmin}}).Code,
		"the operator always may")
}

func TestLibraryConferral_GlobalDefault(t *testing.T) {
	f := libraryAgent(t)
	isOwner := true
	for _, p := range []*db.SpawnProfile{
		{Name: "granting", Harness: "claude", PermissionOverrides: map[string]db.PermissionOverride{
			reviewerSlug: {Effect: db.PermEffectGrant}}},
		{Name: "owning", Harness: "claude", IsOwner: &isOwner},
		{Name: "denying", Harness: "claude", PermissionOverrides: map[string]db.PermissionOverride{
			reviewerSlug: {Effect: db.PermEffectDeny}}},
	} {
		_, err := db.CreateSpawnProfile(p)
		require.NoError(t, err)
	}
	set := func(name string) int {
		return agentReq(t, f, librarian, http.MethodPut, "/v1/spawn-profile-default", map[string]any{"name": name}).Code
	}
	assert.Equal(t, http.StatusForbidden, set("granting"), "a grant without permissions.grant")
	assert.Equal(t, http.StatusForbidden, set("owning"), "is_owner without groups.owners.manage")
	assert.Equal(t, http.StatusOK, set("denying"), "a deny only narrows")

	mayConfer(t)
	assert.Equal(t, http.StatusOK, set("granting"), "with the authority to confer it")
	require.NoError(t, db.GrantAgentPermissionWithScope(librarian, agentd.PermGroupsOwnersManage, `{"group":["alpha"]}`, "test"))
	assert.Equal(t, http.StatusForbidden, set("owning"), "the global default reaches every group, not just alpha")
	require.NoError(t, db.GrantAgentPermission(librarian, agentd.PermGroupsOwnersManage, "test"))
	assert.Equal(t, http.StatusOK, set("owning"), "an unscoped groups.owners.manage covers it")

	_, err := db.RevokeAgentPermission(librarian, agentd.PermGroupsOwnersManage)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPut, "/v1/spawn-profile-default",
		map[string]any{"name": "owning"}).Code, "the operator always may")
}

// Importing a profile is a profile write too, judged before anything lands.
func TestLibraryConferral_ProfileImport(t *testing.T) {
	f := libraryAgent(t)
	body := map[string]any{
		"format": "tclaude-spawn-profiles", "format_version": 2,
		"profiles": []map[string]any{profileBody("imported", grantsReviewer), profileBody("imported-plain", nil)},
	}
	assert.Equal(t, http.StatusForbidden, agentReq(t, f, librarian, http.MethodPost, "/v1/spawn-profiles/import", body).Code)
	for _, name := range []string{"imported", "imported-plain"} {
		got, err := db.ResolveSpawnProfile(name)
		require.NoError(t, err)
		assert.Nilf(t, got, "a refused import writes nothing, %s included", name)
	}
	assert.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", body).Code,
		"the operator always may")
}

// Renaming a role onto a name a profile already references (one that dangled,
// say from older data) hands that profile the role's grants, so the rename is
// judged as conferring them all, through either rename route.
func TestLibraryConferral_RoleRenameOntoAReferencedName(t *testing.T) {
	f := libraryAgent(t)
	_, err := db.CreateRole(&db.Role{Name: "conferral-admin",
		Permissions: []db.PermissionGrant{{Slug: reviewerSlug}}})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "dangling", Harness: "claude",
		RoleRefs: []string{"conferral-target"}})
	require.NoError(t, err)
	patchRename := func(to string) int {
		return agentReq(t, f, librarian, http.MethodPatch, "/v1/roles/conferral-admin",
			map[string]any{"name": to, "permissions": []string{reviewerSlug}}).Code
	}
	libraryRename := func(from, to string) int {
		return agentReq(t, f, librarian, http.MethodPost, "/v1/library/rename",
			map[string]any{"kind": "roles", "name": from, "to": to}).Code
	}
	assert.Equal(t, http.StatusForbidden, patchRename("conferral-target"), "PATCH onto a referenced name")
	assert.Equal(t, http.StatusForbidden, libraryRename("conferral-admin", "conferral-target"), "library rename onto a referenced name")
	assert.Equal(t, http.StatusOK, libraryRename("conferral-admin", "conferral-elsewhere"),
		"a rename no profile is waiting for adds nothing")

	mayConfer(t)
	assert.Equal(t, http.StatusOK, libraryRename("conferral-elsewhere", "conferral-target"), "with the authority to confer it")
	got, err := db.GetRole("conferral-target")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// A config bundle is judged while it is planned, so a refused profile leaves
// the bundle's other items unwritten too.
func TestLibraryConferral_ConfigBundleRefusesBeforeApplying(t *testing.T) {
	f := libraryAgent(t)
	require.NoError(t, db.GrantAgentPermission(librarian, agentd.PermConfigImport, "test"))
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
		map[string]any{"name": "conferral-bundled", "brief": "Review."}).Code)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/spawn-profiles",
		profileBody("bundled", grantsReviewer)).Code)
	rec := humanReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/conferral-bundled&only=profiles/bundled", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	_, err := db.DeleteSpawnProfile("bundled")
	require.NoError(t, err)
	_, err = db.DeleteRole("conferral-bundled")
	require.NoError(t, err)

	rec = agentReq(t, f, librarian, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermPermissionsGrant)
	role, err := db.GetRole("conferral-bundled")
	require.NoError(t, err)
	assert.Nil(t, role, "nothing in the bundle applies")

	rec = humanReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
