package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Minting an owner at spawn honours a scoped groups.owners.manage grant the way
// the owners-add endpoint does: a grant scoped to group alpha mints an owner in
// alpha and nowhere else, through a direct spawn and through a trigger worker
// alike. An explicit deny wins over any grant.

// ownerGrant sets up the spawner's groups.owners.manage authority.
type ownerGrant struct {
	name  string
	apply func(t *testing.T, conv string, alpha *db.AgentGroup)
	// allowed is the expected answer per target group.
	allowed map[string]bool
}

var ownerGrantCases = []ownerGrant{
	{"scoped to alpha", func(t *testing.T, conv string, _ *db.AgentGroup) {
		require.NoError(t, db.GrantAgentPermissionWithScope(conv, agentd.PermGroupsOwnersManage, `{"group":["alpha"]}`, "test"))
	}, map[string]bool{"alpha": true, "beta": false}},
	{"unscoped", func(t *testing.T, conv string, _ *db.AgentGroup) {
		require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsOwnersManage, "test"))
	}, map[string]bool{"alpha": true, "beta": true}},
	// The group grants the slug to its members; the agent's own deny wins.
	{"denied over a grant", func(t *testing.T, conv string, alpha *db.AgentGroup) {
		require.NoError(t, db.ReplaceAgentGroupPermissions(alpha.ID, []string{agentd.PermGroupsOwnersManage}, "test"))
		require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermGroupsOwnersManage, db.PermEffectDeny, "test"))
	}, map[string]bool{"alpha": false, "beta": false}},
}

func TestSpawnOwnerScope_DirectSpawn(t *testing.T) {
	for _, c := range ownerGrantCases {
		for _, target := range []string{"alpha", "beta"} {
			t.Run(fmt.Sprintf("%s into %s", c.name, target), func(t *testing.T) {
				f := newFlow(t)
				alpha := f.HaveGroup("alpha")
				f.HaveGroup("beta")
				const spawner = "scop-1111-2222-3333-4444"
				f.HaveMember("alpha", spawner)
				f.HaveMember("beta", spawner)
				require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsMembersSpawn, "test"))
				c.apply(t, spawner, alpha)

				spawn := f.AsAgent(spawner).SpawnWith(target, map[string]any{"name": "deputy", "is_owner": true})
				if !c.allowed[target] {
					assert.Equalf(t, http.StatusForbidden, spawn.Code, "body=%s", spawn.Raw)
					return
				}
				require.Equalf(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)
				g, err := db.GetAgentGroupByName(target)
				require.NoError(t, err)
				assert.True(t, ownsGroup(t, g.ID, spawn.ConvID), "the child owns %s", target)
			})
		}
	}
}

func TestSpawnOwnerScope_TriggerWorker(t *testing.T) {
	for _, c := range ownerGrantCases {
		for _, target := range []string{"alpha", "beta"} {
			t.Run(fmt.Sprintf("%s into %s", c.name, target), func(t *testing.T) {
				f := triggerFlow(t)
				var alpha *db.AgentGroup
				for _, name := range []string{"alpha", "beta"} {
					g := f.HaveGroup(name)
					if name == "alpha" {
						alpha = g
					}
					_, err := db.SetAgentGroupDefaultCwd(name, t.TempDir())
					require.NoError(t, err)
				}
				isOwner := true
				_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", IsOwner: &isOwner})
				require.NoError(t, err)
				owner := spawnedOwner(t, f)
				for _, slug := range []string{agentd.PermGroupsTriggersManage, agentd.PermAgentSpawn} {
					require.NoError(t, db.GrantAgentPermission(owner, slug, "test"))
				}
				c.apply(t, owner, alpha)

				outcome := openPR(t, ownerTriggerOn(t, f, target, owner), 9)
				if !c.allowed[target] {
					assert.Equalf(t, "permission_denied", outcome.Outcome, "%+v", outcome)
					return
				}
				require.Equalf(t, "spawned", outcome.Outcome, "%+v", outcome)
				conv, err := db.CurrentConvForAgent(outcome.SpawnedAgent)
				require.NoError(t, err)
				g, err := db.GetAgentGroupByName(target)
				require.NoError(t, err)
				assert.True(t, ownsGroup(t, g.ID, conv), "the worker owns %s", target)
			})
		}
	}
}

// spawnedOwner is a rule owner with the launch posture a parent needs.
func spawnedOwner(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "owner", "initial_message": "Own it."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	agentd.WaitForBackgroundForTest()
	return spawn.ConvID
}

// ownerTriggerOn stores a PR-opened spawn trigger on group, owned by owner's
// agent, whose worker launches from profile "reviewer" with roleRefs.
func ownerTriggerOn(t *testing.T, f *testharness.Flow, group, owner string, roleRefs ...string) int64 {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	f.HaveConvWithTitle(refusalAuthor, "author")
	f.HaveMember(group, refusalAuthor)
	ownerAgent, err := db.AgentIDForConv(owner)
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "owners", Enabled: true, OwnerAgent: ownerAgent,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftExclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
			Spawn: &db.TriggerSpawnAction{Profile: "reviewer", NameTemplate: "review-{{pr.number}}",
				InstructionTemplate: "Review {{pr.url}}.", MaxLiveWorkers: 5, RoleRefs: roleRefs}}}})
	require.NoError(t, err)
	return ruleID
}
