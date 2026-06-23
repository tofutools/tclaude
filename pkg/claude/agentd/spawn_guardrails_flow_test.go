package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Spawn guardrails — runaway-prevention for an agent the human granted
// `groups.spawn`. Three checks fire in handleGroupSpawn before any CC
// subprocess is launched: a per-group member cap (binds the human
// too), a group restriction (agent may only spawn into a group it is a
// member/owner of), and a per-caller spawn rate limit. See
// spawn_guardrails.go.
//
// Every scenario asserts at a real surface — the spawn HTTP status and
// `tclaude agent groups members` (GET /v1/groups/{name}/members) — not
// the simulator internals.

// Scenario: an agent that holds `groups.spawn` and is a MEMBER of the
// target group spawns a worker into it. The group restriction must let
// this through — growing your own team is exactly the safe case.
func TestSpawnGuardrails_AgentSpawnsIntoOwnGroup_OK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		const lead = "lead-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("alpha", lead)
		require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))

		// f.Spawn fatals on any non-200 — the success IS the assertion.
		resp := f.AsAgent(lead).Spawn("alpha", "worker")
		require.NotEmpty(t, resp.ConvID, "spawn must return a conv-id")

		// Real surface: the worker is now in the group.
		var found bool
		for _, m := range f.ListGroupMembers("alpha") {
			if m.ConvID == resp.ConvID {
				found = true
			}
		}
		assert.True(t, found, "spawned worker must appear in group alpha")
	})
}

// Scenario: a coordinator agent that OWNS the target group but is not a
// member of it spawns a worker in. Owner counts alongside member — a
// group owner already wields unilateral power over the group's members
// elsewhere in the daemon, and a coordinator that grows a team is
// typically an owner rather than a member.
func TestSpawnGuardrails_AgentOwnerSpawnsIntoGroup_OK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		g := f.HaveGroup("alpha")

		const po = "popo-aaaa-bbbb-cccc-222222222222"
		require.NoError(t, db.AddAgentGroupOwner(g.ID, po, "test"))
		require.NoError(t, db.GrantAgentPermission(po, agentd.PermGroupsSpawn, "test"))

		resp := f.AsAgent(po).Spawn("alpha", "worker")
		require.NotEmpty(t, resp.ConvID, "an owner (non-member) must be allowed to spawn into its group")
	})
}

// Scenario: an agent with `groups.spawn` tries to spawn into a group it
// is neither a member nor an owner of. The group restriction refuses it
// with 403 — a spawn-capable agent grows its OWN team, not arbitrary
// new ones. No member is added.
func TestSpawnGuardrails_AgentSpawnsIntoForeignGroup_Refused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha") // the target — the caller is NOT in it
		f.HaveGroup("beta")  // the caller's own group

		const lead = "lead-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("beta", lead)
		require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))

		resp := f.AsAgent(lead).SpawnWith("alpha", map[string]any{"alias": "worker"})
		require.Equal(t, http.StatusForbidden, resp.Code,
			"spawn into a non-member group must be refused; body=%s", resp.Raw)
		assert.Contains(t, string(resp.Raw), "group_restricted", "error code")

		// Real surface: nothing was added to alpha.
		assert.Empty(t, f.ListGroupMembers("alpha"),
			"a refused spawn must not add a member")
	})
}

// Scenario: the group restriction's allowlist (agent.spawn_allowed_groups)
// widens the rule — an agent may spawn into an allowlisted group even
// when it is neither a member nor an owner of it.
func TestSpawnGuardrails_AllowlistWidensForeignSpawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := agentd.SpawnAllowedGroups
		agentd.SpawnAllowedGroups = []string{"alpha"}
		t.Cleanup(func() { agentd.SpawnAllowedGroups = prev })

		f := newFlow(t)
		f.HaveGroup("alpha")
		f.HaveGroup("beta")

		const lead = "lead-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("beta", lead)
		require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))

		resp := f.AsAgent(lead).SpawnWith("alpha", map[string]any{"alias": "worker"})
		require.Equal(t, http.StatusOK, resp.Code,
			"an allowlisted group must accept the spawn; body=%s", resp.Raw)
	})
}

// Scenario: with the restriction toggled off globally
// (agent.spawn_group_restriction = false) an agent may spawn into any
// group, member or not.
func TestSpawnGuardrails_RestrictionToggleOff_AllowsForeignSpawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := agentd.SpawnGroupRestriction
		agentd.SpawnGroupRestriction = false
		t.Cleanup(func() { agentd.SpawnGroupRestriction = prev })

		f := newFlow(t)
		f.HaveGroup("alpha")
		f.HaveGroup("beta")

		const lead = "lead-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("beta", lead)
		require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))

		resp := f.AsAgent(lead).SpawnWith("alpha", map[string]any{"alias": "worker"})
		require.Equal(t, http.StatusOK, resp.Code,
			"a disabled restriction must accept any spawn; body=%s", resp.Raw)
	})
}

