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

// tcl-fg2kqk: re-applying an edited profile re-applies the grants it gave at
// birth. Only rows that still carry the profile's birth record change; a row
// changed since, an explicit grant and a revoked slug stay as they are, and a
// part of the profile that never took effect gives nothing.

// agentGrants is the agent's permission rows as slug → effect, and the rows'
// birth sources.
func agentGrants(t *testing.T, convID string) (map[string]string, map[string]*db.PermissionBirthSource) {
	t.Helper()
	rows, err := db.ListAgentPermissionOverrideRowsForConv(convID)
	require.NoError(t, err)
	effects, sources := map[string]string{}, map[string]*db.PermissionBirthSource{}
	for _, row := range rows {
		effects[row.Slug] = row.Effect
		sources[row.Slug] = row.BirthSource
	}
	return effects, sources
}

func patchProfile(t *testing.T, f *testharness.Flow, name string, extra map[string]any) {
	t.Helper()
	rec := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/"+name, profileBody(name, extra))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestProfileReapplyGrants_OnlyTheProfilesOwnRows(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"permission_overrides": map[string]string{agentd.PermHumanNotify: "grant", agentd.PermHumanClipboard: "grant"},
	})).Code)
	plain := spawnStopped(t, f, "alpha", map[string]any{"name": "plain", "profile": "worker"})
	changed := spawnStopped(t, f, "alpha", map[string]any{"name": "changed", "profile": "worker"})
	revoked := spawnStopped(t, f, "alpha", map[string]any{"name": "revoked", "profile": "worker"})
	explicit := spawnStopped(t, f, "alpha", map[string]any{"name": "explicit", "profile": "worker",
		"permission_overrides": map[string]string{agentd.PermHumanNotify: "grant"}})

	// Since the spawn, the operator narrowed one agent's clipboard and revoked
	// another's notify.
	require.NoError(t, db.SetAgentPermissionOverrideByAgentID(changed.AgentID, agentd.PermHumanClipboard, db.PermEffectDeny, "operator"))
	_, err := db.RevokeAgentPermission(revoked.ConvID, agentd.PermHumanNotify)
	require.NoError(t, err)

	// The edit narrows notify, drops clipboard and adds profiles.manage.
	patchProfile(t, f, "worker", map[string]any{
		"permission_overrides": map[string]string{agentd.PermHumanNotify: "deny", agentd.PermProfilesManage: "grant"},
	})
	preview := profileReapplyPreview(t, f, "worker")
	planned := map[string]map[string]string{}
	for _, a := range preview.Agents {
		planned[a.AgentID] = map[string]string{}
		for _, g := range a.Grants {
			planned[a.AgentID][g.Slug] = g.Status
		}
	}
	assert.Equal(t, map[string]string{
		agentd.PermHumanNotify: "changed", agentd.PermHumanClipboard: "removed", agentd.PermProfilesManage: "added",
	}, planned[plain.AgentID])
	assert.Equal(t, "kept", planned[changed.AgentID][agentd.PermHumanClipboard])
	assert.Equal(t, "not added", planned[revoked.AgentID][agentd.PermHumanNotify])

	for _, a := range []testharness.SpawnResp{plain, changed, revoked, explicit} {
		code, res, raw := postReapply(t, f, "worker", a.AgentID)
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "applied", res.Outcome, raw)
	}

	got, sources := agentGrants(t, plain.ConvID)
	assert.Equal(t, map[string]string{agentd.PermHumanNotify: db.PermEffectDeny, agentd.PermProfilesManage: db.PermEffectGrant}, got)
	require.NotNil(t, sources[agentd.PermProfilesManage], "an added grant records the profile")
	assert.NotZero(t, sources[agentd.PermProfilesManage].ProfileID)

	got, _ = agentGrants(t, changed.ConvID)
	assert.Equal(t, db.PermEffectDeny, got[agentd.PermHumanClipboard], "a row changed since the spawn stays")
	assert.Equal(t, db.PermEffectDeny, got[agentd.PermHumanNotify])

	got, _ = agentGrants(t, revoked.ConvID)
	_, has := got[agentd.PermHumanNotify]
	assert.False(t, has, "a revoked grant is not given back")

	got, _ = agentGrants(t, explicit.ConvID)
	assert.Equal(t, map[string]string{agentd.PermHumanNotify: db.PermEffectGrant}, got,
		"the spawn request's own overrides replaced the profile's, so nothing of the profile's applies")

	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "permissions.reapply", Limit: 50})
	require.NoError(t, err)
	byAgent := map[string]int{}
	for _, e := range entries {
		byAgent[e.TargetAgent]++
	}
	assert.Equal(t, 3, byAgent[plain.AgentID], "one audit row per grant change")
	assert.Zero(t, byAgent[explicit.AgentID])
}

