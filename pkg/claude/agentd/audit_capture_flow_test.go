package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the audit-capture middleware (audit.go, JOH-268):
// every daemon-proxied command run through the /v1 (CLI) mux must leave a
// symbolic audit_log row naming WHO ran WHAT against WHICH target — and
// denials/errors are recorded too, not only successes.

// auditRowByVerb returns the newest audit row with the given verb, or
// fails the test if none exists.
func auditRowByVerb(t *testing.T, verb string) db.AuditLogEntry {
	t.Helper()
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: verb})
	require.NoError(t, err)
	require.NotEmpty(t, rows, "expected an audit row with verb %q", verb)
	return rows[0]
}

// Scenario: a human spawns a worker. The audit trail must record
// "operator | spawn | crew/worker", success.
func TestAudit_SpawnRecordsHumanActor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, spawn.Code, "spawn should succeed; body=%s", spawn.Raw)

	row := auditRowByVerb(t, "spawn")
	assert.Equal(t, db.AuditActorHuman, row.ActorKind)
	assert.Equal(t, "operator", row.ActorLabel)
	assert.Equal(t, "crew", row.GroupName)
	assert.Equal(t, "worker", row.TargetLabel)
	assert.Equal(t, http.StatusOK, row.Status)
	assert.Equal(t, db.AuditSourceCLI, row.Source)
	assert.Equal(t, http.MethodPost, row.Method)
}

// Scenario: an agent sends an intra-group message. The trail records
// "<sender> | message | <recipient> | <preview>", with the agent as the
// actor.
func TestAudit_MessageRecordsAgentActorAndPreview(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	// Spawn two real agents so they carry resolvable conv titles (a real
	// agent always does); the audit middleware snapshots those titles.
	po := f.AsHuman().Spawn("crew", "po")
	require.Equal(t, http.StatusOK, po.Code, "spawn po; body=%s", po.Raw)
	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	// Wait until both titles resolve at the contact surface.
	f.AssertGroupMember("crew", po.ConvID, "po", 5*time.Second)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{
			"to":   worker.ConvID,
			"body": "rebasing now, hold the merge",
		}), po.ConvID))
	require.Equal(t, http.StatusOK, rec.Code, "intra-group message should succeed; body=%s", rec.Body.String())

	row := auditRowByVerb(t, "message")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, po.ConvID, row.ActorConv)
	assert.Equal(t, "po", row.ActorLabel, "actor label is the sender's display title")
	assert.Equal(t, worker.ConvID, row.TargetConv)
	assert.Equal(t, "worker", row.TargetLabel, "target label is the recipient's display title")
	assert.Contains(t, row.Detail, "rebasing now", "message preview is captured in detail")
	assert.Equal(t, http.StatusOK, row.Status)
}

// Scenario: a worker replies to a message from the PO via `tclaude agent
// reply` (POST /v1/messages/{id}/reply). The reply must leave its own
// audit row — verb "reply", actor the worker, target the original sender
// (the PO) — even though the reply path is three segments and the body
// carries no `to`. Regression: the single-segment "messages" audit route
// never matched the reply path, so replies were silently unaudited.
func TestAudit_ReplyRecordsAgentActorAndTarget(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	po := f.AsHuman().Spawn("crew", "po")
	require.Equal(t, http.StatusOK, po.Code, "spawn po; body=%s", po.Raw)
	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	f.AssertGroupMember("crew", po.ConvID, "po", 5*time.Second)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)

	// PO messages the worker; capture the message id to reply to.
	sent := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{
			"to":      worker.ConvID,
			"subject": "status?",
			"body":    "where are we on the rebase",
		}), po.ConvID))
	require.Equal(t, http.StatusOK, sent.Code, "message should succeed; body=%s", sent.Body.String())
	var sentBody struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(sent.Body.Bytes(), &sentBody))
	require.NotZero(t, sentBody.ID, "message id should be returned")

	// Worker replies — the reply target is derived server-side from the
	// original (the PO), not carried in the body.
	reply := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost,
			fmt.Sprintf("/v1/messages/%d/reply", sentBody.ID),
			map[string]any{"body": "rebase done, merging now"}), worker.ConvID))
	require.Equal(t, http.StatusOK, reply.Code, "reply should succeed; body=%s", reply.Body.String())

	row := auditRowByVerb(t, "reply")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, worker.ConvID, row.ActorConv)
	assert.Equal(t, "worker", row.ActorLabel, "actor label is the replier's display title")
	assert.Equal(t, po.ConvID, row.TargetConv, "reply target is the original sender")
	assert.Equal(t, "po", row.TargetLabel)
	assert.Equal(t, "crew", row.GroupName, "reply stays on the original's routing group")
	assert.Contains(t, row.Detail, "rebase done", "reply body is captured in detail")
	assert.Equal(t, http.StatusOK, row.Status)
	assert.Equal(t, db.AuditSourceCLI, row.Source)
}

// Scenario: an agent renames ITSELF (`tclaude agent rename "<title>"` →
// POST /v1/whoami/rename). Self-lifecycle used to be invisible to the trail
// (only the --target peer path /v1/agent/{conv}/… was audited). With the
// self.rename slug granted, the action succeeds and the new title must land
// in the audit detail, same as the manager-path rename.
func TestAudit_SelfRenameCapturesTitle(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)
	require.NoError(t, db.SetAgentPermissionOverride(
		worker.ConvID, agentd.PermSelfRename, db.PermEffectGrant, "operator"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/rename",
			map[string]any{"title": "audit-fixer"}), worker.ConvID))
	require.Equal(t, http.StatusOK, rec.Code, "self-rename should succeed; body=%s", rec.Body.String())

	row := auditRowByVerb(t, "rename")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, worker.ConvID, row.ActorConv)
	assert.Equal(t, db.AuditSourceCLI, row.Source)
	assert.Contains(t, row.Detail, "audit-fixer", "the new title is captured in detail")
}