// Scenario: an agent in a tight loop spawns repeatedly. The first
// SpawnMaxPerWindow spawns land; the next is refused with 429. The
// real-surface invariant — the rate-limited attempt does NOT record a
// spawn-history row, so a retry loop can't extend the window by
// burning rejected attempts.
func TestSpawnGuardrails_RateLimit_RefusesAfterN(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := agentd.SpawnMaxPerWindow
		agentd.SpawnMaxPerWindow = 2
		t.Cleanup(func() { agentd.SpawnMaxPerWindow = prev })

		f := newFlow(t)
		f.HaveGroup("alpha")

		const lead = "lead-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("alpha", lead)
		require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))

		a := f.AsAgent(lead)
		r1 := a.SpawnWith("alpha", map[string]any{"alias": "w1"})
		require.Equal(t, http.StatusOK, r1.Code, "spawn #1 body=%s", r1.Raw)
		r2 := a.SpawnWith("alpha", map[string]any{"alias": "w2"})
		require.Equal(t, http.StatusOK, r2.Code, "spawn #2 body=%s", r2.Raw)

		r3 := a.SpawnWith("alpha", map[string]any{"alias": "w3"})
		require.Equal(t, http.StatusTooManyRequests, r3.Code,
			"spawn #3 must be rate-limited; body=%s", r3.Raw)
		assert.Contains(t, string(r3.Raw), "rate_limited", "error code")

		// Real-surface invariant: exactly the two successful spawns were
		// recorded — the refused third did not consume a slot.
		n, err := db.CountSpawnsSince(lead, time.Now().Add(-time.Hour))
		require.NoError(t, err, "CountSpawnsSince")
		assert.Equal(t, 2, n, "a rate-limited attempt must not record a spawn-history row")
	})
}

// Scenario: the spawn rate limit is per-caller. Two different
// caller-agents each spawning up to the cap don't throttle each other.
func TestSpawnGuardrails_RateLimit_IsPerCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := agentd.SpawnMaxPerWindow
		agentd.SpawnMaxPerWindow = 1
		t.Cleanup(func() { agentd.SpawnMaxPerWindow = prev })

		f := newFlow(t)
		f.HaveGroup("alpha")

		const leadA = "leda-aaaa-bbbb-cccc-111111111111"
		const leadB = "ledb-aaaa-bbbb-cccc-222222222222"
		f.HaveMember("alpha", leadA)
		f.HaveMember("alpha", leadB)
		require.NoError(t, db.GrantAgentPermission(leadA, agentd.PermGroupsSpawn, "test"))
		require.NoError(t, db.GrantAgentPermission(leadB, agentd.PermGroupsSpawn, "test"))

		rA := f.AsAgent(leadA).SpawnWith("alpha", map[string]any{"alias": "wa"})
		require.Equal(t, http.StatusOK, rA.Code, "lead-a spawn body=%s", rA.Raw)
		// lead-b is a distinct caller — it has its own fresh allowance.
		rB := f.AsAgent(leadB).SpawnWith("alpha", map[string]any{"alias": "wb"})
		require.Equal(t, http.StatusOK, rB.Code,
			"a different caller-agent must not be throttled by lead-a's spawn; body=%s", rB.Raw)
	})
}

// Scenario: a group at its max_members cap refuses a further spawn with
// 409 — and the cap binds the HUMAN too, because it is a hard property
// of the group, not a limit on the caller. A human raises the cap to
// add more.
func TestSpawnGuardrails_MaxMembers_RefusesWhenFull_EvenForHuman(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		const incumbent = "exis-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("alpha", incumbent)
		// Cap the group at its current size — it is now full.
		_, err := db.SetAgentGroupMaxMembers("alpha", 1)
		require.NoError(t, err, "SetAgentGroupMaxMembers")

		resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "worker"})
		require.Equal(t, http.StatusConflict, resp.Code,
			"a spawn into a full group must be refused, human or not; body=%s", resp.Raw)
		assert.Contains(t, string(resp.Raw), "group_full", "error code")

		// Real surface: no member slipped past the cap.
		assert.Len(t, f.ListGroupMembers("alpha"), 1,
			"membership must stay at the cap")
	})
}

// Scenario: raising max_members past the current size lets a spawn
// through again — the dual of the refusal test, pinning the obvious
// regression where the cap never releases.
func TestSpawnGuardrails_MaxMembers_RaisingCapAllowsSpawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		const incumbent = "exis-aaaa-bbbb-cccc-111111111111"
		f.HaveMember("alpha", incumbent)
		_, err := db.SetAgentGroupMaxMembers("alpha", 1)
		require.NoError(t, err)

		// Full — refused.
		r1 := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w1"})
		require.Equal(t, http.StatusConflict, r1.Code, "body=%s", r1.Raw)

		// Human raises the cap; the next spawn lands.
		_, err = db.SetAgentGroupMaxMembers("alpha", 2)
		require.NoError(t, err)
		r2 := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w2"})
		require.Equal(t, http.StatusOK, r2.Code,
			"raising the cap must let the spawn through; body=%s", r2.Raw)
	})
}

// Scenario: the human bypasses both agent-only guardrails. With the
// group restriction on and an agent rate limit that would block after
// one spawn, a human — a member of nothing, with no conv-id — still
// spawns freely into a group, repeatedly.
func TestSpawnGuardrails_HumanBypassesRestrictionAndRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := agentd.SpawnMaxPerWindow
		agentd.SpawnMaxPerWindow = 1 // would block an agent after one spawn
		t.Cleanup(func() { agentd.SpawnMaxPerWindow = prev })

		require.True(t, agentd.SpawnGroupRestriction,
			"the group restriction must be on by default for this test to mean anything")

		f := newFlow(t)
		f.HaveGroup("alpha") // the human is (and cannot be) a member of nothing

		for i := 1; i <= 3; i++ {
			resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w"})
			require.Equal(t, http.StatusOK, resp.Code,
				"human spawn #%d must bypass every agent guardrail; body=%s", i, resp.Raw)
		}
	})
}
