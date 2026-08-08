package agentd_test

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-1080, at the surface an operator would see it: an API-drive Copilot spawn
// whose bootstrap never completes gets neither its rename nor its welcome, and
// therefore comes up nameless and unbriefed while looking, on every dashboard
// surface, exactly like an agent that read its brief and had nothing to say.
//
// # Why these flows set the legacy-injection revert
//
// Because that is where runSpawnPostInit is reachable for Copilot, and it is
// worth being precise rather than leaving the config line looking incidental.
// Copilot's harness declares LaunchEnrollment, so a DEFAULT Copilot spawn
// returns from executeSpawn's launch-enrollment branch before
// finishSpawnEnrollment — the only caller of runSpawnPostInit — is reached; its
// rename and welcome are launch arguments instead. Measured, not inferred: with
// the wait counted and the revert off, a --copilot-api spawn calls it zero
// times. agent.spawn_legacy_injection is the operator escape hatch that turns
// launch enrollment off, and under it the post-init path is exactly the one
// below.
//
// That narrows who this reaches; it does not soften it. An operator sets the
// escape hatch precisely because the launch-arg path misbehaved for them, so a
// defect reachable only through the fallback fires for someone who has already
// run out of options and will read the resulting silence as the fallback
// failing too.
func haveLegacySpawnInjection(t *testing.T) {
	t.Helper()
	legacy := true
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLegacyInjection: &legacy},
	}))
}

// countingPostInitWait installs a post-init wait that answers `came` and counts
// its calls. agentd's TestMain installs a binary-wide "the channel never came
// up", which is the truthful answer when the bootstrap is stubbed out; these
// flows want the count as well, and one of them wants the opposite answer.
func countingPostInitWait(t *testing.T, came bool) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	t.Cleanup(agentd.SetCopilotAPIPostInitWaitForTest(func(string) bool {
		calls.Add(1)
		return came
	}))
	return &calls
}

// The headline: a channel that never comes up must still leave the agent named
// and pointed at its briefing.
//
// Every assertion here is a POSITIVE fact — a keystroke that was sent — which
// is deliberate. The obvious way to write this test is "no RPC was attempted",
// and that is an absence: it is satisfied just as well by a post-init that did
// nothing at all, which is the defect. What separates the fix from the defect
// is that text ARRIVED.
func TestCopilotDrive_APISpawnWhoseChannelNeverCameUpIsStillNamedAndBriefed(t *testing.T) {
	f := newCopilotFlow(t)
	haveLegacySpawnInjection(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)
	waitCalls := countingPostInitWait(t, false)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"harness": harness.CopilotName, "name": "copilot-worker",
		"copilot_api": true, "profile": copilotAPIProfile,
		"initial_message": "Audit the auth module",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "copilot spawn body=%s", spawn.Raw)

	target := spawn.TmuxTarget()
	msg := soleInboxMessage(t, spawn.ConvID)

	// The rename. Before the fix this was refused twice over: deliverRename
	// routed it to the API channel because the launch posture says API, and —
	// once that was corrected only at the top — dispatchSlashCommand refused it
	// again with "lifecycle command has no typed RPC mapping".
	f.AssertSentContains(target, "/rename copilot-worker", 10*time.Second)
	// The welcome, which is the whole point: it names the inbox message the
	// briefing is sitting in. Without it the agent never learns the briefing
	// exists.
	f.AssertSentContains(target, fmt.Sprintf("inbox read %d", msg.ID), 10*time.Second)

	assert.Equal(t, int64(1), waitCalls.Load(),
		"post-init must ASK whether the channel came up; a spawn that never asks "+
			"is the state this ticket is about, and it looks identical from here "+
			"unless the count is checked")
}

// The mirror, and the arm that keeps the fallback from becoming the rule: when
// the channel DID come up, nothing is typed. The pane exemption is for a
// channel that never arrived, not for an API-driven agent in general.
//
// # What this covers, and what it does not
//
// Stated rather than left to be discovered, because the gap is exactly the
// shape that turns a test into a certificate. This flow has no Copilot server
// behind it, so "the channel came up" is the stubbed wait's answer and not a
// real connection: the deliveries below therefore fail at copilotAPIDrive and
// are REPORTED, which is TCL-1058's rule and is the property being pinned. What
// it does NOT show is the successful typed delivery — that needs a live handle,
// and it is pinned at the seam itself by
// TestTheCopilotPaneOverrideChangesTheChannelAndNothingElseDoes, which runs
// against a real client on a fake server.
//
// The wait-count assertion is this test's positive control. Without it the
// keystroke assertion is a pure absence and a post-init goroutine that never
// fired would satisfy it.
func TestCopilotDrive_APISpawnWithAChannelThatCameUpIsNotTypedInto(t *testing.T) {
	f := newCopilotFlow(t)
	haveLegacySpawnInjection(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)
	waitCalls := countingPostInitWait(t, true)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"harness": harness.CopilotName, "name": "copilot-worker",
		"copilot_api": true, "profile": copilotAPIProfile,
		"initial_message": "Audit the auth module",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "copilot spawn body=%s", spawn.Raw)

	// The positive control this test needs, and the reason it is not a bare
	// absence: post-init must have RUN and reached its channel question. With
	// only the keystroke assertion below, a spawn whose post-init goroutine
	// never fired at all would pass.
	require.Eventually(t, func() bool { return waitCalls.Load() == 1 },
		10*time.Second, 20*time.Millisecond,
		"post-init never reached the channel question")
	// The deliveries then fail against a registry with no handle (this flow has
	// no Copilot server behind it) and are REPORTED, never typed. That refusal
	// is TCL-1058's rule and this change does not touch it.
	assertNoKeystrokesTo(t, f, spawn.TmuxTarget())
}

