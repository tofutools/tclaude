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

// tagsResp mirrors the daemon's tag-endpoint wire shape (/v1/whoami/tags and
// /v1/agent/{sel}/tags).
type tagsResp struct {
	ConvID        string   `json:"conv_id"`
	Tags          []string `json:"tags"`
	CallerConv    string   `json:"caller_conv"`
	CallerAgentID string   `json:"caller_agent_id"`
}

// tagsForConv resolves a conv to its actor and returns its stored tags.
func tagsForConv(t *testing.T, conv string) []string {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "conv %s has an actor", conv)
	tags, err := db.ListAgentTags(agentID)
	require.NoError(t, err)
	return tags
}

// Scenario: an agent replaces, reads and clears its OWN tag set via
// /v1/whoami/tags (self.tags). The set is stored sorted; a replace with a
// smaller list drops the removed tags; an empty list clears.
func TestTags_SelfReplaceGetClear(t *testing.T) {
	f := newFlow(t)
	const worker = "wtag-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtag", "tmux-wtag", f.TestCwd("wtag"))
	f.HaveMember("alpha", worker)
	require.NoError(t,
		db.SetAgentPermissionOverride(worker, agentd.PermSelfTags, db.PermEffectGrant, "test"),
		"grant self.tags")

	// Replace.
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/whoami/tags",
			map[string]any{"tags": []string{"zeta", "alpha", "alpha"}}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "self replace: body=%s", rec.Body.String())
	var set tagsResp
	testharness.DecodeJSON(t, rec, &set)
	assert.Equal(t, []string{"alpha", "zeta"}, set.Tags, "stored de-duped + sorted")
	assert.Empty(t, set.CallerConv, "self write carries no caller_conv")

	// Get.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/tags", nil), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "self get: body=%s", rec.Body.String())
	var got tagsResp
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, []string{"alpha", "zeta"}, got.Tags)

	// Replace with a smaller set drops the removed tag.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/whoami/tags",
			map[string]any{"tags": []string{"alpha"}}), worker))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"alpha"}, tagsForConv(t, worker))

	// Clear via empty list.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/whoami/tags",
			map[string]any{"tags": []string{}}), worker))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, tagsForConv(t, worker), "empty list clears the set")
}

// Scenario: a self replace carrying an invalid tag (a control character) is
// refused with a 400 — the UI-hygiene validation, not an injection guard.
func TestTags_SelfRejectsInvalidTag(t *testing.T) {
	f := newFlow(t)
	const worker = "wtg2-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtg2", "tmux-wtg2", f.TestCwd("wtg2"))
	f.HaveMember("alpha", worker)
	require.NoError(t,
		db.SetAgentPermissionOverride(worker, agentd.PermSelfTags, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/whoami/tags",
			map[string]any{"tags": []string{"ok", "bad\ntag"}}), worker))
	assert.Equalf(t, http.StatusBadRequest, rec.Code, "bad tag ⇒ 400; body=%s", rec.Body.String())
	assert.Empty(t, tagsForConv(t, worker), "the rejected replace stored nothing")
}

// Scenario: a lead that OWNS the group sets a worker's tags via the
// manager-pattern --target route — no agent.tags slug needed; group ownership
// is the structural bypass (mirrors the cross-agent task/rename verbs).
func TestTags_OwnerSetsWorkerWithoutSlug(t *testing.T) {
	f := newFlow(t)
	const lead = "ltag-aaaa-bbbb-cccc-dddd"
	const worker = "wtg3-aaaa-bbbb-cccc-dddd"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtg3", "tmux-wtg3", f.TestCwd("wtg3"))
	f.HaveMember("squad", worker)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/agent/"+worker+"/tags",
			map[string]any{"tags": []string{"reviewer"}}), lead))
	require.Equalf(t, http.StatusOK, rec.Code, "owner set: body=%s", rec.Body.String())
	var resp tagsResp
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, []string{"reviewer"}, resp.Tags)
	assert.Equal(t, lead, resp.CallerConv, "cross-agent write echoes caller_conv")
	assert.Equal(t, []string{"reviewer"}, tagsForConv(t, worker))
}

