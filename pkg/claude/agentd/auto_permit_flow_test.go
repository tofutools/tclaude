package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Auto-permit flow tests. They drive a real PermissionRequest through the
// ordinary hook broker — the same endpoint every sandboxed launch already uses
// — and assert on what the DAEMON does with it. That placement is the point of
// the feature: an agent inside the sandbox can reach neither tmux nor the
// database, so the decision and the keystroke both have to happen host-side.

const (
	autoPermitConv  = "a0000000-1111-2222-3333-444444444444"
	autoPermitLabel = "spwn-auto-permit"
	autoPermitTmux  = "tmux-auto-permit"
)

// enterWorktreeEvent is the hook Claude Code fires when its EnterWorktree
// safety check goes up — the prompt no allow-rule, auto-mode setting or
// PreToolUse approval can clear.
func enterWorktreeEvent() session.BrokeredHookRequest {
	return session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: autoPermitConv, HookEventName: "PermissionRequest",
		ToolName: "EnterWorktree",
	}}
}

// Scenario: the operator granted this agent auto-permit.enter-worktree. Its
// prompt is answered from the host, and the answer is recorded where the
// operator can find it.
func TestAutoPermit_GrantedAgentIsAnswered(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)
	require.NoError(t, db.GrantAgentPermission(autoPermitConv,
		session.PermAutoPermitEnterWorktree, "human"))

	code, _ := postBrokeredHook(t, f, callerPID, enterWorktreeEvent())
	require.Equal(t, http.StatusOK, code, "a hook is never refused for lacking a grant")

	// The press is deliberately asynchronous: the harness is still blocked on
	// this hook, so the dialog is not painted yet.
	f.AssertSentContains(autoPermitTmux+":0.0", "Enter", 3*time.Second)

	// The row is written after the keystroke goes out, so poll rather than
	// racing it.
	var entries []db.AuditLogEntry
	require.Eventually(t, func() bool {
		var err error
		entries, err = db.ListAuditLog(db.AuditLogFilter{Verb: "auto-permit.answer"})
		return err == nil && len(entries) == 1
	}, 3*time.Second, 20*time.Millisecond,
		"the operator must see what was approved for them")
	assert.Equal(t, db.AuditActorSystem, entries[0].ActorKind)
	assert.Equal(t, autoPermitConv, entries[0].TargetConv)
	assert.Equal(t, "EnterWorktree", entries[0].Detail)
}

// Scenario: the agent is running a DIFFERENT harness. A tool name only means
// something inside the harness that defines it, and the accept keys are how
// THAT harness draws its dialog — so a same-named prompt elsewhere is not this
// condition, grant or no grant.
func TestAutoPermit_OtherHarnessIsLeftAlone(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)
	require.NoError(t, db.GrantAgentPermission(autoPermitConv,
		session.PermAutoPermitEnterWorktree, "human"))

	row, err := db.LoadSession(autoPermitLabel)
	require.NoError(t, err)
	row.Harness = harness.CodexName
	require.NoError(t, db.SaveSession(row))

	code, _ := postBrokeredHook(t, f, callerPID, enterWorktreeEvent())
	require.Equal(t, http.StatusOK, code)

	assertNoAutoPermitPress(t, f)
}

// Scenario: consent is withdrawn between the prompt and the keystroke. The
// press is the act being authorized, so the grant is re-read after the settle
// wait and the revocation lands before the key does.
func TestAutoPermit_RevokedDuringSettleIsNotPressed(t *testing.T) {
	// Stretch the settle wait so the revocation below is unambiguously inside
	// it, rather than racing a 400 ms window on a loaded machine.
	t.Cleanup(agentd.SetAutoPermitSettleDelayForTest(2 * time.Second))
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)
	require.NoError(t, db.GrantAgentPermission(autoPermitConv,
		session.PermAutoPermitEnterWorktree, "human"))

	code, _ := postBrokeredHook(t, f, callerPID, enterWorktreeEvent())
	require.Equal(t, http.StatusOK, code)
	_, err := db.RevokeAgentPermission(autoPermitConv, session.PermAutoPermitEnterWorktree)
	require.NoError(t, err)

	assertNoAutoPermitPress(t, f)
}

// Scenario: the same prompt on an agent nobody granted the slug. The hook still
// succeeds — a hook is telemetry, not a request that can be denied — but no key
// is pressed and nothing is recorded. Off is the default for every agent.
func TestAutoPermit_UngrantedAgentIsLeftAlone(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)

	code, _ := postBrokeredHook(t, f, callerPID, enterWorktreeEvent())
	require.Equal(t, http.StatusOK, code, "an ungranted agent's hook is not an error")

	assertNoAutoPermitPress(t, f)
}

// Scenario: a granted agent hits a DIFFERENT prompt. Consent is per named
// prompt with no wildcard to fall back on — which is what keeps this from being
// a blanket accept mode.
func TestAutoPermit_OtherPromptsAreLeftAlone(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)
	require.NoError(t, db.GrantAgentPermission(autoPermitConv,
		session.PermAutoPermitEnterWorktree, "human"))

	event := enterWorktreeEvent()
	event.Input.ToolName = "Bash"
	code, _ := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)

	assertNoAutoPermitPress(t, f)
}

// Scenario: a group OWNER of the agent, with no slug granted. Ownership does not
// structurally confer the authority to pre-answer a gate the harness reserves
// for a human — the same "lean no" as human.clipboard.
func TestAutoPermit_GroupOwnershipDoesNotConsent(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, autoPermitConv, autoPermitLabel, autoPermitTmux, brokerPanePID)
	g := f.HaveGroup("owned-team")
	f.HaveMember("owned-team", autoPermitConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, autoPermitConv, "test"))

	code, _ := postBrokeredHook(t, f, callerPID, enterWorktreeEvent())
	require.Equal(t, http.StatusOK, code)

	assertNoAutoPermitPress(t, f)
}

// assertNoAutoPermitPress fails if anything was sent to the agent's pane or
// recorded as answered. It waits out the settle delay first, so it cannot pass
// merely by looking before a press that was on its way.
func assertNoAutoPermitPress(t *testing.T, f *testharness.Flow) {
	t.Helper()
	assert.False(t, f.World.Tmux.WaitForSendKeys(autoPermitTmux+":0.0", "Enter", time.Second),
		"the prompt must be left waiting for the human")
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "auto-permit.answer"})
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was answered, so nothing is recorded")
}
