package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type seancePlanView struct {
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
	Cwd         string `json:"cwd"`
	Hops        int    `json:"hops"`
	Requested   int    `json:"requested_back"`
	Exact       bool   `json:"exact"`
}

func requestSeancePlan(
	t *testing.T,
	f *testharness.Flow,
	caller string,
	body map[string]any,
) (*seancePlanView, int, string) {
	t.Helper()
	req := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/whoami/seance", body), caller)
	rec := testharness.Serve(f.Mux, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var out seancePlanView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out),
		"decode séance plan: %s", rec.Body.String())
	return &out, rec.Code, rec.Body.String()
}

func requestSeanceRun(
	t *testing.T,
	f *testharness.Flow,
	caller string,
	body map[string]any,
) (*seanceRunView, int, string) {
	t.Helper()
	req := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/whoami/seance/run", body), caller)
	rec := testharness.Serve(f.Mux, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var out seanceRunView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out),
		"decode séance result: %s", rec.Body.String())
	return &out, rec.Code, rec.Body.String()
}

type seanceRunView struct {
	Answer      string `json:"answer"`
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
}

func haveSeanceSession(f *testharness.Flow, convID, label, tmuxSession, cwd string) {
	f.HaveAliveSession(convID, label, tmuxSession, cwd)
	setSeanceSessionSnapshot(f.T, convID, sandboxpolicy.EmptySnapshot())
}

func haveSeanceCodexSession(f *testharness.Flow, convID, label, tmuxSession, cwd string) {
	f.HaveAliveCodexSession(convID, label, tmuxSession, cwd)
	setSeanceSessionSnapshot(f.T, convID, sandboxpolicy.EmptySnapshot())
}

func setSeanceSessionSnapshot(t *testing.T, convID string, snapshot sandboxpolicy.Snapshot) {
	t.Helper()
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		row.EffectiveSandbox = &snapshot
		require.NoError(t, db.SaveSession(row))
	}
}

// Production-path contract: the daemon resolves the caller's stable actor,
// walks the real succession table backward, and returns the dead generation's
// harness + cwd. The caller never needs direct access to ~/.tclaude/data.
func TestSeancePlan_DefaultAndSelectorsResolveThroughDaemon(t *testing.T) {
	f := newFlow(t)

	const (
		oldConv = "aaaabbbb-1111-2222-3333-444444444444"
		newConv = "ccccdddd-1111-2222-3333-444444444444"
	)
	oldCwd := f.TestCwd("seance-old")
	newCwd := f.TestCwd("seance-new")

	f.HaveConvWithTitle(oldConv, "worker-x")
	haveSeanceSession(f, oldConv, "seance-old-label", "seance-old-tmux", oldCwd)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "worker")
	haveSeanceSession(f, newConv, "seance-new-label", "seance-new-tmux", newCwd)
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err, "rotate actor old → new")

	agentID, err := db.AgentIDForConv(newConv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	t.Run("default self predecessor", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{"back": 1})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.Equal(t, "claude", got.Harness)
		assert.Equal(t, oldCwd, got.Cwd)
		assert.Equal(t, 1, got.Hops)
		assert.Equal(t, 1, got.Requested)
		assert.False(t, got.Exact)
	})

	t.Run("stable agent id walks from live head", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": agentID,
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("agent name walks from live head", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": "worker",
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("conversation prefix stays on exact dead generation", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": oldConv[:8],
			"back":   99,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.True(t, got.Exact)
		assert.Zero(t, got.Hops)
	})

	t.Run("human may name an exact dead generation", func(t *testing.T) {
		req := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/whoami/seance", map[string]any{"target": oldConv}))
		rec := testharness.Serve(f.Mux, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got seancePlanView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, oldConv, got.Predecessor)
		assert.True(t, got.Exact)
	})

	t.Run("live exact generation is rejected", func(t *testing.T) {
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": newConv,
		})
		assert.Equal(t, http.StatusConflict, status, "body=%s", body)
		assert.Contains(t, body, "live generation")
	})

	t.Run("another actor is forbidden", func(t *testing.T) {
		const otherConv = "eeeeffff-1111-2222-3333-444444444444"
		f.HaveConvWithTitle(otherConv, "other-worker")
		f.HaveMember("alpha", otherConv)
		otherAgent, err := db.AgentIDForConv(otherConv)
		require.NoError(t, err)

		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": otherAgent,
		})
		assert.Equal(t, http.StatusForbidden, status, "body=%s", body)
		assert.Contains(t, body, "only with one of its own")
	})

	t.Run("removed worktree fails before subprocess planning", func(t *testing.T) {
		require.NoError(t, os.Remove(oldCwd), "remove predecessor startup dir")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{"back": 1})
		assert.Equal(t, http.StatusNotFound, status, "body=%s", body)
		assert.Contains(t, body, "grave is unreachable")
		assert.Contains(t, body, oldCwd)
	})
}

