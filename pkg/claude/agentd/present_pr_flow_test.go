package agentd_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPresentPR_RejectsURLOutsideCallingAgentRepository(t *testing.T) {
	f := newFlow(t)
	const worker = "pprr-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprr", "tmux-pprr", f.TestCwd("pprr"))
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	var gotCaller, gotDir, gotURL string
	t.Cleanup(agentd.SetPresentedPRAccessValidatorForTest(func(caller, repoDir, rawURL string) error {
		gotCaller, gotDir, gotURL = caller, repoDir, rawURL
		return errors.New("repository is outside the caller's launch tree")
	}))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs", map[string]any{
			"url": "https://github.com/victim/private/pull/1", "repo_dir": "/victim/private",
		}), worker))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "pr_repo_refused")
	assert.Equal(t, worker, gotCaller)
	assert.Equal(t, "/victim/private", gotDir)
	assert.Equal(t, "https://github.com/victim/private/pull/1", gotURL)
}

func TestPresentPR_RequiresCanonicalGitHubPullURL(t *testing.T) {
	f := newFlow(t)
	const worker = "ppru-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-ppru", "tmux-ppru", f.TestCwd("ppru"))
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	for _, rawURL := range []string{
		"http://github.com/tofutools/tclaude/pull/1",
		"https://example.com/tofutools/tclaude/pull/1",
		"https://github.com/tofutools/tclaude/pull/1/files",
		"https://github.com/tofutools/tclaude/pull/1?x=1",
	} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs", map[string]any{"url": rawURL}), worker))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rawURL+": "+rec.Body.String())
		assert.Contains(t, rec.Body.String(), "invalid_pr_url")
	}
}

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
	f.HaveAliveSession(worker, "lbl-pprs", "tmux-pprs", f.TestCwd("pprs"))
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
	auditRows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "present-pr"})
	require.NoError(t, err)
	require.Len(t, auditRows, 1, "credential-triggering presentation is audited")
	assert.Equal(t, worker, auditRows[0].ActorConv)

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
	t.Cleanup(agentd.SetPresentedPRInfoResolverForTest(
		func(rawURL string) (int, string, string, bool) {
			return 124, rawURL, "open", true
		}))
	const worker = "pprd-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprd", "tmux-pprd", f.TestCwd("pprd"))
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

func TestPresentPR_DashboardRefreshesAndExpiresTerminalState(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetPresentedPRInfoResolverForTest(
		func(rawURL string) (int, string, string, bool) {
			return 126, rawURL, "merged", true
		}))
	const worker = "pprx-aaaa-bbbb-cccc-dddd"
	const prURL = "https://github.com/tofutools/tclaude/pull/126"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprx", "tmux-pprx", f.TestCwd("pprx"))
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": prURL, "summary": "ready", "state": "open"}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "present self: body=%s", rec.Body.String())

	agentID, err := db.AgentIDForConv(worker)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	agePresentedPR(t, agentID, prURL, 5*time.Minute)

	mux := agentd.BuildDashboardHandlerForTest()
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()

	snap := fetchDashSnapshot(t, mux)
	m := findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	require.Len(t, m.PresentedPRs, 1, "freshly terminal PR remains visible for the grace window")
	assert.Equal(t, "merged", m.PresentedPRs[0].State)

	agePresentedPR(t, agentID, prURL, 5*time.Minute)
	snap = fetchDashSnapshot(t, mux)
	m = findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	assert.Empty(t, m.PresentedPRs, "old terminal PRs are omitted from dashboard rows")

	row, err := db.GetAgentPR(agentID, prURL)
	require.NoError(t, err)
	assert.Equal(t, "handled", row.State)
}

func TestPresentPR_RecentlyMergedPollIsGlobalAndRepoDeduped(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const (
		workerA = "pprm-aaaa-bbbb-cccc-dddd"
		workerB = "pprm-eeee-ffff-gggg-hhhh"
		workerC = "pprm-iiii-jjjj-kkkk-llll"
		merged  = "https://github.com/tofutools/tclaude/pull/800"
		other   = "https://github.com/tofutools/other/pull/7"
	)

	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	for _, worker := range []string{workerA, workerB, workerC} {
		f.HaveConvWithTitle(worker, worker)
		f.HaveAliveSession(worker, "lbl-"+worker, "tmux-"+worker, f.TestCwd(worker))
	}
	f.HaveMember("alpha", workerA)
	f.HaveMember("beta", workerB)
	f.HaveMember("beta", workerC)

	agentA, err := db.AgentIDForConv(workerA)
	require.NoError(t, err)
	agentB, err := db.AgentIDForConv(workerB)
	require.NoError(t, err)
	agentC, err := db.AgentIDForConv(workerC)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentA, merged, "same PR, first group", "open")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentB, merged, "same PR, second group", "open")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentC, other, "different repo", "open")
	require.NoError(t, err)

	var calls int
	var gotRepos []string
	var gotResultLimit int
	t.Cleanup(agentd.SetRecentlyMergedPRsResolverForTest(
		func(repos []string, resultLimit int) ([]string, bool) {
			calls++
			gotRepos = append([]string(nil), repos...)
			gotResultLimit = resultLimit
			return []string{merged}, true
		}))

	agentd.PollRecentlyMergedPRsForTest()

	assert.Equal(t, 1, calls, "one bulk GitHub search covers every agent and group")
	assert.Equal(t, []string{"tofutools/other", "tofutools/tclaude"}, gotRepos)
	assert.True(t, slices.IsSorted(gotRepos), "repository arguments are deterministic")
	assert.Equal(t, 20, gotResultLimit, "small sets retain the useful recent-results floor")

	rowA, err := db.GetAgentPR(agentA, merged)
	require.NoError(t, err)
	rowB, err := db.GetAgentPR(agentB, merged)
	require.NoError(t, err)
	rowC, err := db.GetAgentPR(agentC, other)
	require.NoError(t, err)
	assert.Equal(t, "merged", rowA.State)
	assert.Equal(t, "merged", rowB.State, "one result updates every agent referencing that PR")
	assert.Equal(t, "open", rowC.State, "unmatched PRs retain the individual-refresh fallback")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	memberA := findDashMember(snap, "alpha", workerA)
	require.NotNil(t, memberA)
	require.Len(t, memberA.PresentedPRs, 1)
	assert.Equal(t, "merged", memberA.PresentedPRs[0].State)
	memberB := findDashMember(snap, "beta", workerB)
	require.NotNil(t, memberB)
	require.Len(t, memberB.PresentedPRs, 1)
	assert.Equal(t, "merged", memberB.PresentedPRs[0].State)
}

func TestPresentPR_OwnerPresentsWorkerWithoutSlug(t *testing.T) {
	f := newFlow(t)
	const lead = "pprl-aaaa-bbbb-cccc-dddd"
	const worker = "pprw-aaaa-bbbb-cccc-dddd"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprw", "tmux-pprw", f.TestCwd("pprw"))
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

func agePresentedPR(t *testing.T, agentID, prURL string, age time.Duration) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_prs SET updated_at = ? WHERE agent_id = ? AND pr_url = ?`,
		time.Now().Add(-age).UTC().UnixNano(), agentID, prURL)
	require.NoError(t, err)
}
