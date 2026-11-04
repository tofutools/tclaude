package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A trigger, schedule or process worker takes its roles, role label,
// description, saved prompts, startup context, owner flag and permission
// overrides from its own profile, then the group's default profile, then the
// global default profile, exactly as a direct spawn naming that profile does,
// and with the rule owner's authority checked the same way (tcl-a2e791, Q12).

type workerIdentity struct {
	Role, Descr string
	Owner       bool
	Overrides   map[string]string
}

func workerIdentityOf(t *testing.T, outcome db.TriggerActionOutcome) (workerIdentity, string) {
	t.Helper()
	require.Equalf(t, "spawned", outcome.Outcome, "%+v", outcome)
	conv, err := db.CurrentConvForAgent(outcome.SpawnedAgent)
	require.NoError(t, err)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	var out workerIdentity
	for _, m := range members {
		if m.ConvID == conv {
			out.Role, out.Descr = m.Role, m.Descr
		}
	}
	out.Owner = ownsGroup(t, g.ID, conv)
	out.Overrides, err = db.ListAgentPermissionOverridesForConv(conv)
	require.NoError(t, err)
	if len(out.Overrides) == 0 {
		out.Overrides = nil
	}
	return out, conv
}

const reviewerSlug = agentd.PermHumanNotify

func TestManagedIdentityTiers_NothingConfiguredUnchanged(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude",
		Role: "reviewer", Descr: "reviews PRs"})
	require.NoError(t, err)
	outcome := openPR(t, refusalTrigger(t, f, ""), 9)
	got, _ := workerIdentityOf(t, outcome)
	assert.Equal(t, workerIdentity{Role: "reviewer", Descr: "reviews PRs"}, got)
	assert.Empty(t, outcome.Detail)
}

// The pinned case Q12 asked for: a group default profile granting a
// permission reaches a trigger worker.
func TestManagedIdentityTiers_GroupDefaultGrantReachesWorker(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		Descr: "house worker", PermissionOverrides: map[string]db.PermissionOverride{
			reviewerSlug: {Effect: db.PermEffectGrant}}})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	outcome := openPR(t, refusalTrigger(t, f, ""), 9)
	got, _ := workerIdentityOf(t, outcome)
	assert.Equal(t, "grant", got.Overrides[reviewerSlug], "the group default's grant reaches the worker")
	assert.Equal(t, "house worker", got.Descr)
	assert.Contains(t, outcome.Detail, `"group-default"`, "the run discloses which tier conferred it")
}

// ...and an explicit deny still wins: the worker's own profile is the higher
// tier, and its override map is taken whole.
func TestManagedIdentityTiers_ExplicitDenyWins(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		PermissionOverrides: map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectGrant}}})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude",
		PermissionOverrides: map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectDeny}}})
	require.NoError(t, err)
	got, _ := workerIdentityOf(t, openPR(t, refusalTrigger(t, f, ""), 9))
	assert.Equal(t, map[string]string{reviewerSlug: "deny"}, got.Overrides)
}

// A role a default profile names brings its label, brief and grants, and the
// worker's own profile's deny still narrows the role's grant.
func TestManagedIdentityTiers_GroupDefaultRole(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateRole(&db.Role{Name: "auditor", Brief: "Audit every change.",
		Permissions: []db.PermissionGrant{{Slug: reviewerSlug}}})
	require.NoError(t, err)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		RoleRefs: []string{"auditor"}})
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	ruleID := refusalTrigger(t, f, "")
	got, conv := workerIdentityOf(t, openPR(t, ruleID, 9))
	assert.Equal(t, "auditor", got.Role)
	assert.Equal(t, "grant", got.Overrides[reviewerSlug])
	prompt, ok := f.World.SpawnInitialPrompt(conv)
	require.True(t, ok)
	assert.Contains(t, prompt, "Audit every change.", "the role's brief joins the startup context")

	stored, err := db.GetSpawnProfile("reviewer")
	require.NoError(t, err)
	stored.PermissionOverrides = map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectDeny}}
	require.NoError(t, db.UpdateSpawnProfile(stored))
	got, _ = workerIdentityOf(t, openPR(t, ruleID, 10))
	assert.Equal(t, "deny", got.Overrides[reviewerSlug], "the worker's own deny beats the role's grant")
}

// An agent-owned rule confers only what its owner may: a default profile's
// grant the owner cannot confer is skipped and disclosed, while the same grant
// on the worker's own profile still refuses the firing loudly.
func TestManagedIdentityTiers_OwnerAuthority(t *testing.T) {
	f := uniformTriggerGroup(t)
	isOwner := true
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		IsOwner: &isOwner,
		PermissionOverrides: map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectGrant}}})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	// A spawned owner, so it has the launch posture a parent needs.
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "owner", "initial_message": "Own it."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	agentd.WaitForBackgroundForTest()
	owner := spawn.ConvID
	ownerAgent, err := db.AgentIDForConv(owner)
	require.NoError(t, err)
	for _, slug := range []string{agentd.PermGroupsTriggersManage, agentd.PermGroupsMembersSpawn} {
		require.NoError(t, db.GrantAgentPermission(owner, slug, "test"))
	}
	ruleID := refusalTrigger(t, f, ownerAgent)

	outcome := openPR(t, ruleID, 9)
	got, _ := workerIdentityOf(t, outcome)
	assert.Equal(t, workerIdentity{}, got, "nothing the owner cannot confer reaches the worker")
	assert.Contains(t, outcome.Detail, "is_owner ignored (caller lacks "+agentd.PermGroupsOwnersManage+")")
	assert.Contains(t, outcome.Detail, "permission_overrides ignored (caller lacks "+agentd.PermPermissionsGrant+")")

	stored, err := db.GetSpawnProfile("reviewer")
	require.NoError(t, err)
	stored.PermissionOverrides = map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectGrant}}
	require.NoError(t, db.UpdateSpawnProfile(stored))
	refused := openPR(t, ruleID, 10)
	assert.Equal(t, "permission_denied", refused.Outcome, "the worker's own profile is direct intent: %+v", refused)
	assert.Contains(t, refused.Detail, agentd.PermPermissionsGrant)
}