func TestSeancePlan_HumanMustNameTarget(t *testing.T) {
	f := newFlow(t)
	req := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/whoami/seance", map[string]any{"back": 1}))
	rec := testharness.Serve(f.Mux, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "must pass --target")
}

func TestSeancePlan_FirstGenerationGetsClearNotFound(t *testing.T) {
	f := newFlow(t)
	const conv = "11112222-3333-4444-5555-666677778888"
	f.HaveConvWithTitle(conv, "first-life")
	haveSeanceSession(f, conv, "first-life-label", "first-life-tmux", f.TestCwd("first-life"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", conv)

	_, status, body := requestSeancePlan(t, f, conv, map[string]any{"back": 1})
	assert.Equal(t, http.StatusNotFound, status, "body=%s", body)
	assert.Contains(t, body, "no predecessor")
}

func TestSeancePlan_PrunedExactGenerationDoesNotRedirect(t *testing.T) {
	f := newFlow(t)
	const (
		first  = "deadbeef-1111-2222-3333-444444444444"
		middle = "feedface-1111-2222-3333-444444444444"
		head   = "cafebabe-1111-2222-3333-444444444444"
	)
	firstCwd := f.TestCwd("seance-pruned-first")
	f.HaveConvWithTitle(first, "first")
	haveSeanceSession(f, first, "first-label", "first-tmux", firstCwd)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", first)
	f.HaveConvWithTitle(middle, "middle")
	haveSeanceSession(f, middle, "middle-label", "middle-tmux", f.TestCwd("seance-pruned-middle"))
	_, err := db.RotateAgentConv(first, middle, "reincarnate")
	require.NoError(t, err)
	f.HaveConvWithTitle(head, "head")
	haveSeanceSession(f, head, "head-label", "head-tmux", f.TestCwd("seance-pruned-head"))
	_, err = db.RotateAgentConv(middle, head, "reincarnate")
	require.NoError(t, err)
	require.NoError(t, db.DeleteConvIndex(first), "prune first generation from cache")

	for _, selector := range []string{first, first[:8]} {
		got, status, body := requestSeancePlan(t, f, head, map[string]any{
			"target": selector,
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "selector=%q body=%s", selector, body)
		assert.Equal(t, first, got.Predecessor,
			"exact pruned generation must not redirect to head then walk back to middle")
		assert.True(t, got.Exact)
	}
}

func TestSeancePlan_OpenCodeExactGenerationDoesNotRedirect(t *testing.T) {
	f := newFlow(t)
	const (
		first  = "ses_alpha111111111111111111111111"
		middle = "ses_bravo222222222222222222222222"
		head   = "ses_charlie3333333333333333333333"
	)
	firstCwd := f.TestCwd("seance-opencode-first")
	for _, generation := range []struct {
		id, title, cwd string
	}{
		{id: first, title: "open-first", cwd: firstCwd},
		{id: middle, title: "open-middle", cwd: f.TestCwd("seance-opencode-middle")},
		{id: head, title: "open-head", cwd: f.TestCwd("seance-opencode-head")},
	} {
		f.HaveConvWithTitle(generation.id, generation.title)
		haveSeanceSession(f, generation.id, generation.title+"-label", generation.title+"-tmux", generation.cwd)
		row, err := db.GetConvIndex(generation.id)
		require.NoError(t, err)
		require.NotNil(t, row)
		row.Harness = harness.OpenCodeName
		require.NoError(t, db.UpsertConvIndex(row))
		setSessionHarness(t, generation.id, harness.OpenCodeName)
		sessions, err := db.FindSessionsByConvID(generation.id)
		require.NoError(t, err)
		require.NotEmpty(t, sessions)
		for _, session := range sessions {
			session.SandboxMode = harness.OpenCodeSandboxAccessControl
			session.ApprovalPolicy = harness.OpenCodeApprovalDeny
			require.NoError(t, db.SaveSession(session))
		}
	}
	f.HaveGroup("alpha")
	f.HaveMember("alpha", first)
	_, err := db.RotateAgentConv(first, middle, "reincarnate")
	require.NoError(t, err)
	_, err = db.RotateAgentConv(middle, head, "reincarnate")
	require.NoError(t, err)
	require.NoError(t, db.DeleteConvIndex(first), "prune first OpenCode generation from cache")

	for _, selector := range []string{first, first[:8]} {
		got, status, body := requestSeancePlan(t, f, head, map[string]any{"target": selector})
		require.Equal(t, http.StatusOK, status, "selector=%q body=%s", selector, body)
		assert.Equal(t, first, got.Predecessor)
		assert.Equal(t, harness.OpenCodeName, got.Harness)
		assert.Equal(t, firstCwd, got.Cwd)
		assert.True(t, got.Exact)
	}

	previous := agentd.RunSeanceHarness
	called := false
	agentd.RunSeanceHarness = func(_ context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
		called = true
		return agentd.SeanceExecResult{Stdout: "unsafe", Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })
	_, status, body := requestSeanceRun(t, f, head, map[string]any{
		"target": first, "question": "what did you know?",
	})
	assert.Equal(t, http.StatusConflict, status, "body=%s", body)
	assert.Contains(t, body, "managed-server permission posture")
	assert.False(t, called, "OpenCode must fail before the daemon subprocess boundary")
}

func TestSeancePlan_RejectsUnboundedAndShortSelectors(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "abcd1234-1111-2222-3333-444444444444"
		newConv = "9876fedc-1111-2222-3333-444444444444"
	)
	f.HaveConvWithTitle(oldConv, "old")
	haveSeanceSession(f, oldConv, "old-label", "old-tmux", f.TestCwd("bounded-old"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "new")
	haveSeanceSession(f, newConv, "new-label", "new-tmux", f.TestCwd("bounded-new"))
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{name: "short conversation prefix", body: map[string]any{"target": oldConv[:4]}, want: "at least 8"},
		{name: "target length", body: map[string]any{"target": strings.Repeat("x", 257)}, want: "too long"},
		{name: "back bound", body: map[string]any{"back": 129}, want: "between 1 and 128"},
		{name: "body bound", body: map[string]any{"target": strings.Repeat("x", 5<<10)}, want: "invalid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, status, body := requestSeancePlan(t, f, newConv, tc.body)
			assert.Equal(t, http.StatusBadRequest, status, "body=%s", body)
			assert.Contains(t, body, tc.want)
		})
	}

	t.Run("ambiguous exact prefix", func(t *testing.T) {
		const samePrefix = "abcd1234-aaaa-bbbb-cccc-dddddddddddd"
		f.HaveConvWithTitle(samePrefix, "same-prefix")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": oldConv[:8],
		})
		assert.Equal(t, http.StatusConflict, status, "body=%s", body)
		assert.Contains(t, body, "multiple generations")
	})

	t.Run("short agent name is not mistaken for a conversation prefix", func(t *testing.T) {
		f.HaveConvWithTitle(newConv, "ace")
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": "ace",
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("short OpenCode conversation prefix is rejected", func(t *testing.T) {
		const openCodeConv = "ses_alpha111111111111111111111111"
		f.HaveConvWithTitle(openCodeConv, "open-short-prefix")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": openCodeConv[:5],
		})
		assert.Equal(t, http.StatusBadRequest, status, "body=%s", body)
		assert.Contains(t, body, "at least 8")
	})
}

