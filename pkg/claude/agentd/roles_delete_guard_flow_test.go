package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-351: roles resolve at DEPLOY time, so deleting a role a template still
// references would silently change that template's next deploy. The DELETE
// endpoint refuses while any template references the role, naming the
// referencing templates so the human can go repoint them. These flow tests
// exercise the guard at the /v1/roles/{name} surface.

// deleteRole DELETEs a role as the human and returns the recorder.
func deleteRole(t *testing.T, f *testharness.Flow, name string) *httptest.ResponseRecorder {
	t.Helper()
	return humanReq(t, f, http.MethodDelete, "/v1/roles/"+name, nil)
}

// Scenario: a role is referenced by a template. Deleting it is refused (409),
// and the error names the referencing template. After the reference is removed
// (the template is edited to drop role_ref), the delete succeeds (204).
func TestRoleDelete_RefusedWhileReferenced(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name":  "guarded-role",
		"descr": "referenced by a template",
	}).Code, "create role")

	// A template whose sole agent references the role.
	createBody := map[string]any{
		"name": "guarded-team",
		"agents": []map[string]any{
			{"name": "rev", "role_ref": "guarded-role"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	// Delete is refused with 409, and the body names the referencing template.
	rec := deleteRole(t, f, "guarded-role")
	require.Equalf(t, http.StatusConflict, rec.Code, "delete refused while referenced: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "guarded-team", "the error names the referencing template")
	assert.Contains(t, rec.Body.String(), "role_in_use", "the error carries the role_in_use code")

	// The role still exists (the refusal didn't delete it).
	require.Equalf(t, http.StatusOK,
		humanReq(t, f, http.MethodGet, "/v1/roles/guarded-role", nil).Code, "role survives the refused delete")

	// Drop the reference by editing the template's agent to carry no role_ref,
	// then the delete succeeds.
	editBody := map[string]any{
		"name": "guarded-team",
		"agents": []map[string]any{
			{"name": "rev", "role_ref": ""},
		},
	}
	require.Equalf(t, http.StatusOK,
		humanReq(t, f, http.MethodPatch, "/v1/templates/guarded-team", editBody).Code, "edit template to drop role_ref")

	rec = deleteRole(t, f, "guarded-role")
	require.Equalf(t, http.StatusNoContent, rec.Code, "delete succeeds once unreferenced: %s", rec.Body.String())

	// And it's gone.
	require.Equalf(t, http.StatusNotFound,
		humanReq(t, f, http.MethodGet, "/v1/roles/guarded-role", nil).Code, "role is deleted")
}

// Scenario: deleting the referencing template also frees the role — the guard
// keys on live references only, so no template means no block.
func TestRoleDelete_FreedWhenReferencingTemplateDeleted(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "freed-role",
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "freed-team",
		"agents": []map[string]any{{"name": "a", "role_ref": "freed-role"}},
	}).Code, "create template")

	// Blocked while the template lives.
	require.Equalf(t, http.StatusConflict, deleteRole(t, f, "freed-role").Code, "blocked while referenced")

	// Delete the template, then the role deletes cleanly.
	require.Equalf(t, http.StatusNoContent,
		humanReq(t, f, http.MethodDelete, "/v1/templates/freed-team", nil).Code, "delete template")
	require.Equalf(t, http.StatusNoContent, deleteRole(t, f, "freed-role").Code, "role freed once template gone")
}

// Scenario: a template carries a DANGLING role_ref (its role was removed
// out-of-band, e.g. legacy data). Deleting that now-missing role name answers
// 404 "no such role" — the existence check runs before the reference guard, so
// the operator isn't handed a misleading "still referenced" 409 for a role that
// doesn't exist.
func TestRoleDelete_DanglingRefStillAnswers404(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "ghost-role",
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "haunted-team",
		"agents": []map[string]any{{"name": "a", "role_ref": "ghost-role"}},
	}).Code, "create template referencing the role")

	// Remove the role out-of-band via the DB primitive (which bypasses the wire
	// guard), leaving haunted-team with a dangling role_ref.
	n, err := db.DeleteRole("ghost-role")
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "role removed out-of-band")

	// Deleting the now-missing role via the API answers 404, not a 409 that would
	// wrongly claim it's still referenced.
	rec := deleteRole(t, f, "ghost-role")
	require.Equalf(t, http.StatusNotFound, rec.Code, "missing role answers 404: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "role_in_use", "no misleading in-use error for a missing role")
}

// Scenario: an unreferenced role deletes without a fuss — the guard only fires
// on a live reference.
func TestRoleDelete_UnreferencedSucceeds(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "lonely-role",
	}).Code, "create role")
	rec := deleteRole(t, f, "lonely-role")
	require.Equalf(t, http.StatusNoContent, rec.Code, "unreferenced role deletes: %s", rec.Body.String())
	assert.False(t, strings.Contains(rec.Body.String(), "role_in_use"), "no in-use error for an unreferenced role")
}
