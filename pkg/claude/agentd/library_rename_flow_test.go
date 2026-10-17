package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func libraryRename(t *testing.T, f *testharness.Flow, kind, name, to string) (int, string) {
	t.Helper()
	rec := humanReq(t, f, http.MethodPost, "/v1/library/rename", map[string]any{"kind": kind, "name": name, "to": to})
	return rec.Code, rec.Body.String()
}

func getJSON(t *testing.T, f *testharness.Flow, path string) map[string]any {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	var out map[string]any
	testharness.DecodeJSON(t, rec, &out)
	return out
}

func TestLibraryRename_EveryKindCarriesItsReferences(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("ops")
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/roles", map[string]any{"name": "rn-reviewer", "brief": "Review the change."}},
		{"/v1/saved-prompts", map[string]any{"name": "kickoff", "body": "Start with the tests."}},
		{"/v1/spawn-profiles", map[string]any{"name": "rev", "role_ref": "rn-reviewer", "prompt_refs": []string{"kickoff"}, "model": "opus"}},
		{"/v1/templates", map[string]any{"name": "team", "agents": []map[string]any{{"name": "lead", "role_ref": "rn-reviewer", "spawn_profile": "rev"}}}},
		{"/v1/sandbox-profiles", map[string]any{"name": "base", "filesystem": []map[string]string{{"path": "/opt/rename-example", "access": "read"}}}},
		{"/v1/sandbox-profiles", map[string]any{"name": "child", "includes": []string{"base"}}},
	} {
		rec := humanReq(t, f, http.MethodPost, c.path, c.body)
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, "%s: %s", c.path, rec.Body.String())
	}
	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/ops", map[string]any{"default_profile": "rev"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, c := range [][3]string{
		{"roles", "rn-reviewer", "rn-critic"},
		{"saved_prompts", "kickoff", "opening"},
		{"profiles", "rev", "reviewer-profile"},
		{"sandbox_profiles", "base", "base-ro"},
		{"templates", "team", "review-team"},
	} {
		code, body := libraryRename(t, f, c[0], c[1], c[2])
		require.Equal(t, http.StatusOK, code, "%v: %s", c, body)
	}

	profile := getJSON(t, f, "/v1/spawn-profiles/reviewer-profile")
	require.Equal(t, "rn-critic", profile["role_ref"], "a role rename follows into the profile")
	require.Equal(t, []any{"opening"}, profile["prompt_refs"], "a prompt rename follows into the profile")
	require.Equal(t, "opus", profile["model"], "nothing but the name changes")
	team := getJSON(t, f, "/v1/templates/review-team")
	lead := team["agents"].([]any)[0].(map[string]any)
	require.Equal(t, "rn-critic", lead["role_ref"])
	require.Equal(t, "reviewer-profile", lead["spawn_profile"], "a profile rename follows into the template")
	child := getJSON(t, f, "/v1/sandbox-profiles/child")
	require.Equal(t, []any{"base-ro"}, child["includes"], "a sandbox rename follows into its includers")
	group, err := db.GetAgentGroupByName("ops")
	require.NoError(t, err)
	require.Equal(t, "reviewer-profile", group.DefaultProfile, "and into a group's default profile")
	for _, gone := range []string{"/v1/roles/rn-reviewer", "/v1/saved-prompts/kickoff", "/v1/templates/team", "/v1/sandbox-profiles/base"} {
		require.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodGet, gone, nil).Code, gone)
	}
}

func TestLibraryRename_RefusesMissingTakenAndUnknown(t *testing.T) {
	f := newFlow(t)
	for _, name := range []string{"one", "two"} {
		rec := humanReq(t, f, http.MethodPost, "/v1/roles", map[string]any{"name": name})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	code, body := libraryRename(t, f, "roles", "one", "two")
	require.Equal(t, http.StatusConflict, code, body)
	code, body = libraryRename(t, f, "roles", "missing", "three")
	require.Equal(t, http.StatusNotFound, code, body)
	code, body = libraryRename(t, f, "processes", "one", "three")
	require.Equal(t, http.StatusBadRequest, code, body)
	code, body = libraryRename(t, f, "roles", "one", "")
	require.Equal(t, http.StatusBadRequest, code, body)
	code, body = libraryRename(t, f, "roles", "one", "one")
	require.Equal(t, http.StatusOK, code, body, "renaming to the same name is a no-op")

	const agent = "019fe740-43a4-7023-b8ae-1ee64459f4b1"
	f.HaveAliveSession(agent, "renamer", "renamer-pane", f.TestCwd("renamer"))
	rec := agentReq(t, f, agent, http.MethodPost, "/v1/library/rename", map[string]any{"kind": "roles", "name": "one", "to": "three"})
	require.Equal(t, http.StatusForbidden, rec.Code, "renaming needs the kind's manage permission")
}
