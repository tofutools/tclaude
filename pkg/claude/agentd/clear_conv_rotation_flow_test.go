package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These flow tests cover issue #192: Claude Code's /clear rotates the
// conversation's conv-id while keeping the same process alive. agentd
// keys group memberships / ownerships / permission overrides on
// conv-id, so without a migration a /clear strands the live agent —
// its identity stays on the dead old conv-id, the dashboard shows it
// offline forever, and the fresh conv shows up as a detached non-agent
// conversation.
//
// CCSim.clear models the real /clear: a conv-id rotation plus the
// observed SessionEnd(reason=clear) / SessionStart(source=clear) hook
// sequence, driven through the production hook callback. The daemon
// surfaces below are then asserted exactly as `tclaude agent groups
// members` / `tclaude agent resume` would render them.

// 36-char (UUID-shaped, hex) conv-ids — ScanAndUpsertFile gates on
// len==36, and the title-resolution path scans the post-/clear .jsonl.
const (
	clearGroup      = "alpha"
	clearAgentConv  = "c1ea0000-1111-2222-3333-444444444444"
	clearAgentLabel = "spwn-clear-001"
	clearAgentTmux  = "tclaude-spwn-clear-001"
	clearAgentTitle = "clear-victim"
	clearPeerConv   = "9ee50000-1111-2222-3333-555555555555"
)

// setupClearedAgent stands up an agent — custom title, group
// membership, group ownership, a grant + a deny permission override —
// with a live session, plus a peer in the same group. It is the shared
// Given for the /clear scenarios below. Returns the group row.
func setupClearedAgent(t *testing.T, f *testharness.Flow) *db.AgentGroup {
	t.Helper()
	g := f.HaveGroup(clearGroup)
	f.HaveAliveSession(clearAgentConv, clearAgentLabel, clearAgentTmux, f.World.HomeDir)
	// Stamp the agent's display name into the .jsonl exactly as a real
	// /rename would, so the hook's pre-migration scan picks it up via
	// the production conv_index path. (HaveConvWithTitle would short-
	// circuit the .jsonl-scan path the fix relies on — see the
	// testharness title-refresh quirk.)
	cc := f.World.CCs.GetByLabel(clearAgentLabel)
	require.NotNil(t, cc, "CCSim for the agent should be registered")
	require.NoError(t, cc.WriteCustomTitle(clearAgentTitle),
		"seed the agent's customTitle turn")
	f.HaveMember(clearGroup, clearAgentConv)
	f.HaveMember(clearGroup, clearPeerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, clearAgentConv, "human"),
		"AddAgentGroupOwner")
	require.NoError(t, db.GrantAgentPermission(clearAgentConv, "human.notify", "human"),
		"GrantAgentPermission")
	require.NoError(t, db.SetAgentPermissionOverride(clearAgentConv, "agent.spawn",
		db.PermEffectDeny, "human"), "SetAgentPermissionOverride(deny)")
	return g
}

func findMember(members []testharness.MemberView, conv string) *testharness.MemberView {
	for i := range members {
		if members[i].ConvID == conv {
			return &members[i]
		}
	}
	return nil
}

