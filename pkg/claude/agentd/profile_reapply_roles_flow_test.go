package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-fg2kqk: a re-apply keeps a role's identity in step with its grants. The
// role label (the agent's role in its spawn group) and the role briefs in its
// startup context follow the roles the profile names now.

func memberRole(t *testing.T, group, convID string) string {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	for _, m := range members {
		if m.ConvID == convID {
			return m.Role
		}
	}
	t.Fatalf("conv %s is not a member of %s", convID, group)
	return ""
}

func profileContextOf(t *testing.T, agentID string) string {
	t.Helper()
	snap, err := db.GetAgentStartupSnapshot(agentID)
	require.NoError(t, err)
	require.NotNil(t, snap)
	return snap.ProfileContext
}

func roleFixture(t *testing.T) *testharness.Flow {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("alpha")
	for name, brief := range map[string]string{"notifier": "Tell the human.", "copier": "Copy things."} {
		slug := agentd.PermHumanNotify
		if name == "copier" {
			slug = agentd.PermHumanClipboard
		}
		require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
			map[string]any{"name": name, "brief": brief, "permissions": []string{slug}}).Code)
	}
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"role_refs": []string{"notifier"}, "startup_context": "Be brief.",
	})).Code)
	return f
}

func TestProfileReapplyRoles_LabelAndBriefFollowTheRoles(t *testing.T) {
	f := roleFixture(t)
	a := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	renamed := spawnStopped(t, f, "alpha", map[string]any{"name": "renamed", "profile": "worker"})
	explicit := spawnStopped(t, f, "alpha", map[string]any{"name": "explicit", "profile": "worker", "role_refs": []string{"notifier"}})
	require.Equal(t, "notifier", memberRole(t, "alpha", a.ConvID))
	require.Equal(t, "Be brief.\n\n## Role\n\nTell the human.", profileContextOf(t, a.AgentID))

	// The operator gave one agent its own role label since.
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	own := "lead"
	_, err = db.UpdateAgentGroupMember(g.ID, renamed.ConvID, &own, nil)
	require.NoError(t, err)

	patchProfile(t, f, "worker", map[string]any{"role_refs": []string{"copier"}, "startup_context": "Be brief."})
	for _, s := range []testharness.SpawnResp{a, renamed, explicit} {
		code, _, raw := postReapply(t, f, "worker", s.AgentID)
		require.Equal(t, http.StatusOK, code, raw)
	}

	assert.Equal(t, "copier", memberRole(t, "alpha", a.ConvID))
	assert.Equal(t, "Be brief.\n\n## Role\n\nCopy things.", profileContextOf(t, a.AgentID),
		"the profile text stays and the role brief follows the new role")
	assert.Equal(t, "lead", memberRole(t, "alpha", renamed.ConvID), "a label set since the spawn stays")
	assert.Equal(t, "Be brief.\n\n## Role\n\nCopy things.", profileContextOf(t, renamed.AgentID))
	assert.Equal(t, "notifier", memberRole(t, "alpha", explicit.ConvID), "roles the spawn request named stay")
	assert.Equal(t, "Be brief.\n\n## Role\n\nTell the human.", profileContextOf(t, explicit.AgentID))
}

// A role whose brief was edited since the spawn: a re-apply brings the
// current text, as a resume of a fresh spawn would see it.
func TestProfileReapplyRoles_EditedRoleBriefIsReapplied(t *testing.T) {
	f := roleFixture(t)
	a := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	rec := humanReq(t, f, http.MethodPatch, "/v1/roles/notifier",
		map[string]any{"name": "notifier", "brief": "Tell the human, briefly.", "permissions": []string{agentd.PermHumanNotify}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	code, _, raw := postReapply(t, f, "worker", a.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "Be brief.\n\n## Role\n\nTell the human, briefly.", profileContextOf(t, a.AgentID))
	assert.Equal(t, "notifier", memberRole(t, "alpha", a.ConvID))
}
