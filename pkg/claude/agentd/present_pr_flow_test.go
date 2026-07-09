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

type presentPRResp struct {
	ConvID        string `json:"conv_id"`
	Handled       bool   `json:"handled"`
	CallerConv    string `json:"caller_conv"`
	CallerAgentID string `json:"caller_agent_id"`
	PR            struct {
		URL     string `json:"url"`
		Number  int    `json:"number"`
		Summary string `json:"summary"`
		State   string `json:"state"`
	} `json:"pr"`
}

func TestPresentPR_SelfPresentsAndDashboardRenders(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const worker = "pprs-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprs", "tmux-pprs", "/tmp/pprs")
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{
				"url":     "https://github.com/tofutools/tclaude/pull/123",
				"summary": "ready",
				"state":   "open",
			}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "present self: body=%s", rec.Body.String())
	var resp presentPRResp
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 123, resp.PR.Number)
	assert.Equal(t, "ready", resp.PR.Summary)
	assert.Empty(t, resp.CallerConv, "self write carries no caller_conv")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m := findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	require.Len(t, m.PresentedPRs, 1)
	assert.Equal(t, "https://github.com/tofutools/tclaude/pull/123", m.PresentedPRs[0].URL)
	assert.Equal(t, 123, m.PresentedPRs[0].Number)
	assert.Equal(t, "open", m.PresentedPRs[0].State)
}

func TestPresentPR_DedupesByURLAndCanMarkHandled(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const worker = "pprd-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprd", "tmux-pprd", "/tmp/pprd")
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	for _, summary := range []string{"first", "updated"} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
				map[string]any{"url": "https://github.com/tofutools/tclaude/pull/124", "summary": summary}), worker))
		require.Equalf(t, http.StatusOK, rec.Code, "present %q: body=%s", summary, rec.Body.String())
	}
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m := findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	require.Len(t, m.PresentedPRs, 1, "duplicate URL upserts one row")
	assert.Equal(t, "updated", m.PresentedPRs[0].Summary)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": "https://github.com/tofutools/tclaude/pull/124", "handled": true}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "handle: body=%s", rec.Body.String())
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m = findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	assert.Empty(t, m.PresentedPRs, "handled PRs are hidden from dashboard")
}

func TestPresentPR_OwnerPresentsWorkerWithoutSlug(t *testing.T) {
	f := newFlow(t)
	const lead = "pprl-aaaa-bbbb-cccc-dddd"
	const worker = "pprw-aaaa-bbbb-cccc-dddd"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprw", "tmux-pprw", "/tmp/pprw")
	f.HaveMember("squad", worker)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+worker+"/prs",
			map[string]any{"url": "https://github.com/tofutools/tclaude/pull/125"}), lead))
	require.Equalf(t, http.StatusOK, rec.Code, "owner present: body=%s", rec.Body.String())
	var resp presentPRResp
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, lead, resp.CallerConv)
	assert.Equal(t, 125, resp.PR.Number)
}
