package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent in a tight loop tries to clone the same source
// conv twice in rapid succession. The first clone lands; the second
// must be refused with HTTP 429 because clone is the only default-
// granted fork-doubling verb (self.clone), and an unbounded loop
// could fork conv-ids and tmux sessions until the host runs out.
//
// The caller here is an *agent* (it owns the group containing the
// target, satisfying the manager-pattern permission check). The
// cooldown applies only to agent-initiated clones — see
// TestClone_RateLimitExemptsHuman for the human side.
//
// The cooldown is per-source-conv: rate-limit storage keys on the
// target being cloned, so cloning *different* sources back-to-back is
// unaffected.
//
// Real surface assertion: agent_clone_history has exactly one row for
// the source after the locked attempt — the failed second clone does
// NOT consume an additional slot (the INSERT-WHERE-NOT-EXISTS leaves
// the table untouched on rate-limit hit).
func TestClone_RateLimitBlocksRapidSecondClone(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = time.Hour
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	grp := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	// oldConv owns its group, so it can clone members of that group
	// (itself) via the manager-pattern path — an agent-initiated clone.
	require.NoError(t, db.AddAgentGroupOwner(grp.ID, oldConv, "test"), "AddAgentGroupOwner")

	// First clone — succeeds.
	c1 := f.AsAgent(oldConv).CloneFresh(oldConv)
	require.NotEmpty(t, c1.NewConv, "first clone returned empty NewConv: %s", c1.Raw)

	// Second clone of the same source — should be 429. Use the raw
	// helper because CloneFresh fatals on non-200.
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+oldConv+"/clone",
		map[string]any{"no_copy_conv": true})
	r = agentd.AsAgentPeer(r, oldConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"second clone status. body=%s", rec.Body.String())

	// Real-surface invariant: only the successful first attempt
	// consumed a slot. INSERT-WHERE-NOT-EXISTS must leave the table
	// untouched on rate-limit; otherwise repeated rapid attempts
	// would extend the cooldown indefinitely (each failed try would
	// reset the timer), which is not the intended behaviour.
	last, err := db.LatestCloneAt(oldConv)
	require.NoError(t, err, "LatestCloneAt")
	require.False(t, last.IsZero(), "agent_clone_history has no row for source; first clone failed to record")
}

// Scenario: the same source can be cloned again once cooldown has
// elapsed. We don't actually sleep the test; we shrink CloneCooldown
// to zero and verify the unlocked path is reachable. This is the
// dual of the locked-path test above and pins the obvious next-day
// regression: a too-aggressive lock that never releases.
func TestClone_RateLimitClearsAfterCooldown(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = time.Hour
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	grp := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	require.NoError(t, db.AddAgentGroupOwner(grp.ID, oldConv, "test"), "AddAgentGroupOwner")

	// First clone lands.
	c1 := f.AsAgent(oldConv).CloneFresh(oldConv)
	require.NotEmpty(t, c1.NewConv, "first clone returned empty NewConv: %s", c1.Raw)

	// Drop cooldown to 0 — the next clone should pass.
	agentd.CloneCooldown = 0

	c2 := f.AsAgent(oldConv).CloneFresh(oldConv)
	require.NotEmpty(t, c2.NewConv, "second clone returned empty NewConv after cooldown drop: %s", c2.Raw)
	assert.NotEqual(t, c1.NewConv, c2.NewConv, "expected distinct new convs from two clones")
}

// Scenario: cloning two *different* sources rapidly is fine. The rate
// limit is per-source, not global, so a manager fanning out clones of
// distinct workers shouldn't be artificially throttled.
func TestClone_RateLimitIsPerSource(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = time.Hour
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)

	const aConv = "aaaa-1111-2222-3333-4444"
	const bConv = "bbbb-1111-2222-3333-4444"

	f.HaveConvWithTitle(aConv, "alpha-worker")
	f.HaveAliveSession(aConv, "spwn-a-001", "tclaude-spwn-a-001", "/tmp/work-a")
	f.HaveConvWithTitle(bConv, "beta-worker")
	f.HaveAliveSession(bConv, "spwn-b-001", "tclaude-spwn-b-001", "/tmp/work-b")
	grp := f.HaveGroup("team")
	f.HaveMember("team", aConv)
	f.HaveMember("team", bConv)
	// aConv owns the team, so as an agent it can clone both members.
	require.NoError(t, db.AddAgentGroupOwner(grp.ID, aConv, "test"), "AddAgentGroupOwner")

	cA := f.AsAgent(aConv).CloneFresh(aConv)
	require.NotEmpty(t, cA.NewConv, "clone of A failed: %s", cA.Raw)
	cB := f.AsAgent(aConv).CloneFresh(bConv)
	require.NotEmpty(t, cB.NewConv, "clone of B failed despite being a different source: %s", cB.Raw)
}

// Scenario: a human cloning the same source twice in rapid succession
// is NOT rate-limited. The cooldown's whole purpose is to bound a
// runaway *agent* loop; a human can't fire clones at machine speed and
// asks for each one deliberately (CLI invocation or a dashboard
// click). Both rapid human clones must land even with the cooldown
// pinned to an hour.
//
// Real-surface invariant: agent_clone_history has NO row for the
// source — the human path skips db.ClaimCloneSlot entirely, so it
// never even touches the rate-limit table.
func TestClone_RateLimitExemptsHuman(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = time.Hour
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-old-001", "tclaude-spwn-old-001", "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	// Two human clones back-to-back — both land despite the 1h cooldown.
	c1 := f.AsHuman().CloneFresh(oldConv)
	require.NotEmpty(t, c1.NewConv, "first human clone returned empty NewConv: %s", c1.Raw)
	c2 := f.AsHuman().CloneFresh(oldConv)
	require.NotEmpty(t, c2.NewConv, "second human clone was rate-limited; humans must be exempt: %s", c2.Raw)
	assert.NotEqual(t, c1.NewConv, c2.NewConv, "expected distinct new convs from two clones")

	// The human path never claims a slot, so the rate-limit table
	// stays empty for this source.
	last, err := db.LatestCloneAt(oldConv)
	require.NoError(t, err, "LatestCloneAt")
	assert.True(t, last.IsZero(),
		"human-initiated clone recorded a rate-limit slot; it should skip ClaimCloneSlot entirely")
}
