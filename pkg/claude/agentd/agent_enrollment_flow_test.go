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

// Flow coverage for the agent-enrollment feature: the explicit
// promote / retire / reinstate boundary between a plain conversation
// and an agent, exercised through the same dashboard surfaces the
// browser uses (/api/snapshot, /api/agents/{conv}/{verb},
// /api/cleanup/agents).

func agentInSnap(agents []dashAgent, conv string) bool {
	for _, a := range agents {
		if a.ConvID == conv {
			return true
		}
	}
	return false
}

func convInSnap(convs []dashConversation, conv string) bool {
	for _, c := range convs {
		if c.ConvID == conv {
			return true
		}
	}
	return false
}

func retiredInSnap(retired []dashRetired, conv string) bool {
	for _, r := range retired {
		if r.ConvID == conv {
			return true
		}
	}
	return false
}

// postAgentVerb fires POST /api/agents/{conv}/{verb} at the dashboard
// mux — the per-row promote / retire / reinstate buttons.
func postAgentVerb(t *testing.T, mux http.Handler, conv, verb string) *httpResult {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/"+verb, nil)
	rec := testharness.Serve(mux, r)
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

type httpResult struct {
	Code int
	Body string
}

// Scenario: a conversation that was never enrolled is NOT an agent —
// it surfaces in the snapshot's conversations[] list (a promotion
// candidate), never in agents[].
func TestEnrollment_NonEnrolledConvShowsInConversations(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "plan-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "just-a-chat")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, convInSnap(snap.Conversations, conv),
		"non-enrolled conv %s should be a promotion candidate in conversations[]", conv)
	assert.False(t, agentInSnap(snap.Agents, conv),
		"non-enrolled conv %s must NOT be in agents[]", conv)
}

// Scenario: an enrolled agent whose tmux pane has closed still shows
// on the roster. Agent-ness is durable now — it does not blink out
// when the agent goes offline.
func TestEnrollment_OfflineEnrolledAgentStaysOnRoster(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "offl-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "offline-agent")
	f.HaveEnrolledAgent(conv) // enrolled, but no live session

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, agentInSnap(snap.Agents, conv),
		"offline enrolled agent %s should still be in agents[]", conv)
	assert.False(t, convInSnap(snap.Conversations, conv),
		"an enrolled agent must NOT also show as a plain conversation")
}

// Scenario: promote moves a conversation onto the roster.
func TestEnrollment_PromoteMovesConvToAgents(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "prom-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "promote-me")

	pre := fetchDashSnapshot(t, mux)
	require.True(t, convInSnap(pre.Conversations, conv), "pre: conv in conversations[]")
	require.False(t, agentInSnap(pre.Agents, conv), "pre: conv not in agents[]")

	res := postAgentVerb(t, mux, conv, "promote")
	require.Equal(t, http.StatusOK, res.Code, "promote: %s", res.Body)

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv), "post: promoted conv should be in agents[]")
	assert.False(t, convInSnap(post.Conversations, conv), "post: promoted conv should leave conversations[]")
}

// Scenario: retire demotes an agent — it leaves the roster, lands in
// retired[], and its group memberships + permission grants are
// revoked. The conversation data itself is untouched.
func TestEnrollment_RetireDemotesAndRevokes(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "retr-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "doomed-agent")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", conv, "doomed") // HaveMember enrolls it
	require.NoError(t, db.GrantAgentPermission(conv, "self.rename", "human"))

	pre := fetchDashSnapshot(t, mux)
	require.True(t, agentInSnap(pre.Agents, conv), "pre: enrolled agent in agents[]")

	res := postAgentVerb(t, mux, conv, "retire")
	require.Equal(t, http.StatusOK, res.Code, "retire: %s", res.Body)

	post := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(post.Agents, conv), "retired agent must leave agents[]")
	assert.True(t, retiredInSnap(post.Retired, conv), "retired agent must appear in retired[]")
	assert.False(t, flowGroupHasMember(f, "alpha", conv), "retire must drop group membership")

	hasPerm, err := db.HasAgentPermissionRow(conv, "self.rename")
	require.NoError(t, err)
	assert.False(t, hasPerm, "retire must revoke permission grants")

	// The conversation data itself survives — conv_index row intact.
	row, err := db.GetConvIndex(conv)
	require.NoError(t, err)
	assert.NotNil(t, row, "retire must NOT touch the conversation's conv_index row")
}

