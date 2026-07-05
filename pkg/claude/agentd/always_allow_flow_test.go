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

// Scenario (JOH-367 headline): an ungranted agent copies to the clipboard
// with --ask-human, and the human clicks "Always allow for this agent".
// The one-off copy goes through AND an allow override is persisted, so the
// NEXT copy — sent with no --ask-human header, so no popup could rescue it
// — passes on the grant alone.
func TestAlwaysAllow_PersistsGrantAndSkipsPopupNextTime(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.StubAlwaysAllowApprovalForTest())

	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "alw0-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	// First copy: no grant, but --ask-human + the stubbed "always" click.
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "first"})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code, "first (always-approved) copy; body=%s", resp.Body.String())
	require.Len(t, rec.texts, 1)

	// The always-allow persisted an allow override on the agent.
	effect, ok, err := db.AgentPermissionOverride(conv, agentd.PermHumanClipboard)
	require.NoError(t, err)
	require.True(t, ok, "always-allow must persist an override")
	assert.Equal(t, "grant", effect)

	// Second copy: NO --ask-human header, so no popup can save it. It passes
	// purely on the persisted grant — proving the popup is skipped now.
	r2 := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "second"})
	r2 = agentd.AsAgentPeer(r2, conv)
	resp2 := testharness.Serve(f.Mux, r2)
	require.Equal(t, http.StatusOK, resp2.Code,
		"second copy must pass on the persisted grant with no popup; body=%s", resp2.Body.String())
	require.Len(t, rec.texts, 2, "the second copy also reached the clipboard")
}

// Scenario: a deny override still beats a popup-persisted allow. Deny is
// authoritative across every gate; "always allow" writes an allow override,
// it does not carve out an exception to deny precedence.
func TestAlwaysAllow_DenyOverrideStillWins(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "alw1-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	// Simulate a prior always-allow grant, then the human sets a deny.
	require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermHumanClipboard, db.PermEffectGrant, "human:popup-always"))
	require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermHumanClipboard, db.PermEffectDeny, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "should be denied"})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, resp.Code,
		"a deny override must beat the always-allow grant; body=%s", resp.Body.String())
	assert.Empty(t, rec.texts, "nothing is copied under a deny override")
}

// Scenario: the popup-persisted grant follows the STABLE AGENT IDENTITY, not
// the conv-id. After a /clear conv rotation (agent A's conv rotates from
// convA to convB), the grant written against convA is honoured for convB —
// because agent_permissions is keyed on agent_id (JOH-26), which is exactly
// what "always allow for THIS agent" should mean.
func TestAlwaysAllow_GrantFollowsAgentIdentityThroughRotation(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const convA = "alw2-aaaa-2222-3333-4444"
	const convB = "alw2-bbbb-2222-3333-4444"
	f.HaveConvWithTitle(convA, "worker")

	// The popup persists the grant exactly like this (granted_by tag and all).
	require.NoError(t, db.SetAgentPermissionOverride(convA, agentd.PermHumanClipboard, db.PermEffectGrant, "human:popup-always"))

	// The agent's conv rotates (a /clear): convA → convB, same actor.
	_, err := db.RotateAgentConv(convA, convB, "clear")
	require.NoError(t, err, "rotate")

	// A copy from the NEW conv, with no --ask-human, passes on the grant that
	// was written against the OLD conv — the grant followed the agent.
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "post-rotation copy"})
	r = agentd.AsAgentPeer(r, convB)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code,
		"the always-allow grant must follow the agent across conv rotation; body=%s", resp.Body.String())
	require.Len(t, rec.texts, 1)
}

// Scenario: the "always" decision is gated SERVER-SIDE on eligibility,
// independent of whether the button was rendered. A forged decision POST for
// an ineligible slug is refused (403); an eligible one is accepted. Closes the
// "scraper skips the UI" hole. Now driven through the dashboard access-request
// endpoint that replaced the loopback /approve page.
func TestAlwaysAllow_DecisionGatesOnEligibility(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t) // DB setup (peerAgentID lookups on the snapshot side)
	h := agentd.BuildDashboardHandlerForTest()

	// Ineligible slug → "always" is refused even for the authed operator.
	const idBad = "alw-bad-0001"
	t.Cleanup(agentd.SeedApprovalForTest(idBad, "agent.delete", false))
	badRec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+idBad+"/decision", map[string]any{"decision": "always"}))
	assert.Equal(t, http.StatusForbidden, badRec.Code,
		"an ineligible slug must reject always; body=%s", badRec.Body.String())

	// Eligible slug → "always" is accepted.
	const idOK = "alw-ok-0002"
	t.Cleanup(agentd.SeedApprovalForTest(idOK, agentd.PermHumanClipboard, true))
	okRec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+idOK+"/decision", map[string]any{"decision": "always"}))
	require.Equal(t, http.StatusOK, okRec.Code,
		"an eligible slug must accept always; body=%s", okRec.Body.String())
}

// Scenario: the FULL live path — a real "always" decision POST against the
// dashboard access-request endpoint, consumed by a real waiter, persists the
// override. Wires the HTTP decision and the waiter's persist end-to-end, so a
// regression where the handler sent the wrong outcome onto the channel would
// be caught.
func TestAlwaysAllow_LiveDecisionThroughWaiterPersists(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t) // DB setup

	const conv = "alw3-1111-2222-3333-4444"
	const id = "alw-live-0001"

	// A real waiter consumes the decision the handler sends, exactly as
	// production does (applyApprovalOutcome → audit + persist).
	done, cleanup := agentd.SeedApprovalWithWaiterForTest(id, agentd.PermHumanClipboard, conv, true)
	t.Cleanup(cleanup)

	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "always"}))
	require.Equal(t, http.StatusOK, rec.Code, "live always decision; body=%s", rec.Body.String())

	// The waiter consumed the real outcome and ran the persist end-to-end.
	require.True(t, <-done, "always-allow must approve the pending request")

	effect, ok, err := db.AgentPermissionOverride(conv, agentd.PermHumanClipboard)
	require.NoError(t, err)
	require.True(t, ok, "the real decision must persist the override through the waiter")
	assert.Equal(t, "grant", effect)
}
