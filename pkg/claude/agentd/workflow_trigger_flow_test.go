package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These flow tests exercise the JOH-59 trigger spike's MECHANISM only:
// POST /v1/agent/{conv}/workflow-run resolves an alive target pane and
// injects the saved workflow's `/<name>` launch command via tmux
// send-keys. We assert the right keys land (or, on rejection, that
// nothing is injected) — NOT a live workflow launch (CC's launch is
// model-/user-cooperative and out of scope for a deterministic test).

// writeUserSavedWorkflow drops a minimal saved-workflow .js under the
// test HOME's user-scope dir (~/.claude/workflows/saved/<name>.js), so
// ccworkflows.DefaultSavedScripts enumerates it as a known name.
func writeUserSavedWorkflow(t *testing.T, homeDir, name string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "workflows", "saved")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	body := "export const meta = { name: '" + name + "', description: 'fixture' }\n" +
		"phase('Only')\n" +
		"await agent('do the thing')\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".js"), []byte(body), 0o644))
}

// sentTo returns every send-keys text recorded against a tmux pane.
func sentTo(f *testharness.Flow, target string) []string {
	var out []string
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target {
			out = append(out, sk.Text)
		}
	}
	return out
}

const (
	wtTargetConv  = "wf01-1111-2222-3333-4444"
	wtTargetLabel = "spwn-wf001"
	wtTargetTmux  = "tclaude-spwn-wf001"
	wtTargetCwd   = "/tmp/wf-work"
	wtSavedName   = "demo-flow"
)

// Human operator triggers a known saved workflow into an alive target
// pane: 200, and `/demo-flow` is injected as the launch command.
func TestWorkflowRun_HumanInjectsLaunchCommand(t *testing.T) {
	f := newFlow(t)
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": wtSavedName})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	f.AssertSentContains(wtTargetTmux+":0.0", "/"+wtSavedName, 2*time.Second)
}

// An unknown name is rejected (404) and nothing is injected — the
// name-only existence gate.
func TestWorkflowRun_UnknownNameRejected(t *testing.T) {
	f := newFlow(t)
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": "no-such-flow"})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)

	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, sentTo(f, wtTargetTmux+":0.0"),
		"nothing should be injected when the workflow name is unknown")
}

// A name that fails the slash-command-safe charset is rejected (400)
// with nothing injected — the keystroke-injection boundary. The classic
// breakout attempt (a newline + another slash command) must not reach
// the pane.
func TestWorkflowRun_KeystrokeInjectionRejected(t *testing.T) {
	f := newFlow(t)
	// Even if an attacker manages to land a matching file on disk, the
	// charset gate rejects the breakout name before existence is checked.
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	for _, bad := range []string{"demo-flow\n/rename pwned", "demo flow", "../etc", "a/b"} {
		r := testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+wtTargetConv+"/workflow-run",
			map[string]any{"name": bad})
		r = agentd.AsHumanPeer(r)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"name %q should be rejected by the charset gate; body=%s", bad, rec.Body.String())
	}
	assert.Empty(t, sentTo(f, wtTargetTmux+":0.0"),
		"no keystrokes should reach the pane for any rejected name")
}

// An agent caller with no workflow.trigger slug and no group ownership
// is refused (403) — the slug is default-denied. Baseline that the gate
// is real before the granted-path test below.
func TestWorkflowRun_AgentWithoutSlugRefused(t *testing.T) {
	f := newFlow(t)
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	const callerConv = "cccc-1111-2222-3333-4444"
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": wtSavedName})
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)

	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, sentTo(f, wtTargetTmux+":0.0"),
		"a refused caller must not cause any injection")
}

// An agent caller GRANTED the workflow.trigger slug succeeds (200) and
// the launch command is injected.
func TestWorkflowRun_AgentWithSlugSucceeds(t *testing.T) {
	f := newFlow(t)
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	const callerConv = "cccc-1111-2222-3333-4444"
	require.NoError(t, db.GrantAgentPermission(callerConv, agentd.PermWorkflowTrigger, "test"))

	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": wtSavedName})
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	f.AssertSentContains(wtTargetTmux+":0.0", "/"+wtSavedName, 2*time.Second)
}

// A workflow.trigger grant to one agent must not leak to another — the
// per-conv grant is keyed by conv-id, so an ungranted sibling is still
// refused.
func TestWorkflowRun_SlugDoesNotLeakAcrossAgents(t *testing.T) {
	f := newFlow(t)
	writeUserSavedWorkflow(t, f.World.HomeDir, wtSavedName)
	f.HaveConvWithTitle(wtTargetConv, "worker")
	f.HaveAliveSession(wtTargetConv, wtTargetLabel, wtTargetTmux, wtTargetCwd)

	const grantedConv = "aaaa-1111-2222-3333-4444"
	const otherConv = "bbbb-1111-2222-3333-4444"
	require.NoError(t, db.GrantAgentPermission(grantedConv, agentd.PermWorkflowTrigger, "test"))

	// A/B in the same setup: the ungranted sibling is refused...
	rDeny := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": wtSavedName})
	rDeny = agentd.AsAgentPeer(rDeny, otherConv)
	recDeny := testharness.Serve(f.Mux, rDeny)
	require.Equal(t, http.StatusForbidden, recDeny.Code,
		"ungranted sibling must be refused; body=%s", recDeny.Body.String())

	// ...while the granted conv succeeds against the same target. This
	// proves the 403 above is the per-conv grant boundary, not some
	// unrelated setup failure.
	rOK := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+wtTargetConv+"/workflow-run",
		map[string]any{"name": wtSavedName})
	rOK = agentd.AsAgentPeer(rOK, grantedConv)
	recOK := testharness.Serve(f.Mux, rOK)
	require.Equal(t, http.StatusOK, recOK.Code,
		"granted conv must succeed; body=%s", recOK.Body.String())
}