func TestSeanceRun_ExecutesThroughDaemonBoundary(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "a11ce000-1111-2222-3333-444444444444"
		newConv = "b0b00000-1111-2222-3333-444444444444"
	)
	oldCwd := f.TestCwd("seance-run-old")
	f.HaveConvWithTitle(oldConv, "old-runner")
	haveSeanceSession(f, oldConv, "old-runner-label", "old-runner-tmux", oldCwd)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "new-runner")
	haveSeanceSession(f, newConv, "new-runner-label", "new-runner-tmux", f.TestCwd("seance-run-new"))
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	previous := agentd.RunSeanceHarness
	var captured agentd.SeanceExecPlan
	agentd.RunSeanceHarness = func(_ context.Context, plan agentd.SeanceExecPlan) agentd.SeanceExecResult {
		captured = plan
		return agentd.SeanceExecResult{
			Stdout:  "The token bug was in the resume path.\n",
			Started: true,
		}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })

	got, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target":     oldConv,
		"question":   "What did you learn?",
		"timeout_ms": int64((30 * time.Second).Milliseconds()),
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	assert.Equal(t, "The token bug was in the resume path.\n", got.Answer)
	assert.Equal(t, oldConv, got.Predecessor)
	assert.Equal(t, harness.DefaultName, got.Harness)
	assert.Equal(t, oldCwd, captured.Cwd)
	command := strings.Join(captured.Argv, " ")
	assert.Contains(t, command, "--resume "+oldConv)
	assert.Contains(t, command, "-p")
	assert.Contains(t, command, "What did you learn?")
	assert.Contains(t, command, "--no-session-persistence")
	assert.Contains(t, command, "--permission-mode bypassPermissions")
	assert.NotContains(t, command, "--safe-mode")

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "seance"})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	audit := rows[0]
	assert.Equal(t, db.AuditActorAgent, audit.ActorKind)
	assert.Equal(t, newConv, audit.ActorConv)
	assert.Equal(t, oldConv, audit.TargetConv)
	assert.Equal(t, oldConv[:8], audit.TargetLabel)
	assert.Contains(t, audit.Detail, "harness claude")
	assert.NotContains(t, audit.Detail, "What did you learn?")
	assert.NotContains(t, audit.Detail, "token bug")
}