// Scenario: reinstate returns a retired agent to active status.
func TestEnrollment_ReinstateRestoresAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "rein-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "comeback-agent")
	f.HaveRetiredAgent(conv)

	pre := fetchDashSnapshot(t, mux)
	require.True(t, retiredInSnap(pre.Retired, conv), "pre: agent in retired[]")
	require.False(t, agentInSnap(pre.Agents, conv), "pre: agent not in agents[]")

	res := postAgentVerb(t, mux, conv, "reinstate")
	require.Equal(t, http.StatusOK, res.Code, "reinstate: %s", res.Body)

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv), "reinstated agent should be back in agents[]")
	assert.False(t, retiredInSnap(post.Retired, conv), "reinstated agent should leave retired[]")
}

// Scenario: the cleanup modal's retire tier — bulk-retire confirmed
// offline agents in one POST.
func TestEnrollment_CleanupRetireTier(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "clnr-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "offline-worker")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", conv, "worker") // enrolled, offline (no session)

	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"retire"}`)
	assert.Equal(t, 1, resp.Retired, "one agent retired; body outcomes=%+v", resp.Outcomes)
	assert.Equal(t, 0, resp.Skipped, "offline agent must not be skipped")

	post := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(post.Agents, conv), "retired agent leaves agents[]")
	assert.True(t, retiredInSnap(post.Retired, conv), "retired agent appears in retired[]")
}

// Scenario: the cleanup retire tier re-checks tmux liveness at execute
// time — an agent that is actually online is skipped, never retired,
// even if the browser sent it in the list.
func TestEnrollment_CleanupRetireSkipsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "live-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "live-worker")
	f.HaveAliveSession(conv, "spwn-live", "tmux-live", "/tmp/live")
	f.HaveEnrolledAgent(conv)

	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"retire"}`)
	assert.Equal(t, 0, resp.Retired, "an online agent must not be retired")
	assert.Equal(t, 1, resp.Skipped, "an online agent must be skipped")

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv), "online agent stays on the roster")
}

// Scenario: reincarnation preserves agentic status — the successor is
// enrolled as an active agent, and the superseded predecessor is
// un-enrolled so it doesn't linger on the roster as an offline ghost.
func TestEnrollment_ReincarnatePreservesAgentStatus(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rcrn-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveAliveSession(conv, "spwn-rcrn", "tmux-rcrn", "/tmp/rcrn")
	f.HaveEnrolledAgent(conv)

	r := f.Reincarnate(conv, "carry on")
	require.NotEmpty(t, r.NewConv, "reincarnate should return a new conv-id")

	newState, err := db.EnrollmentState(r.NewConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, newState,
		"the reincarnated successor %s must be an active agent", r.NewConv)

	oldState, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentNone, oldState,
		"the superseded predecessor %s must be un-enrolled (its identity moved to the successor)", conv)
}

// Scenario: cloning preserves agentic status — the clone is enrolled
// as an active agent in its own right, and the original keeps running
// as an agent too.
func TestEnrollment_ClonePreservesAgentStatus(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "clon-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveAliveSession(conv, "spwn-clon", "tmux-clon", "/tmp/clon")
	f.HaveEnrolledAgent(conv)

	c := f.CloneFresh(conv, "worker-2")
	require.NotEmpty(t, c.NewConv, "clone should return a new conv-id")

	cloneState, err := db.EnrollmentState(c.NewConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, cloneState,
		"the clone %s must be an active agent", c.NewConv)

	origState, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, origState,
		"the original %s stays an active agent after cloning", conv)
}

// Scenario (issue 1): promoting an OFFLINE conversation must land it
// in the virtual "Ungrouped" group, not just the agents roster. The
// snapshot's ungrouped[] array is no longer online-gated — a freshly
// promoted offline conv has no live tmux session, but it is a real
// agent in no group, so the Groups tab must surface it as a drag
// source.
func TestEnrollment_PromoteOfflineConvSurfacesInUngrouped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	// No HaveAliveSession → the conversation is offline.
	const conv = "poff-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "promote-me-offline")

	pre := fetchDashSnapshot(t, mux)
	require.True(t, convInSnap(pre.Conversations, conv), "pre: conv is a promotion candidate")
	require.False(t, ungroupedHas(pre, conv), "pre: not an agent, not ungrouped yet")

	res := postAgentVerb(t, mux, conv, "promote")
	require.Equal(t, http.StatusOK, res.Code, "promote: %s", res.Body)

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv), "post: promoted conv on the roster")
	assert.True(t, ungroupedHas(post, conv),
		"post: a promoted offline conv in no group must surface in ungrouped[] "+
			"(the virtual Ungrouped group); got %d ungrouped rows", len(post.Ungrouped))
}

