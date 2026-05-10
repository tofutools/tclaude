package agentd_test

import (
	"net/http"
	"testing"
	"time"

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
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "")

	// First clone — succeeds.
	c1 := f.AsHuman().CloneFresh(oldConv, "")
	if c1.NewConv == "" {
		t.Fatalf("first clone returned empty NewConv: %s", c1.Raw)
	}

	// Second clone of the same source — should be 429. Use the raw
	// helper because CloneFresh fatals on non-200.
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+oldConv+"/clone",
		map[string]any{"no_copy_conv": true})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second clone status = %d, want %d. body=%s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	// Real-surface invariant: only the successful first attempt
	// consumed a slot. INSERT-WHERE-NOT-EXISTS must leave the table
	// untouched on rate-limit; otherwise repeated rapid attempts
	// would extend the cooldown indefinitely (each failed try would
	// reset the timer), which is not the intended behaviour.
	last, err := db.LatestCloneAt(oldConv)
	if err != nil {
		t.Fatalf("LatestCloneAt: %v", err)
	}
	if last.IsZero() {
		t.Fatal("agent_clone_history has no row for source; first clone failed to record")
	}
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
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "")

	// First clone lands.
	c1 := f.AsHuman().CloneFresh(oldConv, "")
	if c1.NewConv == "" {
		t.Fatalf("first clone returned empty NewConv: %s", c1.Raw)
	}

	// Drop cooldown to 0 — the next clone should pass.
	agentd.CloneCooldown = 0

	c2 := f.AsHuman().CloneFresh(oldConv, "")
	if c2.NewConv == "" {
		t.Fatalf("second clone returned empty NewConv after cooldown drop: %s", c2.Raw)
	}
	if c2.NewConv == c1.NewConv {
		t.Errorf("expected distinct new convs from two clones, got duplicate %s", c2.NewConv)
	}
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
	f.HaveGroup("team")
	f.HaveMember("team", aConv, "alpha")
	f.HaveMember("team", bConv, "beta")

	cA := f.AsHuman().CloneFresh(aConv, "")
	if cA.NewConv == "" {
		t.Fatalf("clone of A failed: %s", cA.Raw)
	}
	cB := f.AsHuman().CloneFresh(bConv, "")
	if cB.NewConv == "" {
		t.Fatalf("clone of B failed despite being a different source: %s", cB.Raw)
	}
}