// Scenario: a fully-provisioned agent (member, owner, two permission
// overrides, a custom title) is /clear'd.
//
// Expected: every identity facet now resolves under the NEW conv-id,
// the old conv-id drops cleanly off the agent roster, the carried name
// still shows, and a succession edge old→new is recorded.
func TestClearRotation_MigratesAgentIdentityToNewConvID(t *testing.T) {
	f := newFlow(t)
	g := setupClearedAgent(t, f)

	c := f.Clear(clearAgentLabel)
	require.NotEqual(t, c.OldConv, c.NewConv, "conv-id must rotate on /clear")

	// `tclaude agent groups members alpha`: the new conv is the live
	// member, with the carried title and owner flag; the old conv-id is
	// gone — no offline ghost.
	members := f.ListGroupMembers(g.Name)
	newM := findMember(members, c.NewConv)
	require.NotNil(t, newM, "new conv must be a member of %q; got %+v", g.Name, members)
	assert.Equal(t, clearAgentTitle, newM.Title,
		"carried display name should survive the /clear")
	assert.True(t, newM.Online, "agent should read online under the new conv-id")
	assert.True(t, newM.Owner, "group ownership should migrate to the new conv-id")
	assert.Nil(t, findMember(members, c.OldConv),
		"old conv-id must NOT linger as a member after /clear")

	// Permission overrides — grant AND deny — belong to the stable actor
	// (JOH-26), not a conv generation: they were never rekeyed, the rows stayed
	// put on agent_id and only the conv pointer advanced. So they resolve from
	// the new conv — AND from the old one, which is the same actor.
	wantPerms := map[string]string{
		"human.notify": db.PermEffectGrant,
		"agent.spawn":  db.PermEffectDeny,
	}
	newPerms, err := db.ListAgentPermissionOverridesForConv(c.NewConv)
	require.NoError(t, err)
	assert.Equal(t, wantPerms, newPerms, "permission overrides resolve under the new conv-id")
	oldPerms, err := db.ListAgentPermissionOverridesForConv(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, wantPerms, oldPerms,
		"the predecessor generation resolves to the same actor, so it sees the same overrides")

	// Ownership is agent-keyed too: both generations report the actor's ownership.
	ownNew, err := db.IsAgentGroupOwner(g.ID, c.NewConv)
	require.NoError(t, err)
	assert.True(t, ownNew, "new conv-id should own the group")
	ownOld, err := db.IsAgentGroupOwner(g.ID, c.OldConv)
	require.NoError(t, err)
	assert.True(t, ownOld, "the predecessor generation resolves to the same owning actor")

	// Both conv-ids are generations of ONE still-active actor (JOH-26 PR3c):
	// the actor's live pointer advanced old→new, but the predecessor is a past
	// generation, NOT a standalone retired entry — both resolve to the same
	// active agent. (The actor is reachable from the old conv via the
	// succession edge / séance.)
	oldState, err := db.AgentState(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, oldState,
		"the predecessor generation resolves to the still-active actor")
	newState, err := db.AgentState(c.NewConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, newState,
		"new conv-id is the live generation of the active agent")
	oldAgent, err := db.AgentIDForConv(c.OldConv)
	require.NoError(t, err)
	newAgent, err := db.AgentIDForConv(c.NewConv)
	require.NoError(t, err)
	assert.Equal(t, oldAgent, newAgent, "both generations share one actor")

	// Succession edge old→new — what powers ResolveLatestConv routing.
	succ, err := db.GetConvSuccessor(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, c.NewConv, succ, "a succession edge old→new must be recorded")

	// The agent's display name carried onto the actor's pending_name — the
	// dashboard fallback that shows the name immediately, before the /rename
	// injection lands and is scanned into conv_index.
	actor, err := db.GetAgentByConv(c.NewConv)
	require.NoError(t, err)
	require.NotNil(t, actor)
	assert.Equal(t, clearAgentTitle, actor.PendingName,
		"pending_name on the actor should carry the agent's display name")

	// The hook injected /rename so the new conversation also regains a
	// real customTitle — what makes the name durable across rescans and
	// visible in surfaces that don't consult pending_name (CC's own UI,
	// `tclaude conv ls`).
	f.AssertSentContains(clearAgentTmux+":0.0", "/rename "+clearAgentTitle, 2*time.Second)
}

// Scenario: after a /clear, the agent is taken offline and a resume is
// issued against the PRE-/clear conv-id.
//
// Expected: the resume resolves the stale id forward and brings the
// NEW conv back online — `tclaude agent resume <old-id>` still works.
func TestClearRotation_ResumeOfPreClearIDTargetsNewConv(t *testing.T) {
	f := newFlow(t)
	setupClearedAgent(t, f)

	c := f.Clear(clearAgentLabel)
	f.MarkOffline(clearAgentTmux) // agent goes offline post-/clear

	r := f.Resume(c.OldConv) // resume addressing the stale id
	f.AssertResumeSpawned(r)
	assert.Equal(t, c.NewConv, r.ConvID,
		"resume of the pre-/clear id must target the new conv")
}

