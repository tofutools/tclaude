package agentd_test

import (
	"net/http"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPresentPR_RejectsURLOutsideCallingAgentRepository(t *testing.T) {
	f := newFlow(t)
	const worker = "pprr-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	repo := presentedPRTestRepo(t, f, "pprr", "git@github.com:tofutools/tclaude.git", "github.com")
	f.HaveAliveSession(worker, "lbl-pprr", "tmux-pprr", repo)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs", map[string]any{
			"url": "https://github.com/victim/private/pull/1", "repo_dir": "/victim/private",
		}), worker))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "pr_repo_refused")
	assert.Contains(t, rec.Body.String(), "launch directory or a subdirectory")
}

func TestPresentPR_RequiresCanonicalGitHubPullURL(t *testing.T) {
	f := newFlow(t)
	const worker = "ppru-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-ppru", "tmux-ppru", f.TestCwd("ppru"))
	savePresentedPRPolicy(t, "github.com/tofutools")
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

func TestPresentPR_GitProxyDisabledAcceptsLegacyHTTPURL(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const (
		worker = "pprc-aaaa-bbbb-cccc-dddd"
		prURL  = "https://gitlab.example.com/acme/app/-/merge_requests/42"
	)
	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprc", "tmux-pprc", f.TestCwd("not-a-repo"))
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": prURL, "summary": "legacy compatible"}), worker))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	member := findDashMember(snap, "alpha", worker)
	require.NotNil(t, member)
	require.Len(t, member.PresentedPRs, 1)
	assert.Equal(t, prURL, member.PresentedPRs[0].URL)
}

func TestPresentPR_ConfigFailureDoesNotEnableLegacyMode(t *testing.T) {
	f := newFlow(t)
	const worker = "pprf-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprf", "tmux-pprf", f.TestCwd("not-a-repo"))
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))
	savePresentedPRPolicy(t, "github.com/tofutools")
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{not-json"), 0o600))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": "https://example.com/arbitrary/review/1"}), worker))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "could not determine Git proxy mode")
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
	repo := presentedPRTestRepo(t, f, "pprs", "git@github.com:tofutools/tclaude.git", "github.com/tofutools")
	f.HaveAliveSession(worker, "lbl-pprs", "tmux-pprs", repo)
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
	repo := presentedPRTestRepo(t, f, "pprd", "git@github.com:tofutools/tclaude.git", "github.com/tofutools")
	f.HaveAliveSession(worker, "lbl-pprd", "tmux-pprd", repo)
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
	repo := presentedPRTestRepo(t, f, "pprx", "git@github.com:tofutools/tclaude.git", "github.com/tofutools")
	f.HaveAliveSession(worker, "lbl-pprx", "tmux-pprx", repo)
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
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com/tofutools"},
	}}}))
	f.HaveMember("alpha", workerA)
	f.HaveMember("beta", workerB)
	f.HaveMember("beta", workerC)

	agentA, err := db.AgentIDForConv(workerA)
	require.NoError(t, err)
	agentB, err := db.AgentIDForConv(workerB)
	require.NoError(t, err)
	agentC, err := db.AgentIDForConv(workerC)
	require.NoError(t, err)
	_, err = db.UpsertValidatedAgentPR(agentA, merged, "same PR, first group", "open", "/repo/a")
	require.NoError(t, err)
	_, err = db.UpsertValidatedAgentPR(agentB, merged, "same PR, second group", "open", "/repo/b")
	require.NoError(t, err)
	_, err = db.UpsertValidatedAgentPR(agentC, other, "different repo", "open", "/repo/c")
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

func TestPresentPR_RecentlyMergedPollQuarantinesLegacyRowsInsideBroadPolicy(t *testing.T) {
	f := newFlow(t)
	const worker = "pprg-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-pprg", "tmux-pprg", f.TestCwd("pprg"))
	_, _, err := db.EnsureAgentForConv(worker, "test")
	require.NoError(t, err)
	agentID, err := db.AgentIDForConv(worker)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentID, "https://github.com/victim/private/pull/1", "legacy row", "open")
	require.NoError(t, err)

	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com"},
	}}}))
	called := false
	t.Cleanup(agentd.SetRecentlyMergedPRsResolverForTest(func([]string, int) ([]string, bool) {
		called = true
		return nil, true
	}))
	agentd.PollRecentlyMergedPRsForTest()
	assert.False(t, called, "an untrusted legacy row must not trigger an authenticated search")
}

func TestPresentPR_OwnerPresentsWorkerWithoutSlug(t *testing.T) {
	f := newFlow(t)
	const lead = "pprl-aaaa-bbbb-cccc-dddd"
	const worker = "pprw-aaaa-bbbb-cccc-dddd"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	repo := presentedPRTestRepo(t, f, "pprw", "git@github.com:tofutools/tclaude.git", "github.com/tofutools")
	f.HaveAliveSession(worker, "lbl-pprw", "tmux-pprw", repo)
	f.HaveConvWithTitle(lead, "lead")
	f.HaveAliveSession(lead, "lbl-pprl", "tmux-pprl", repo)
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

func presentedPRTestRepo(t *testing.T, f *testharness.Flow, name, remote string, allowed ...string) string {
	t.Helper()
	repo := f.TestCwd(name)
	require.NoError(t, os.MkdirAll(repo, 0o755))
	for _, args := range [][]string{{"init"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		require.NoError(t, cmd.Run())
	}
	savePresentedPRPolicy(t, allowed...)
	return repo
}

func savePresentedPRPolicy(t *testing.T, allowed ...string) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: allowed,
	}}}))
}

func agePresentedPR(t *testing.T, agentID, prURL string, age time.Duration) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_prs SET updated_at = ? WHERE agent_id = ? AND pr_url = ?`,
		time.Now().Add(-age).UTC().UnixNano(), agentID, prURL)
	require.NoError(t, err)
}
