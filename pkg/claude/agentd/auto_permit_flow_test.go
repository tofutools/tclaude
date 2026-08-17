package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The question Claude Code's EnterWorktree safety check asks. The sim renders
// it with the real Yes / "No, and tell Claude what to do differently" options.
const enterWorktreeQuestion = `Enter the worktree at "/home/dev/git/proj-wt"? ` +
	`This moves the session's working directory and write access there, and loads ` +
	`project configuration (CLAUDE.md, settings) from that location.`

// autoPermitAgent stands up a live Claude Code agent parked on the EnterWorktree
// safety check: an enrolled conv, an alive pane whose sim is rendering the
// dialog, and a session row in awaiting_permission naming the tool. Returns the
// pane sim and the session row's id.
func autoPermitAgent(t *testing.T, f *testharness.Flow, conv, label, tmux string) (*testharness.CCSim, string) {
	t.Helper()
	f.HaveConvWithTitle(conv, "worker")
	f.HaveEnrolledAgent(conv)
	f.HaveAliveSession(conv, label, tmux, f.TestCwd("wt"))

	cc := f.World.CCs.GetByLabel(label)
	require.NotNil(t, cc, "the pane sim should be registered")
	cc.ShowPermissionPrompt(enterWorktreeQuestion)

	rows, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	row := rows[0]
	row.Status = session.StatusAwaitingPermission
	row.StatusDetail = "EnterWorktree"
	require.NoError(t, db.SaveSession(row))
	return cc, row.ID
}

// past is a tick time far enough ahead of the seeded row that the dwell delay
// (which exists to let the dialog paint) is satisfied.
func autoPermitTickTime() time.Time { return time.Now().Add(time.Minute) }

// Scenario: an agent whose operator opted it into `enter-worktree` is parked on
// the EnterWorktree safety check. The sweep presses Enter, the dialog is
// ACCEPTED (asserted on the sim's own state, not merely on a key arriving), and
// the answer is recorded in the audit trail.
func TestAutoPermit_AnswersConsentedPrompt(t *testing.T) {
	f := newFlow(t)
	const conv = "aup0-1111-2222-3333-4444"
	cc, sessionID := autoPermitAgent(t, f, conv, "aup0", "tcl-aup0")

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	require.NoError(t, db.SetAgentAutoPermit(agentID, "enter-worktree", "human", time.Now()))

	agentd.RunAutoPermitTickForTest(autoPermitTickTime())

	assert.Equal(t, 1, cc.PermissionPromptsAccepted(), "the consented dialog is accepted")
	assert.False(t, cc.PermissionPromptUp(), "the prompt no longer blocks the agent")

	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "auto-permit.answer"})
	require.NoError(t, err)
	require.Len(t, entries, 1, "the operator must be able to see what was approved for them")
	assert.Equal(t, db.AuditActorSystem, entries[0].ActorKind)
	assert.Equal(t, db.AuditSourceAutoPermit, entries[0].Source)
	assert.Equal(t, conv, entries[0].TargetConv)
	assert.Equal(t, sessionID, entries[0].SessionID)
	assert.Contains(t, entries[0].Detail, "enter-worktree")
	assert.Contains(t, entries[0].Detail, "EnterWorktree", "the answered prompt is named")
}

// Scenario: the same prompt, on an agent nobody opted in. Nothing is pressed —
// opt-in is the whole gate, and off is the default for every agent.
func TestAutoPermit_LeavesUnconsentedPromptAlone(t *testing.T) {
	f := newFlow(t)
	const conv = "aup1-1111-2222-3333-4444"
	cc, _ := autoPermitAgent(t, f, conv, "aup1", "tcl-aup1")

	agentd.RunAutoPermitTickForTest(autoPermitTickTime())

	assert.Equal(t, 0, cc.PermissionPromptsAccepted(), "no consent, no keystroke")
	assert.True(t, cc.PermissionPromptUp(), "the prompt still waits for the human")
}

// Scenario: an opted-in agent is awaiting permission on a DIFFERENT prompt than
// the one consented to. Consent is per named condition, so nothing is pressed —
// this is what keeps auto-permit from being a blanket accept-everything mode.
func TestAutoPermit_LeavesOtherPromptsAlone(t *testing.T) {
	f := newFlow(t)
	const conv = "aup2-1111-2222-3333-4444"
	cc, _ := autoPermitAgent(t, f, conv, "aup2", "tcl-aup2")

	// Same agent, same consent — but the pane is now on a Bash prompt.
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentAutoPermit(agentID, "enter-worktree", "human", time.Now()))

	rows, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	rows[0].StatusDetail = "Bash"
	require.NoError(t, db.SaveSession(rows[0]))
	cc.ShowPermissionPrompt("Run `rm -rf build`?")

	agentd.RunAutoPermitTickForTest(autoPermitTickTime())

	assert.Equal(t, 0, cc.PermissionPromptsAccepted(), "an unconsented prompt is never answered")
	assert.True(t, cc.PermissionPromptUp())
}

