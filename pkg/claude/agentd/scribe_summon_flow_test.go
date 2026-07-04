package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the scribe-summon endpoint behind the dashboard's "Edit
// with agent" buttons (JOH-361): a human summon spawns a pre-briefed,
// pre-granted scribe; a repeat click reuses the live one rather than
// double-spawning; and an agent caller is gated exactly like the spawn path
// (groups.spawn + — because a summon carries birth-time grants —
// permissions.grant). Asserted at the real surfaces the dashboard reads:
// db.ListAgentGroupMembers (the agent listing) and
// db.ListAgentPermissionOverridesForConv (the granted slugs).

// scribeSummonResp is the decoded /v1/scribe response.
type scribeSummonResp struct {
	Name      string `json:"name"`
	ConvID    string `json:"conv_id"`
	Reused    bool   `json:"reused"`
	FocusMode string `json:"focus_mode"`
}

// stubScribeTerminal records how many times a scribe window was opened and
// returns success (native), so summonScribe's auto-focus never touches a real
// terminal.
func stubScribeTerminal(t *testing.T) *int {
	t.Helper()
	var opens int
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opens++
		return nil
	}))
	return &opens
}

func decodeScribeResp(t *testing.T, rec *httptest.ResponseRecorder) scribeSummonResp {
	t.Helper()
	var resp scribeSummonResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode /v1/scribe body=%s", rec.Body.String())
	return resp
}

// Scenario: a human summons a scribe. It comes up in its own eponymous
// one-member group, holding exactly the requested slug, and its window is
// opened.
func TestScribeSummon_HumanCreatesGrantedScribe(t *testing.T) {
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "You edit summoning circles on this daemon. Discover them with `tclaude agent templates ls`.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())

	resp := decodeScribeResp(t, rec)
	require.NotEmpty(t, resp.ConvID, "summon returned a conv-id")
	assert.False(t, resp.Reused, "a first summon is a fresh spawn, not a reuse")

	// The scribe's eponymous group exists and holds exactly one member — the
	// agent-listing surface the dashboard renders.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g, "scribe group was created")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "exactly one scribe in the group")
	assert.Equal(t, resp.ConvID, members[0].ConvID, "the member is the summoned scribe")

	// The requested slug is a real persisted grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "templates.manage granted at birth")

	// The scribe's window was opened.
	assert.Equal(t, 1, *opens, "summon opened the scribe's terminal window")
}

// Scenario: a repeat click reuses the live scribe rather than spawning a
// second — the group still holds exactly one member — and re-opens its window.
func TestScribeSummon_ReuseIfAliveNoDoubleSpawn(t *testing.T) {
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit the circle named feature-team.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	assert.False(t, first.Reused, "first summon spawns")

	second := summon()
	assert.True(t, second.Reused, "second summon reuses the live scribe")
	assert.Equal(t, first.ConvID, second.ConvID, "reuse returns the same scribe conv")

	// Still exactly one scribe — reuse did not litter a second.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "reuse-if-alive spawned no second scribe")

	// The grant is (still) present after reuse's idempotent re-grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "grant intact after reuse")

	// Both summons opened a window (fresh spawn's auto-focus + reuse's re-focus).
	assert.Equal(t, 2, *opens, "each summon opened the scribe's window")
}