// A profile that gave no overrides at the spawn does not start giving them.
func TestProfileReapplyGrants_NoGrantFromAPartThatNeverApplied(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", nil)).Code)
	a := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	patchProfile(t, f, "worker", map[string]any{"permission_overrides": map[string]string{agentd.PermHumanNotify: "grant"}})
	code, _, raw := postReapply(t, f, "worker", a.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	got, _ := agentGrants(t, a.ConvID)
	assert.Empty(t, got)
}

// A role's grants follow the profile's role refs, recording the role.
func TestProfileReapplyGrants_RolesMoveWithTheirGrants(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	for name, slug := range map[string]string{"notifier": agentd.PermHumanNotify, "copier": agentd.PermHumanClipboard} {
		require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
			map[string]any{"name": name, "permissions": []string{slug}}).Code)
	}
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{"role_refs": []string{"notifier"}})).Code)
	a := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	got, _ := agentGrants(t, a.ConvID)
	require.Equal(t, map[string]string{agentd.PermHumanNotify: db.PermEffectGrant}, got)

	patchProfile(t, f, "worker", map[string]any{"role_refs": []string{"copier"}})
	code, _, raw := postReapply(t, f, "worker", a.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	got, sources := agentGrants(t, a.ConvID)
	assert.Equal(t, map[string]string{agentd.PermHumanClipboard: db.PermEffectGrant}, got)
	require.NotNil(t, sources[agentd.PermHumanClipboard])
	assert.Equal(t, "copier", sources[agentd.PermHumanClipboard].Role)
}

// An agent re-applier needs the authority to confer what a re-apply widens,
// and none to narrow.
func TestProfileReapplyGrants_AgentReapplierConferral(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"permission_overrides": map[string]string{agentd.PermHumanNotify: "grant"},
	})).Code)
	a := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	const caller = "regr-1111-2222-3333-4444"
	f.HaveMember("alpha", caller)
	for _, slug := range []string{agentd.PermProfilesManage, agentd.PermAgentMorph} {
		require.NoError(t, db.GrantAgentPermission(caller, slug, "test"))
	}
	reapply := func() (int, string) {
		rec := agentReq(t, f, caller, http.MethodPost, "/v1/spawn-profiles/worker/reapply", map[string]any{"agent_id": a.AgentID})
		return rec.Code, rec.Body.String()
	}

	patchProfile(t, f, "worker", map[string]any{"permission_overrides": map[string]string{
		agentd.PermHumanNotify: "grant", agentd.PermHumanClipboard: "grant"}})
	code, raw := reapply()
	assert.Equal(t, http.StatusForbidden, code, "widening without permissions.grant: %s", raw)
	got, _ := agentGrants(t, a.ConvID)
	assert.Equal(t, map[string]string{agentd.PermHumanNotify: db.PermEffectGrant}, got, "a refused re-apply changes nothing")

	patchProfile(t, f, "worker", map[string]any{"permission_overrides": map[string]string{agentd.PermHumanNotify: "deny"}})
	code, raw = reapply()
	require.Equal(t, http.StatusOK, code, "narrowing needs no authority: %s", raw)
	got, _ = agentGrants(t, a.ConvID)
	assert.Equal(t, map[string]string{agentd.PermHumanNotify: db.PermEffectDeny}, got)
}
