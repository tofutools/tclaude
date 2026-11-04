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
		IsOwner:             &isOwner,
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

// A group default reaches every spawn into the group, including an
// operator-authored automation's workers, which have no owner to check at
// launch. So an agent may make a profile a group's default only when it could
// confer what that profile carries itself; owning the group is not enough.
func TestManagedIdentityTiers_SettingTheDefaultTakesConferralAuthority(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const setter = "setr-1111-2222-3333-4444"
	f.HaveMember("alpha", setter)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, setter, "test"))
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
	set := func(profile string) int {
		return agentReq(t, f, setter, http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_profile": profile}).Code
	}
	assert.Equal(t, http.StatusForbidden, set("granting"), "an owner without permissions.grant")
	assert.Equal(t, http.StatusForbidden, set("owning"), "an owner without groups.owners.manage")
	assert.Equal(t, http.StatusOK, set("denying"), "a deny only narrows")

	require.NoError(t, db.GrantAgentPermission(setter, agentd.PermPermissionsGrant, "test"))
	require.NoError(t, db.GrantAgentPermission(setter, reviewerSlug, "test"))
	assert.Equal(t, http.StatusOK, set("granting"), "with the authority to confer it")
	require.NoError(t, db.GrantAgentPermissionWithScope(setter, agentd.PermGroupsOwnersManage, `{"group":["alpha"]}`, "test"))
	assert.Equal(t, http.StatusOK, set("owning"), "a grant scoped to the group covers it")

	assert.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_profile": "granting"}).Code, "the operator always may")
}

// ownedRuleWithoutGrant is an agent-owned trigger on alpha whose owner may
// spawn there but holds no permissions.grant.
func ownedRuleWithoutGrant(t *testing.T, f *testharness.Flow, roleRefs []string) int64 {
	t.Helper()
	owner := spawnedOwner(t, f)
	for _, slug := range []string{agentd.PermGroupsTriggersManage, agentd.PermGroupsMembersSpawn} {
		require.NoError(t, db.GrantAgentPermission(owner, slug, "test"))
	}
	ruleID := ownerTriggerOn(t, f, "alpha", owner, roleRefs...)
	return ruleID
}

// A role the rule names is direct intent, but a default profile's grant
// alongside it is still ambient: it is skipped, not a refusal of every firing.
func TestManagedIdentityTiers_DefaultGrantBesideAnExplicitRoleIsSkipped(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateRole(&db.Role{Name: "plain", Brief: "Be plain."})
	require.NoError(t, err)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		PermissionOverrides: map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectGrant}}})
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	outcome := openPR(t, ownedRuleWithoutGrant(t, f, []string{"plain"}), 9)
	got, _ := workerIdentityOf(t, outcome)
	assert.Equal(t, "plain", got.Role)
	assert.Nil(t, got.Overrides)
	assert.Contains(t, outcome.Detail, "permission_overrides ignored (caller lacks "+agentd.PermPermissionsGrant+")")
}

// A default profile's grant the owner cannot confer is dropped, but its
// denies stay: a deny only narrows.
func TestManagedIdentityTiers_SkippedDefaultKeepsItsDenies(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		PermissionOverrides: map[string]db.PermissionOverride{
			reviewerSlug:  {Effect: db.PermEffectGrant},
			"self.rename": {Effect: db.PermEffectDeny},
		}})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	got, _ := workerIdentityOf(t, openPR(t, ownedRuleWithoutGrant(t, f, nil), 9))
	assert.Equal(t, map[string]string{"self.rename": "deny"}, got.Overrides)
}

// An owner who may confer the default's grant passes it on.
func TestManagedIdentityTiers_OwnerWithAuthorityConfersTheDefault(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		PermissionOverrides: map[string]db.PermissionOverride{reviewerSlug: {Effect: db.PermEffectGrant}}})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	owner := spawnedOwner(t, f)
	for _, slug := range []string{agentd.PermGroupsTriggersManage, agentd.PermGroupsMembersSpawn,
		agentd.PermPermissionsGrant, reviewerSlug} {
		require.NoError(t, db.GrantAgentPermission(owner, slug, "test"))
	}
	got, _ := workerIdentityOf(t, openPR(t, ownerTriggerOn(t, f, "alpha", owner), 9))
	assert.Equal(t, map[string]string{reviewerSlug: "grant"}, got.Overrides)
}