// Scenario: the status projection says the agent is awaiting the consented
// prompt, but the pane does NOT show that dialog — the stale-status case (the
// human already answered, or the prompt was some other dialog). The pane read
// is the real gate, so no key is sent.
func TestAutoPermit_RequiresPaneEvidence(t *testing.T) {
	f := newFlow(t)
	const conv = "aup3-1111-2222-3333-4444"
	cc, _ := autoPermitAgent(t, f, conv, "aup3", "tcl-aup3")

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentAutoPermit(agentID, "enter-worktree", "human", time.Now()))

	// The dialog is gone from the pane while the row still says awaiting.
	cc.ShowPermissionPrompt("")

	agentd.RunAutoPermitTickForTest(autoPermitTickTime())

	assert.Equal(t, 0, cc.PermissionPromptsAccepted(),
		"a stale status must not license a blind keystroke")
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "auto-permit.answer"})
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was answered, so nothing is recorded")
}

// Scenario: a second tick right after an answer does not press again. The
// cooldown is the belt to the pane-evidence braces.
func TestAutoPermit_DoesNotAnswerTwice(t *testing.T) {
	f := newFlow(t)
	const conv = "aup4-1111-2222-3333-4444"
	cc, _ := autoPermitAgent(t, f, conv, "aup4", "tcl-aup4")

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentAutoPermit(agentID, "enter-worktree", "human", time.Now()))

	now := autoPermitTickTime()
	agentd.RunAutoPermitTickForTest(now)
	// The harness re-renders the same dialog (a pathological pane) while the
	// row still reads awaiting: the cooldown must still hold the second press.
	cc.ShowPermissionPrompt(enterWorktreeQuestion)
	agentd.RunAutoPermitTickForTest(now.Add(time.Second))

	assert.Equal(t, 1, cc.PermissionPromptsAccepted(), "one prompt, one press")
}

// Scenario: the opt-in API. An agent holding self.auto-permit turns a condition
// on and back off, and the listing reflects both the registry and its own state.
func TestAutoPermit_SelfOptInRoundTrip(t *testing.T) {
	f := newFlow(t)
	const conv = "aup5-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveEnrolledAgent(conv)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermSelfAutoPermit, "test"))

	on := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/auto-permit",
		map[string]any{"condition": "enter-worktree", "enabled": true})
	resp := testharness.Serve(f.Mux, agentd.AsAgentPeer(on, conv))
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), "\"enabled\":true")

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	stored, err := db.ListAgentAutoPermits(agentID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, "enter-worktree", stored[0].Condition)

	off := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/auto-permit",
		map[string]any{"condition": "enter-worktree", "enabled": false})
	resp = testharness.Serve(f.Mux, agentd.AsAgentPeer(off, conv))
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())

	stored, err = db.ListAgentAutoPermits(agentID)
	require.NoError(t, err)
	assert.Empty(t, stored, "consent is revocable")
}

// Scenario: an agent with no grant is refused. self.auto-permit is deliberately
// NOT default-granted — it consents to a gate reserved for a human keystroke.
func TestAutoPermit_UngrantedForbidden(t *testing.T) {
	f := newFlow(t)
	const conv = "aup6-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveEnrolledAgent(conv)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/auto-permit",
		map[string]any{"condition": "enter-worktree", "enabled": true})
	resp := testharness.Serve(f.Mux, agentd.AsAgentPeer(r, conv))
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), agentd.PermSelfAutoPermit, "the 403 names the missing slug")
}

// Scenario: a group OWNER without the slug is still refused. Like
// human.clipboard, self.auto-permit is NOT owner-implied: owning a group does
// not structurally confer the authority to pre-answer human-only prompts.
func TestAutoPermit_GroupOwnerStillForbidden(t *testing.T) {
	f := newFlow(t)
	const ownerConv = "aup7-1111-2222-3333-4444"
	g := f.HaveGroup("owned-team")
	f.HaveMember("owned-team", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/auto-permit",
		map[string]any{"condition": "enter-worktree", "enabled": true})
	resp := testharness.Serve(f.Mux, agentd.AsAgentPeer(r, ownerConv))
	require.Equal(t, http.StatusForbidden, resp.Code,
		"group ownership must NOT confer auto-permit consent; body=%s", resp.Body.String())
}

// Scenario: an unknown condition is refused rather than stored, so an operator
// can't come away believing they consented to something inert.
func TestAutoPermit_UnknownConditionRejected(t *testing.T) {
	f := newFlow(t)
	const conv = "aup8-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveEnrolledAgent(conv)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermSelfAutoPermit, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/auto-permit",
		map[string]any{"condition": "everything", "enabled": true})
	resp := testharness.Serve(f.Mux, agentd.AsAgentPeer(r, conv))
	require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), "enter-worktree", "the error lists what IS available")
}