// The operator constraint, pinned: the API drive is opt-in, both paths stay,
// and a Copilot agent that never asked for the drive must behave exactly as it
// did before this change existed.
//
// The wait-count assertion is the sharp half. "The rename and welcome were
// typed" would pass even if a send-keys spawn had started blocking for 90
// seconds on an API channel it never asked for; the count says the question was
// never even asked.
func TestCopilotDrive_ASendKeysSpawnNeverWaitsForAnAPIChannel(t *testing.T) {
	f := newCopilotFlow(t)
	haveLegacySpawnInjection(t)
	f.HaveGroup("crew")
	waitCalls := countingPostInitWait(t, false)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"harness": harness.CopilotName, "name": "copilot-worker",
		"initial_message": "Audit the auth module",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "copilot spawn body=%s", spawn.Raw)

	target := spawn.TmuxTarget()
	msg := soleInboxMessage(t, spawn.ConvID)
	f.AssertSentContains(target, "/rename copilot-worker", 10*time.Second)
	f.AssertSentContains(target, fmt.Sprintf("inbox read %d", msg.ID), 10*time.Second)

	assert.Equal(t, int64(0), waitCalls.Load(),
		"a Copilot spawn that did not ask for the API drive must not consult the "+
			"API channel at all")
}

// The ordering the pane fallback's safety rests on, pinned rather than
// observed.
//
// Falling back to the pane would be a gamble if the bootstrap could still
// foreground a fresh session over the pane's afterwards — the welcome would
// land in a session about to be replaced, which is the loss the wait exists to
// prevent, arriving through the remedy. It cannot, and the reason is an
// ordering: both deadlines are copilotAPIBootstrapTimeout(), and the
// bootstrap's starts first (at completeCopilotAPILaunch, inside the spawn
// facade) while the wait's starts later (after post-init has waited for the
// pane to come alive). So the bootstrap's context is already cancelled when the
// wait expires.
//
// This pins the half that can move: the START ORDER. The other half — that the
// two budgets are the same number — is one function with one caller each, and
// TestTheSpawnPostInitWaitGivesUpAtTheBootstrapsBudget pins the wait's use of
// it.
func TestCopilotDrive_ThePostInitWaitStartsAfterTheBootstrapDoes(t *testing.T) {
	f := newCopilotFlow(t)
	haveLegacySpawnInjection(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	bootstrapAt := make(chan time.Time, 4)
	t.Cleanup(agentd.SetCopilotAPIBootstrapForTest(
		func(convID string, copilotAPI bool, resume bool, initialPrompt string) {
			if copilotAPI {
				bootstrapAt <- time.Now()
			}
		}))
	waitAt := make(chan time.Time, 4)
	t.Cleanup(agentd.SetCopilotAPIPostInitWaitForTest(func(string) bool {
		waitAt <- time.Now()
		return false
	}))

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"harness": harness.CopilotName, "name": "copilot-worker",
		"copilot_api": true, "profile": copilotAPIProfile,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "copilot spawn body=%s", spawn.Raw)

	var started, waited time.Time
	select {
	case started = <-bootstrapAt:
	case <-time.After(10 * time.Second):
		t.Fatal("the API bootstrap was never kicked off for an API-drive launch")
	}
	select {
	case waited = <-waitAt:
	case <-time.After(15 * time.Second):
		t.Fatal("post-init never waited for the API channel")
	}
	assert.False(t, waited.Before(started),
		"the post-init wait started BEFORE the bootstrap did, so its equal budget "+
			"no longer outlives the bootstrap's context and the pane fallback can "+
			"race a setForeground that replaces the session it typed into")
}

// assertNoKeystrokesTo is the "and nothing was typed" half, scoped to one
// pane's target so an unrelated pane in the same flow cannot satisfy it.
func assertNoKeystrokesTo(t *testing.T, f *testharness.Flow, target string) {
	t.Helper()
	// Long enough for post-init to have finished both deliveries; it has
	// nothing to wait on once the (stubbed) channel question is answered.
	time.Sleep(2 * time.Second)
	for _, sent := range f.World.Tmux.Sent() {
		assert.NotEqualf(t, target, sent.Target,
			"an API-connected agent must not be typed into: %+v", sent)
	}
}