func TestSeanceRun_ReplaysExactPredecessorCodexSandbox(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "c0de0000-1111-2222-3333-444444444444"
		newConv = "c0de0000-5555-6666-7777-888888888888"
	)
	oldCwd := f.TestCwd("seance-codex-old")
	newCwd := f.TestCwd("seance-codex-new")
	haveSeanceCodexSession(f, oldConv, "codex-old", "tmux-codex-old", oldCwd)
	predecessorSnapshot := sandboxpolicy.EmptySnapshot()
	predecessorSnapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "POLICY_OWNER", Value: "predecessor",
	}}
	oldRows, err := db.FindSessionsByConvID(oldConv)
	require.NoError(t, err)
	require.NotEmpty(t, oldRows)
	for _, row := range oldRows {
		row.SandboxMode = harness.SandboxManagedProfile
		row.ApprovalPolicy = harness.ApprovalNever
		row.EffectiveSandbox = &predecessorSnapshot
		require.NoError(t, db.SaveSession(row))
	}
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	haveSeanceCodexSession(f, newConv, "codex-new", "tmux-codex-new", newCwd)
	successorSnapshot := sandboxpolicy.EmptySnapshot()
	successorSnapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "POLICY_OWNER", Value: "successor",
	}}
	newRows, err := db.FindSessionsByConvID(newConv)
	require.NoError(t, err)
	require.NotEmpty(t, newRows)
	for _, row := range newRows {
		row.SandboxMode = harness.SandboxManagedProfile
		row.ApprovalPolicy = harness.ApprovalNever
		row.EffectiveSandbox = &successorSnapshot
		require.NoError(t, db.SaveSession(row))
	}
	_, err = db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	previousProfile := agentd.EnsureSeanceCodexProfile
	var rendered *sandboxpolicy.Snapshot
	agentd.EnsureSeanceCodexProfile = func(
		cwd, launchID string,
		snapshot *sandboxpolicy.Snapshot,
	) (string, string, *harness.CodexSplitPolicyCapability, error) {
		assert.Equal(t, oldCwd, cwd)
		assert.Len(t, launchID, 16)
		rendered = snapshot
		return "tclaude-agent-0123456789abcdef", t.TempDir() + "/profile",
			&harness.CodexSplitPolicyCapability{ExecutablePath: "/verified/codex"}, nil
	}
	t.Cleanup(func() { agentd.EnsureSeanceCodexProfile = previousProfile })
	previousRevalidate := agentd.RevalidateSeanceCodexCapability
	agentd.RevalidateSeanceCodexCapability = func(capability harness.CodexSplitPolicyCapability) error {
		assert.Equal(t, "/verified/codex", capability.ExecutablePath)
		return nil
	}
	t.Cleanup(func() { agentd.RevalidateSeanceCodexCapability = previousRevalidate })

	previousRun := agentd.RunSeanceHarness
	var captured agentd.SeanceExecPlan
	agentd.RunSeanceHarness = func(_ context.Context, plan agentd.SeanceExecPlan) agentd.SeanceExecResult {
		captured = plan
		return agentd.SeanceExecResult{Stdout: "remembered\n", Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previousRun })

	got, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was the policy?",
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	assert.Equal(t, "remembered\n", got.Answer)
	require.NotNil(t, rendered)
	assert.Equal(t, "predecessor", rendered.Effective.Environment[0].Value,
		"the historical generation wins over the successor actor snapshot")
	assert.Equal(t, "predecessor", captured.Environment["POLICY_OWNER"])
	command := strings.Join(captured.Argv, " ")
	assert.Contains(t, command, "-p tclaude-agent-0123456789abcdef")
	assert.Contains(t, command, "--ask-for-approval never")
	assert.Contains(t, command, "exec resume "+oldConv)
	assert.Contains(t, command, "--ephemeral")
	assert.NotContains(t, command, `sandbox_mode="read-only"`)
	assert.Equal(t, "/verified/codex", captured.Argv[0])
}