// Scenario: after the scribe's session dies (e.g. a daemon restart leaves the
// membership row but kills the tmux session), a re-summon spawns a FRESH scribe
// and prunes the dead one — the group does not accumulate stale members.
func TestScribeSummon_DeadScribePrunedOnResummon(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit summoning circles.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.False(t, first.Reused, "first summon spawns")

	// Kill the scribe's tmux session but leave its membership row (what a daemon
	// restart does).
	sessions, err := db.FindSessionsByConvID(first.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the fresh scribe has a session row")
	f.MarkOffline(sessions[0].TmuxSession)

	second := summon()
	assert.False(t, second.Reused, "a dead scribe is not reused — a fresh one is spawned")
	assert.NotEqual(t, first.ConvID, second.ConvID, "the fresh scribe is a new conv")

	// The dead scribe was pruned: exactly one (live) member remains.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "the dead scribe was pruned, not accumulated")
	assert.Equal(t, second.ConvID, members[0].ConvID, "the sole member is the fresh scribe")
}

// Scenario: gating parity with the spawn path. An agent caller is refused
// unless it holds groups.spawn AND — because a summon applies birth-time grants
// — permissions.grant; the human always passes.
func TestScribeSummon_AgentGatedLikeSpawn(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	f.HaveGroup("callers")

	post := func(conv string) *httptest.ResponseRecorder {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	}

	// (a) An agent with NEITHER slug is refused outright.
	const bare = "bare-1111-2222-3333-4444"
	f.HaveMember("callers", bare)
	assert.Equalf(t, http.StatusForbidden, post(bare).Code,
		"an agent without groups.spawn is refused")

	// (b) An agent with groups.spawn but NOT permissions.grant is still refused —
	// a summon carries birth-time grants, so it needs the grant slug too.
	const spawnOnly = "spwn-1111-2222-3333-4444"
	f.HaveMember("callers", spawnOnly)
	require.NoError(t, db.GrantAgentPermission(spawnOnly, agentd.PermGroupsSpawn, "test"))
	assert.Equalf(t, http.StatusForbidden, post(spawnOnly).Code,
		"granting a scribe slugs needs permissions.grant")

	// (c) An agent holding both slugs is allowed — same bar the spawn path sets.
	const granter = "good-1111-2222-3333-4444"
	f.HaveMember("callers", granter)
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermPermissionsGrant, "test"))
	rec := post(granter)
	require.Equalf(t, http.StatusOK, rec.Code, "authorised agent summon body=%s", rec.Body.String())
	assert.Equal(t, "grant",
		mustOverrides(t, decodeScribeResp(t, rec).ConvID)[agentd.PermTemplatesManage],
		"authorised agent's scribe carries the grant")
}

// mustOverrides is a tiny read helper for the per-conv override map.
func mustOverrides(t *testing.T, conv string) map[string]string {
	t.Helper()
	m, err := db.ListAgentPermissionOverridesForConv(conv)
	require.NoError(t, err)
	return m
}

// Scenario: confused-deputy guard. A summon whose name collides with a real,
// non-scribe group must NOT resolve that group — it fails closed rather than
// re-granting/re-briefing a foreign agent or spawning a stray scribe into it.
func TestScribeSummon_RefusesForeignGroupCollision(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// A real working group + a live member that happens to share the scribe name.
	f.HaveGroup("circle-scribe")
	const foreigner = "frgn-1111-2222-3333-4444"
	f.HaveMember("circle-scribe", foreigner)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	assert.Equalf(t, http.StatusConflict, rec.Code, "summon into a non-scribe group must 409; body=%s", rec.Body.String())

	// The foreign member was neither granted the scribe slug nor pulled in.
	overrides, err := db.ListAgentPermissionOverridesForConv(foreigner)
	require.NoError(t, err)
	assert.Empty(t, overrides[agentd.PermTemplatesManage], "the foreign agent was not granted templates.manage")
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "no stray scribe was spawned into the foreign group")
}

// Scenario: input validation at the boundary — an unknown slug, a missing
// slug set, a missing brief and a missing name each 400 without spawning.
func TestScribeSummon_ValidationRejections(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown slug", map[string]any{"name": "circle-scribe", "slugs": []string{"not.a.real.slug"}, "brief": "x"}},
		{"no slugs", map[string]any{"name": "circle-scribe", "slugs": []string{}, "brief": "x"}},
		{"no brief", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}}},
		{"no name", map[string]any{"slugs": []string{agentd.PermTemplatesManage}, "brief": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", tc.body)))
			assert.Equalf(t, http.StatusBadRequest, rec.Code, "expected 400; body=%s", rec.Body.String())
		})
	}
}