// Scenario (issue 3): a reincarnation predecessor must never show up
// as its own agent. The v29→v30 enrollment backfill used to enrol
// every old_conv_id in agent_conv_succession, leaving ghost agents on
// the roster that could not be retired — the enrollment verbs redirect
// forward through the succession chain to the (already retired) head,
// so every retire returned a 409. handleDashboardSnapshot now skips
// superseded predecessors; only the live chain head is the agent.
func TestEnrollment_SupersededPredecessorIsNotAnAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const pred = "pred-1111-2222-3333-4444"
	const head = "head-1111-2222-3333-4444"
	f.HaveConvWithTitle(pred, "old-incarnation")
	f.HaveConvWithTitle(head, "live-incarnation")
	// pred reincarnated into head. RecordConvSuccession enrols head.
	require.NoError(t, db.RecordConvSuccession(pred, head, "reincarnate"))
	// Simulate the buggy backfill: pred got an enrollment row too, even
	// though its identity has moved to head.
	require.NoError(t, db.EnrollAgent(pred, "migration"))

	snap := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(snap.Agents, pred),
		"a superseded reincarnation predecessor must NOT be on the agent roster")
	assert.False(t, ungroupedHas(snap, pred),
		"a superseded predecessor must NOT show in the virtual Ungrouped group")
	assert.True(t, agentInSnap(snap.Agents, head),
		"the live chain head IS this agent and must be on the roster")
}

// Scenario: adding a non-agent conversation to a group promotes it —
// the daemon enrolls it on the membership write, so it shows up as an
// agent without a separate promote step.
func TestEnrollment_AddToGroupPromotes(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "join-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "fresh-conv")
	f.HaveGroup("alpha")

	pre := fetchDashSnapshot(t, mux)
	require.False(t, agentInSnap(pre.Agents, conv), "pre: not yet an agent")
	require.True(t, convInSnap(pre.Conversations, conv), "pre: a plain conversation")

	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/alpha/members",
		map[string]any{"conv": conv})
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "add member: %s", rec.Body.String())

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv),
		"adding a conversation to a group must promote it to an agent")
	state, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, state, "the conv must be an active enrolled agent")
}

// Scenario: the dashboard's "drag a retired agent onto a group" gesture
// — reinstate, then join. runDndReinstate(payload, targetGroup) fires
// POST /api/agents/{conv}/reinstate followed by POST
// /api/groups/{g}/members. Retire stripped the agent's old groups and
// reinstate does not restore them, so the explicit join is what lands
// it in the drop-target group. The agent must come back active AND a
// clean member — not a retired ghost still excluded from the roster.
func TestEnrollment_ReinstateThenJoinGroup(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "rejn-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "comeback-agent")
	f.HaveRetiredAgent(conv)
	f.HaveGroup("beta")

	pre := fetchDashSnapshot(t, mux)
	require.True(t, retiredInSnap(pre.Retired, conv), "pre: agent in retired[]")

	// Step 1: reinstate (clears the retired flag).
	res := postAgentVerb(t, mux, conv, "reinstate")
	require.Equal(t, http.StatusOK, res.Code, "reinstate: %s", res.Body)

	// Step 2: join the drop-target group.
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/beta/members",
		map[string]any{"conv": conv})
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "join group: %s", rec.Body.String())

	post := fetchDashSnapshot(t, mux)
	assert.True(t, agentInSnap(post.Agents, conv),
		"reinstated agent must be back on the roster")
	assert.False(t, retiredInSnap(post.Retired, conv),
		"reinstated agent must leave retired[]")
	assert.True(t, flowGroupHasMember(f, "beta", conv),
		"reinstated agent must be a member of the drop-target group")

	state, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, state,
		"reinstated + joined agent must be active-enrolled, not a retired ghost")
}