// Scenario: a peer messages the agent using its PRE-/clear conv-id
// (e.g. a stale id lifted from an old scrollback).
//
// Expected: the succession edge routes the message forward — it lands
// in the NEW conv's inbox, with the original addressee on record. This
// is the proof that `tclaude agent reply/message` addressing survives
// the /clear (PO addendum #980).
func TestClearRotation_MessageToPreClearIDReachesAgent(t *testing.T) {
	f := newFlow(t)
	setupClearedAgent(t, f)

	c := f.Clear(clearAgentLabel)

	// The peer POSTs addressing the OLD conv-id directly.
	body := map[string]any{"to": c.OldConv, "body": "still with us?"}
	req := agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages", body),
		clearPeerConv)
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"POST /v1/messages body=%s", rec.Body.String())

	var resp struct {
		RedirectedFrom string `json:"redirected_from"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, c.OldConv, resp.RedirectedFrom,
		"send to the pre-/clear id should report a redirect")

	// The message landed in the NEW conv's inbox, not the old one.
	newRows, err := db.ListAgentMessagesForConv(c.NewConv, 100)
	require.NoError(t, err)
	require.Len(t, newRows, 1, "new conv inbox should hold the message")
	assert.Equal(t, c.OldConv, newRows[0].OriginalToConv, "OriginalToConv")
	assert.Equal(t, c.NewConv, newRows[0].ToConv, "ToConv (live successor)")

	oldRows, _ := db.ListAgentMessagesForConv(c.OldConv, 100)
	assert.Empty(t, oldRows, "old conv inbox should stay empty — chain-walk bypasses it")
}

// Scenario: a PLAIN conversation (never an agent — no group, no perms,
// no enrollment) is /clear'd.
//
// Expected: the /clear is a no-op for identity. The successor is NOT
// promoted to an agent and no succession edge is recorded — the
// migration's enrollment guard holds. The session row still follows
// the conv-id rotation, as it did before the fix.
func TestClearRotation_PlainConversationNotPromotedToAgent(t *testing.T) {
	f := newFlow(t)

	const (
		plainConv  = "91a10000-1111-2222-3333-666666666666"
		plainLabel = "spwn-plain-001"
		plainTmux  = "tclaude-spwn-plain-001"
	)
	plainCwd := f.TestCwd("plainwork")
	f.HaveAliveSession(plainConv, plainLabel, plainTmux, plainCwd)
	require.Equal(t, db.AgentStateNone, mustAgentState(t, plainConv),
		"precondition: the conv is a plain conversation, not an agent")

	c := f.Clear(plainLabel)

	assert.Equal(t, db.AgentStateNone, mustAgentState(t, c.NewConv),
		"/clear of a plain conversation must not promote the successor to an agent")
	assert.Equal(t, db.AgentStateNone, mustAgentState(t, c.OldConv),
		"the old plain conversation must stay un-enrolled")

	succ, err := db.GetConvSuccessor(c.OldConv)
	require.NoError(t, err)
	assert.Empty(t, succ, "no succession edge for a plain conversation's /clear")

	// The session row still follows the rotation — the fix does not
	// regress the existing conv-id tracking for non-agents.
	sess, err := db.FindSessionByConvID(c.NewConv)
	require.NoError(t, err)
	require.NotNil(t, sess, "session row should follow the conv-id rotation")
}

// Scenario: a transient migration failure on the post-/clear
// SessionStart hook (a synthetic SQLite hiccup) must NOT advance the
// session row's conv-id, and the next hook with the same rotation
// visible must converge — migrate identity, record the succession
// edge, and advance the session row.
//
// This is the retry condition that lives on needsIdentityMigration's
// (true, nil) predicate: as long as oldConv is still an active agent
// and no succession edge has been recorded, the predicate keeps
// firing across hooks. db.RotateAgentConv is atomic, so a failed
// attempt strands nothing — the next attempt is a clean retry. The
// fault is injected via session.SetRotateAgentConvForTest (a
// counter-keyed wrapper that returns an error once, then falls
// through to the real rotation), which is the cleanest seam for a
// "one-shot transient SQLite error" model.
func TestClearRotation_RetriesIdentityMigrationAfterTransientFailure(t *testing.T) {
	f := newFlow(t)
	g := setupClearedAgent(t, f)

	var calls int32
	t.Cleanup(session.SetRotateAgentConvForTest(
		func(oldConv, newConv, reason string) (string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return "", errors.New("synthetic SQLite hiccup")
			}
			return db.RotateAgentConv(oldConv, newConv, reason)
		}))

	c := f.Clear(clearAgentLabel)

	// After the failing migration: the session row's ConvID must
	// remain on oldConv so the next hook re-evaluates the predicate.
	// The conv-id rotation IS visible to CC (its .jsonl moved) — the
	// daemon's session row is what we deliberately hold back.
	state, err := session.LoadSessionState(clearAgentLabel)
	require.NoError(t, err)
	assert.Equal(t, c.OldConv, state.ConvID,
		"failed migration must leave the session row on the old conv-id so the predicate re-fires")

	// No succession edge yet — the migration transaction rolled back.
	succ, err := db.GetConvSuccessor(c.OldConv)
	require.NoError(t, err)
	assert.Empty(t, succ, "no succession edge until the migration commits")

	// Old conv still active — atomic migration means failure strands
	// nothing; identity is still wholly on the old conv-id.
	oldEnr, err := db.AgentState(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, oldEnr,
		"old conv must remain an active agent until the migration commits")

	// Identity rows have NOT moved yet — the old conv still owns the
	// group, holds the permissions, etc.
	oldPerms, err := db.ListAgentPermissionOverridesForConv(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"human.notify": db.PermEffectGrant,
		"agent.spawn":  db.PermEffectDeny,
	}, oldPerms, "permission overrides must remain on the old conv-id after a failed migration")

	// Drive the next hook: a UserPromptSubmit on the new conv-id, as
	// CC would fire on the user's next turn. needsIdentityMigration
	// sees the same (oldConv active, no edge) state and re-fires; the
	// counter-keyed migrator now returns the real one and commits.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		ConvID:        c.NewConv,
		HookEventName: "UserPromptSubmit",
		Cwd:           f.World.HomeDir,
	}, clearAgentLabel))

	// The retry converged: session row advanced to the new conv-id.
	state2, err := session.LoadSessionState(clearAgentLabel)
	require.NoError(t, err)
	assert.Equal(t, c.NewConv, state2.ConvID,
		"the next hook should converge: session row advances to the new conv-id")

	// Succession edge now recorded — the predicate flips false from
	// this point so further hooks won't re-migrate.
	succ2, err := db.GetConvSuccessor(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, c.NewConv, succ2,
		"the second-attempt migration must record the succession edge")

	// Identity facets resolve under the new conv-id, on the real
	// surfaces (group-members handler) — the same property the
	// happy-path test asserts.
	members := f.ListGroupMembers(g.Name)
	require.NotNil(t, findMember(members, c.NewConv),
		"membership must arrive on the new conv-id after retry; got %+v", members)
	assert.Nil(t, findMember(members, c.OldConv),
		"old conv-id must no longer be a member after retry")

	// Old conv is a past generation of the still-active actor (not a retired
	// entry) — same outcome as the happy path, since RotateAgentConv's
	// transaction is the same.
	oldState2, err := db.AgentState(c.OldConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, oldState2,
		"after retry, the predecessor resolves to the still-active actor")

	// We expect exactly two migration attempts: one failure, one success.
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"migration should be attempted twice: one transient failure, one retry")
}

func mustAgentState(t *testing.T, conv string) string {
	t.Helper()
	s, err := db.AgentState(conv)
	require.NoError(t, err)
	return s
}