func TestSeanceRun_CodexManagedSandboxRejectsHarnessHomeAsWorkspace(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "c0de1111-1111-2222-3333-444444444444"
		newConv = "c0de1111-5555-6666-7777-888888888888"
	)
	oldCwd := f.TestCwd("seance-codex-home")
	t.Setenv("HOME", oldCwd)
	haveSeanceCodexSession(f, oldConv, "codex-home-old", "tmux-codex-home-old", oldCwd)
	oldRows, err := db.FindSessionsByConvID(oldConv)
	require.NoError(t, err)
	require.NotEmpty(t, oldRows)
	for _, row := range oldRows {
		row.SandboxMode = harness.SandboxManagedProfile
		row.ApprovalPolicy = harness.ApprovalNever
		require.NoError(t, db.SaveSession(row))
	}
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	haveSeanceCodexSession(f, newConv, "codex-home-new", "tmux-codex-home-new",
		f.TestCwd("seance-codex-home-successor"))
	_, err = db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	previousProfile := agentd.EnsureSeanceCodexProfile
	profileCalled := false
	agentd.EnsureSeanceCodexProfile = func(
		_, _ string, _ *sandboxpolicy.Snapshot,
	) (string, string, *harness.CodexSplitPolicyCapability, error) {
		profileCalled = true
		return "", "", nil, errors.New("must not be called")
	}
	t.Cleanup(func() { agentd.EnsureSeanceCodexProfile = previousProfile })

	previousRun := agentd.RunSeanceHarness
	runCalled := false
	agentd.RunSeanceHarness = func(_ context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
		runCalled = true
		return agentd.SeanceExecResult{Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previousRun })

	_, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was recorded?",
	})
	assert.Equal(t, http.StatusConflict, status, "body=%s", body)
	assert.Contains(t, body, "sandbox_cwd_conflict")
	assert.Contains(t, body, "private harness state writable")
	assert.False(t, profileCalled)
	assert.False(t, runCalled)
}

func TestSeancePlan_MissingPredecessorSnapshotDoesNotBorrowSuccessorAuthority(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "5eed0000-1111-2222-3333-444444444444"
		newConv = "5eed0000-5555-6666-7777-888888888888"
	)
	haveSeanceSession(f, oldConv, "snapshot-old", "tmux-snapshot-old",
		f.TestCwd("seance-snapshot-old"))
	oldRows, err := db.FindSessionsByConvID(oldConv)
	require.NoError(t, err)
	require.NotEmpty(t, oldRows)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	haveSeanceSession(f, newConv, "snapshot-new", "tmux-snapshot-new",
		f.TestCwd("seance-snapshot-new"))
	_, err = db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)
	for _, row := range oldRows {
		require.NoError(t, db.DeleteSession(row.ID))
	}

	agentID, err := db.AgentIDForConv(newConv)
	require.NoError(t, err)
	successor := sandboxpolicy.EmptySnapshot()
	successor.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "SUCCESSOR_ONLY", Value: "must-not-be-borrowed",
	}}
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &successor))

	_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
		"target": oldConv,
	})
	assert.Equal(t, http.StatusConflict, status, "body=%s", body)
	assert.Contains(t, body, "resume_profile")
	assert.Contains(t, body, "historical sandbox snapshot is unavailable")
	assert.Contains(t, body, "refusing to substitute the successor")
}