// Scenario: an agent attempts to reincarnate ITSELF without holding the
// self.reincarnate slug — the daemon refuses (403). Self-lifecycle DENIALS
// must leave a trail too (the audit module records every outcome), so the
// row exists with verb "reincarnate", the agent as actor, and a 4xx status.
// Doubles as proof the self-path /v1/whoami/{verb} is matched at all.
func TestAudit_SelfReincarnateDeniedIsRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/reincarnate",
			map[string]any{"follow_up": "carry on"}), worker.ConvID))
	require.GreaterOrEqual(t, rec.Code, 400, "self-reincarnate without the slug should be refused; got %d", rec.Code)

	row := auditRowByVerb(t, "reincarnate")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, worker.ConvID, row.ActorConv)
	assert.GreaterOrEqual(t, row.Status, 400, "the denial status is recorded")
	assert.Equal(t, db.AuditSourceCLI, row.Source)
}

// Scenario: an agent deletes a message from its inbox (DELETE
// /v1/messages/{id}). We audit sends, so we audit deletions too — verb
// "message.delete", actor the deleter.
func TestAudit_MessageDeleteRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	po := f.AsHuman().Spawn("crew", "po")
	require.Equal(t, http.StatusOK, po.Code, "spawn po; body=%s", po.Raw)
	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	f.AssertGroupMember("crew", po.ConvID, "po", 5*time.Second)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)

	sent := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{
			"to":   worker.ConvID,
			"body": "please delete me",
		}), po.ConvID))
	require.Equal(t, http.StatusOK, sent.Code, "message should send; body=%s", sent.Body.String())
	var sentBody struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(sent.Body.Bytes(), &sentBody))
	require.NotZero(t, sentBody.ID)

	// The recipient deletes the message it received.
	del := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodDelete,
			fmt.Sprintf("/v1/messages/%d", sentBody.ID), nil), worker.ConvID))
	require.Equal(t, http.StatusOK, del.Code, "delete should succeed; body=%s", del.Body.String())

	row := auditRowByVerb(t, "message.delete")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, worker.ConvID, row.ActorConv)
	assert.Contains(t, row.Detail, fmt.Sprintf("#%d", sentBody.ID), "the deleted message id is captured")
}

// Scenario: an agent prunes its inbox (POST /v1/inbox/prune). The bulk
// delete leaves a "inbox.prune" row summarising the cutoff.
func TestAudit_InboxPruneRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	worker := f.AsHuman().Spawn("crew", "worker")
	require.Equal(t, http.StatusOK, worker.Code, "spawn worker; body=%s", worker.Raw)
	f.AssertGroupMember("crew", worker.ConvID, "worker", 5*time.Second)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/inbox/prune",
			map[string]any{"older_than_seconds": 3600}), worker.ConvID))
	require.Equal(t, http.StatusOK, rec.Code, "prune should succeed; body=%s", rec.Body.String())

	row := auditRowByVerb(t, "inbox.prune")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, worker.ConvID, row.ActorConv)
	assert.Equal(t, http.StatusOK, row.Status)
}

// Scenario: an agent tries to retire another agent it neither owns nor
// has the slug for. The command is denied (403) — and that DENIAL must
// still leave an audit row, so the trail answers "who tried what".
func TestAudit_DeniedAttemptIsRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const target = "019ec010-3333-3333-3333-333333333333"
	const attacker = "019ec010-4444-4444-4444-444444444444"
	f.HaveAliveSession(target, "victim", "tmux-victim", "/work")
	f.HaveMember("crew", target)
	// attacker is a solo agent: not a member of crew, no agent.retire slug,
	// owns no group containing the target → the retire must be refused.

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+target+"/retire", nil), attacker))
	require.GreaterOrEqual(t, rec.Code, 400, "retire by an unauthorised agent should be refused; got %d", rec.Code)

	row := auditRowByVerb(t, "retire")
	assert.Equal(t, db.AuditActorAgent, row.ActorKind)
	assert.Equal(t, attacker, row.ActorConv)
	assert.GreaterOrEqual(t, row.Status, 400, "the denial status is recorded")

	// The failure surfaces under the "failure" outcome filter and is
	// excluded from "success".
	fails, err := db.ListAuditLog(db.AuditLogFilter{Outcome: "failure"})
	require.NoError(t, err)
	require.NotEmpty(t, fails)
	oks, err := db.ListAuditLog(db.AuditLogFilter{Outcome: "success", Verb: "retire"})
	require.NoError(t, err)
	assert.Empty(t, oks, "a denied retire must not appear as a success")
}

// Scenario: a group rename. The new name lives under the `new_name` body
// key (not `name`/`title`), so the trail must still capture it in detail.
func TestAudit_GroupRenameCapturesNewName(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/groups/crew/rename",
			map[string]any{"new_name": "newcrew"})))
	require.Equal(t, http.StatusOK, rec.Code, "rename should succeed; body=%s", rec.Body.String())

	row := auditRowByVerb(t, "group.rename")
	assert.Equal(t, "crew", row.GroupName)
	assert.Contains(t, row.Detail, "newcrew", "the new group name is captured in detail")
}

// Read-only / non-command requests (a GET, the snapshot poll) must NOT
// be audited — the trail is commands only.
func TestAudit_ReadsAreNotRecorded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	// A GET of the group's members is a read — no row.
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/groups/crew/members", nil)))
	require.Equal(t, http.StatusOK, rec.Code)

	n, err := db.CountAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a read-only GET must not create an audit row")
}