// Scenario: an agent that neither holds agent.tags nor owns a group containing
// the target is refused (403) on a cross-agent set.
func TestTags_CrossAgentDeniedWithoutOwnershipOrSlug(t *testing.T) {
	f := newFlow(t)
	const stranger = "stag-aaaa-bbbb-cccc-dddd"
	const worker = "wtg4-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtg4", "tmux-wtg4", f.TestCwd("wtg4"))
	f.HaveMember("squad", worker)
	f.HaveEnrolledAgent(stranger) // a known agent, but no ownership / slug

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/agent/"+worker+"/tags",
			map[string]any{"tags": []string{"x"}}), stranger))
	assert.Equal(t, http.StatusForbidden, rec.Code, "no slug + not owner ⇒ 403; body=%s", rec.Body.String())
}

// Scenario: an explicit per-conv DENY of self.tags beats the default grant —
// a self replace is refused (403) even though the slug is default-granted.
func TestTags_SelfDenyOverridesDefaultGrant(t *testing.T) {
	f := newFlow(t)
	const worker = "wtg5-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtg5", "tmux-wtg5", f.TestCwd("wtg5"))
	f.HaveMember("alpha", worker)
	require.NoError(t,
		db.SetAgentPermissionOverride(worker, agentd.PermSelfTags, db.PermEffectDeny, "test"),
		"deny self.tags")

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPut, "/v1/whoami/tags",
			map[string]any{"tags": []string{"x"}}), worker))
	assert.Equal(t, http.StatusForbidden, rec.Code, "explicit deny wins; body=%s", rec.Body.String())
}

// Scenario (auto-stamp, instantiate): every agent a plain instantiate spawns is
// stamped with the tf:<template-name> task-force tag, and it rides the dashboard
// snapshot on the member row.
func TestTags_AutoStampOnInstantiate(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "frontend-squad",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead"},
				{Name: "dev", Role: "dev"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/frontend-squad/instantiate",
		map[string]any{"group_name": "web", "mission": "ship it"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	members := f.ListGroupMembers("web")
	require.Len(t, members, 2, "both roster agents spawned")
	for _, m := range members {
		assert.Contains(t, tagsForConv(t, m.ConvID), "tf:frontend-squad",
			"member %s carries the task-force tag", m.ConvID)
	}

	// The tag rides the dashboard snapshot on the member row.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m := findDashMember(snap, "web", members[0].ConvID)
	require.NotNil(t, m, "member missing from web snapshot")
	assert.Contains(t, m.Tags, "tf:frontend-squad", "tag rides the snapshot")
}

// Scenario (auto-stamp, reinforce incl. a later choreography wave): a two-wave
// reinforce stamps the tf:<template> tag on BOTH the synchronous wave-0 member
// AND the wave-1 member the background runner spawns once wave 0 settles. Same
// template deployed additively → the same tag (a set, so no duplicate).
func TestTags_AutoStampOnReinforceAcrossWaves(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	const veteran = "77777777-aaaa-bbbb-cccc-000000000007"
	f.HaveMember("crew", veteran)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead", Wave: 0},
				{Name: "dev", Role: "dev", Wave: 1},
			},
		}).Code, "create two-wave template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	// Wave 0 (lead) is up and tagged.
	leadConv := memberByRole(t, "crew", "lead")
	require.NotEmpty(t, leadConv)
	assert.Contains(t, tagsForConv(t, leadConv), "tf:reinforcements", "wave-0 lead is tagged")

	// Settle wave 0 → the runner spawns wave 1 (dev), which must also be tagged.
	settleWaveMember(t, f, leadConv)
	devConv := memberByRole(t, "crew", "dev")
	require.NotEmpty(t, devConv, "wave 1 spawned after wave 0 settled")
	assert.Contains(t, tagsForConv(t, devConv), "tf:reinforcements", "wave-1 dev is tagged too")

	// The pre-existing veteran (not template-spawned) carries no task-force tag.
	assert.NotContains(t, tagsForConv2(t, veteran), "tf:reinforcements",
		"the veteran is not a reinforcement member")
}

// tagsForConv2 is tagsForConv but tolerant of a conv with no actor (returns nil
// rather than failing) — used for the pre-seeded veteran whose enrollment the
// harness may leave partial.
func tagsForConv2(t *testing.T, conv string) []string {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	if agentID == "" {
		return nil
	}
	tags, err := db.ListAgentTags(agentID)
	require.NoError(t, err)
	return tags
}