func TestSeanceRun_BoundsAndFailureModes(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "cafe0000-1111-2222-3333-444444444444"
		newConv = "face0000-1111-2222-3333-444444444444"
	)
	f.HaveConvWithTitle(oldConv, "old-bounds")
	haveSeanceSession(f, oldConv, "old-bounds-label", "old-bounds-tmux", f.TestCwd("seance-bounds-old"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "new-bounds")
	haveSeanceSession(f, newConv, "new-bounds-label", "new-bounds-tmux", f.TestCwd("seance-bounds-new"))
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	previous := agentd.RunSeanceHarness
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })
	runCalls := 0
	agentd.RunSeanceHarness = func(_ context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
		runCalls++
		return agentd.SeanceExecResult{Stdout: "ok", Started: true}
	}
	base := func() map[string]any {
		return map[string]any{"target": oldConv, "question": "question"}
	}

	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "empty question", edit: func(b map[string]any) { b["question"] = " " }, want: "question is required"},
		{name: "large question", edit: func(b map[string]any) { b["question"] = strings.Repeat("q", 32<<10+1) }, want: "question is too long"},
		{name: "large request", edit: func(b map[string]any) { b["padding"] = strings.Repeat("x", 65<<10) }, want: "invalid JSON"},
		{name: "timeout cap", edit: func(b map[string]any) { b["timeout_ms"] = int64((10*time.Minute + time.Millisecond).Milliseconds()) }, want: "no more than"},
		{name: "timeout overflow", edit: func(b map[string]any) { b["timeout_ms"] = int64(^uint64(0) >> 1) }, want: "no more than"},
		{name: "invalid model", edit: func(b map[string]any) { b["model"] = "definitely-not-a-claude-model" }, want: "invalid --model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := base()
			tc.edit(body)
			_, status, response := requestSeanceRun(t, f, newConv, body)
			assert.Equal(t, http.StatusBadRequest, status, "body=%s", response)
			assert.Contains(t, response, tc.want)
		})
	}
	assert.Zero(t, runCalls, "invalid requests must never reach the billable boundary")

	tests := []struct {
		name   string
		result agentd.SeanceExecResult
		status int
		code   string
	}{
		{
			name:   "initialization",
			result: agentd.SeanceExecResult{Err: errors.New("executable not found")},
			status: http.StatusBadGateway,
			code:   "seance_init",
		},
		{
			name: "harness exit",
			result: agentd.SeanceExecResult{
				Started: true,
				Stderr:  "authentication failed",
				Err:     errors.New("exit status 1"),
			},
			status: http.StatusBadGateway,
			code:   "seance_failed",
		},
		{
			name: "answer limit",
			result: agentd.SeanceExecResult{
				Started:         true,
				StdoutTruncated: true,
				Err:             context.Canceled,
			},
			status: http.StatusBadGateway,
			code:   "seance_output_limit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agentd.RunSeanceHarness = func(_ context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
				return tc.result
			}
			_, status, response := requestSeanceRun(t, f, newConv, base())
			assert.Equal(t, tc.status, status, "body=%s", response)
			assert.Contains(t, response, tc.code)
		})
	}

	t.Run("deadline", func(t *testing.T) {
		agentd.RunSeanceHarness = func(ctx context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
			<-ctx.Done()
			return agentd.SeanceExecResult{Started: true, Err: ctx.Err()}
		}
		body := base()
		body["timeout_ms"] = int64(1)
		_, status, response := requestSeanceRun(t, f, newConv, body)
		assert.Equal(t, http.StatusGatewayTimeout, status, "body=%s", response)
		assert.Contains(t, response, "seance_timeout")
	})
}

func TestSeanceRun_ConcurrencyIsBounded(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "1111cafe-1111-2222-3333-444444444444"
		newConv = "2222face-1111-2222-3333-444444444444"
	)
	f.HaveConvWithTitle(oldConv, "old-concurrent")
	haveSeanceSession(f, oldConv, "old-concurrent-label", "old-concurrent-tmux", f.TestCwd("seance-concurrent-old"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "new-concurrent")
	haveSeanceSession(f, newConv, "new-concurrent-label", "new-concurrent-tmux", f.TestCwd("seance-concurrent-new"))
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	previous := agentd.RunSeanceHarness
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	agentd.RunSeanceHarness = func(_ context.Context, _ agentd.SeanceExecPlan) agentd.SeanceExecResult {
		entered <- struct{}{}
		<-release
		return agentd.SeanceExecResult{Stdout: "ok", Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })

	type outcome struct {
		status int
		body   string
	}
	done := make(chan outcome, 2)
	for range 2 {
		go func() {
			_, status, body := requestSeanceRun(t, f, newConv, map[string]any{
				"target": oldConv, "question": "hold",
			})
			done <- outcome{status: status, body: body}
		}()
	}
	<-entered
	<-entered
	_, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "third",
	})
	assert.Equal(t, http.StatusTooManyRequests, status, "body=%s", body)
	assert.Contains(t, body, "seance_busy")
	close(release)
	for range 2 {
		got := <-done
		assert.Equal(t, http.StatusOK, got.status, "body=%s", got.body)
	}
}
