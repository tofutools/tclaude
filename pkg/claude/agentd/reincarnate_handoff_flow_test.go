package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// unreadyPaneEnters models a successor pane that is still starting up when the
// first injection arrives: it swallows the two Enters of one injectTextAndSubmit
// call before its TUI begins reading. That is the marginal-but-not-rare
// production case — the measured gap between Claude Code's SessionStart hook and
// its input box accepting keystrokes was 0.50–0.72s against a ~1s injection
// margin, so anything that slows startup closes it.
const unreadyPaneEnters = 2

// neverReadyPaneEnters models a pane that never accepts a keystroke for the
// whole scenario — more Enters than any injection path sends. Used to show the
// launch-arg path does not depend on pane readiness at all.
const neverReadyPaneEnters = 32

// Scenario: the whole point of a reincarnation's REQUIRED follow_up is that the
// successor does not come up idle. Post-TCL-731 the successor's title and its
// first turn ride in as LAUNCH ARGS (`claude --session-id/--name/[prompt]`)
// rather than as two tmux send-keys streams.
//
// Action: reincarnate a grouped agent with a follow-up.
//
// Expected: the successor is named at launch, the handoff is its launch prompt
// (inlined — this one is far under the inline cap) while still being archived
// in its inbox as a delivered message, and NOTHING is typed into its pane.
func TestReincarnate_HandoffRidesLaunchArgs_Grouped(t *testing.T) {
	f := newFlow(t)

	const oldConv = "hof0-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof0-001"
	const oldTmux = "tclaude-spwn-hof0-001"
	const handoff = "PROBE-HANDOFF: continue the migration in pkg/foo"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, handoff)

	f.AssertSpawnName(r.NewConv, "worker", 10*time.Second)
	f.AssertSpawnInitialPrompt(r.NewConv, handoff, 10*time.Second)

	// The inbox copy exists, names itself in the launch prompt, and is already
	// delivered — the agent has the text, so it must never be nudged again.
	msgID := handoffMessageIDFor(t, r.NewConv)
	prompt, _ := f.World.SpawnInitialPrompt(r.NewConv)
	assert.Contains(t, prompt, fmt.Sprintf("message #%d", msgID),
		"the inlined handoff should note its inbox copy by id")
	assertHandoffDelivered(t, msgID)

	// Nothing was typed into the successor's pane — the whole point of the
	// launch-arg path. (The PREDECESSOR's pane still gets its archive rename
	// and /exit, so scope the check to the successor's target.)
	assertNoSendKeysTo(t, f, r.TmuxTarget())
}

// Scenario: the same, for a SOLO (groupless) agent. The handoff row is inserted
// with group_id 0 — the direct-message transport — so a solo successor must
// still get one. This is the shape a human's single long-lived agent has.
func TestReincarnate_HandoffRidesLaunchArgs_Solo(t *testing.T) {
	f := newFlow(t)

	const oldConv = "hof1-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof1-001"
	const oldTmux = "tclaude-spwn-hof1-001"
	const handoff = "PROBE-SOLO-HANDOFF: pick up where you left off"

	f.HaveConvWithTitle(oldConv, "solo")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, handoff)

	f.AssertSpawnName(r.NewConv, "solo", 10*time.Second)
	f.AssertSpawnInitialPrompt(r.NewConv, handoff, 10*time.Second)

	msgID := handoffMessageIDFor(t, r.NewConv)
	assertHandoffDelivered(t, msgID)
	assertNoSendKeysTo(t, f, r.TmuxTarget())
}

// Scenario (TCL-731, the regression this whole change exists for): the
// successor's pane takes seconds longer than usual to start reading input — a
// loaded box, extra MCP servers, a cold page cache. A pre-TUI tty buffers
// literal text but DROPS the Enter keypresses, so any injected `/rename` and
// handoff nudge would replay as ONE merged line.
//
// Expected: the launch-arg path is immune. The successor still comes up with
// the correct base title and the handoff as its first turn, because argv cannot
// be dropped, merged, or mistimed — and its title is emphatically NOT the
// handoff text.
func TestReincarnate_UnreadyPane_TitleAndHandoffStillLand(t *testing.T) {
	f := newFlow(t)
	f.World.SpawnInputUnreadyEnters = neverReadyPaneEnters

	const oldConv = "hof2-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof2-001"
	const oldTmux = "tclaude-spwn-hof2-001"
	const handoff = "PROBE-HANDOFF-MARKER: finish the audit in pkg/bar"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, handoff)

	f.AssertSpawnName(r.NewConv, "worker", 10*time.Second)
	f.AssertSpawnInitialPrompt(r.NewConv, handoff, 10*time.Second)

	// The successor's own title, read the way production reads it (the .jsonl
	// custom-title turn `claude --name` writes at startup), is the clean base
	// name — not "worker" with the handoff welded onto it.
	title := successorPaneTitle(t, f, r.NewConv, "worker", 10*time.Second)
	assert.Equal(t, "worker", title,
		"an unready pane must not merge the handoff into the successor's title")

	assertHandoffDelivered(t, handoffMessageIDFor(t, r.NewConv))
	assertNoSendKeysTo(t, f, r.TmuxTarget())
}

// Scenario: the SAME unready pane, but with the operator's escape hatch
// (agent.spawn_legacy_injection=true) reverting reincarnate to the legacy
// inject-after-connect path — which is also the path Codex still takes.
//
// This is the characterisation test for the bug: it pins that the simulator
// really does reproduce the merge, so the immunity asserted above is a property
// of the launch-arg path and not an artifact of a forgiving simulator. The
// successor ends up titled `<base><the entire handoff nudge>` and the handoff
// is consumed as rename argument text rather than delivered as a turn.
//
// Keeping this failure documented in a test is deliberate: the revert flag is
// still reachable, and an operator who flips it should be able to find out from
// the test suite what they are trading away.
func TestReincarnate_LegacyInjection_UnreadyPaneMergesRenameAndHandoff(t *testing.T) {
	f := newFlow(t)
	f.World.SpawnInputUnreadyEnters = unreadyPaneEnters

	legacy := true
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLegacyInjection: &legacy},
	}))

	const oldConv = "hof3-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof3-001"
	const oldTmux = "tclaude-spwn-hof3-001"
	const handoff = "PROBE-HANDOFF-MARKER: finish the audit in pkg/bar"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, handoff)

	// Legacy path: the title and the handoff are both typed into the pane...
	f.AssertSentContains(r.TmuxTarget(), "/rename worker", 10*time.Second)
	f.AssertSentContains(r.TmuxTarget(), "PROBE-HANDOFF-MARKER", 15*time.Second)

	// ...and against a pane that is not reading yet, they arrive as ONE line:
	// the successor's title becomes the base name with the whole handoff nudge
	// welded onto it, so the handoff is never delivered as a turn.
	merged := successorPaneTitleMatching(t, f, r.NewConv,
		func(s string) bool { return strings.Contains(s, "PROBE-HANDOFF-MARKER") },
		15*time.Second)
	assert.Truef(t, strings.HasPrefix(merged, "worker"),
		"expected the merged title to start with the base name; got %q", merged)
	assert.NotEqual(t, "worker", merged,
		"this test exists to pin the MERGE; if the legacy path stopped merging, "+
			"the simulator's unready-pane model has regressed")
}

// Scenario: the successor's harness dies on startup — expired auth, a broken
// `claude` install, a failing MCP server. `tclaude session new` writes the
// session row BEFORE it creates the tmux session, and on the launch-enrollment
// path that row is born carrying the preset conv-id, so neither the row nor the
// id it holds is proof that anything is running.
//
// Expected: reincarnate must NOT mistake that for success. It is destructive —
// it archives and `/exit`-s the predecessor — so a dead successor has to land on
// the timeout branch, leaving the predecessor alive with its identity intact.
func TestReincarnate_DeadSuccessorPane_DoesNotDecommissionPredecessor(t *testing.T) {
	t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(50 * time.Millisecond))
	f := newFlow(t)

	const oldConv = "hof4-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof4-001"
	const oldTmux = "tclaude-spwn-hof4-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	// The row lands; the pane does not.
	f.World.SpawnPaneDiesAtLaunch = true

	r := f.AsHuman().ReincarnateWith(oldConv,
		map[string]any{"follow_up": "PROBE-DEAD-PANE: should never be delivered"})
	require.Equalf(t, http.StatusGatewayTimeout, r.Code,
		"a successor whose harness died must not report success; body=%s", r.Raw)

	// The predecessor is untouched: still the group's live member, never
	// archive-renamed, never /exit-ed.
	f.AssertGroupMember(g.Name, oldConv, "worker", 10*time.Second)
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContainsf(t, sk.Text, "/exit",
			"a failed reincarnation must not soft-stop the predecessor; sent=%+v", f.World.Tmux.Sent())
		assert.NotContainsf(t, sk.Text, "worker-x",
			"a failed reincarnation must not archive-rename the predecessor; sent=%+v", f.World.Tmux.Sent())
	}
	row, err := db.GetConvIndex(oldConv)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.False(t, row.IsArchived(), "the predecessor must not be archived")

	// And the pre-fork handoff row was rolled back rather than left addressed
	// to a conv-id that will never exist.
	msgs, err := db.ListAgentMessagesForConv(r.NewConv, 50)
	require.NoError(t, err)
	for _, m := range msgs {
		assert.NotEqual(t, db.ReincarnationHandoffSubject, m.Subject,
			"the pre-fork handoff must be rolled back when the launch fails")
	}
}

// Scenario: a handoff too long to inline. It stays in the inbox and the launch
// prompt points at it by id — the same inline-vs-pointer rule spawn briefings
// follow. The pointer copy must be marked delivered (the successor was told
// where it is) but NOT read (it still has to open it), and still nothing is
// typed into the pane.
func TestReincarnate_OverCapHandoff_RidesAsInboxPointer(t *testing.T) {
	f := newFlow(t)

	const oldConv = "hof5-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-hof5-001"
	const oldTmux = "tclaude-spwn-hof5-001"
	// Comfortably over config.DefaultSpawnInlineMaxChars (2000 runes).
	handoff := "PROBE-OVERCAP: " + strings.Repeat("x", 4000)

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, handoff)

	msgID := handoffMessageIDFor(t, r.NewConv)
	f.AssertSpawnInitialPrompt(r.NewConv, fmt.Sprintf("inbox read %d", msgID), 10*time.Second)
	prompt, _ := f.World.SpawnInitialPrompt(r.NewConv)
	assert.NotContains(t, prompt, handoff,
		"an over-cap handoff stays in the inbox rather than ballooning the launch command")

	assertHandoffDelivered(t, msgID)
	m, err := db.GetAgentMessage(msgID)
	require.NoError(t, err)
	assert.True(t, m.ReadAt.IsZero(),
		"a pointer handoff stays UNREAD — the successor still has to open it")

	assertNoSendKeysTo(t, f, r.TmuxTarget())
}

// Scenario: an agent reincarnated TWICE. Each generation's handoff row is
// inserted before its successor is linked to the actor, so without the
// post-rotation re-derive it would carry an empty to_agent and fall out of the
// actor-keyed inbox as soon as the agent rotated again.
//
// Expected: after the second reincarnation, BOTH handoffs are still readable
// from the live generation's actor inbox.
func TestReincarnate_HandoffsStayInTheActorInboxAcrossGenerations(t *testing.T) {
	f := newFlow(t)

	const gen0 = "hof6-aaaa-bbbb-cccc-dddd"
	const gen0Label = "spwn-hof6-001"
	const gen0Tmux = "tclaude-spwn-hof6-001"

	f.HaveConvWithTitle(gen0, "worker")
	f.HaveEnrolledAgent(gen0)
	f.HaveAliveSession(gen0, gen0Label, gen0Tmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", gen0)

	first := f.AsHuman().Reincarnate(gen0, "PROBE-GEN1: first handoff")
	firstMsg := handoffMessageIDFor(t, first.NewConv)

	// The successor's own pane is what the second reincarnation retires.
	second := f.AsHuman().Reincarnate(first.NewConv, "PROBE-GEN2: second handoff")
	secondMsg := handoffMessageIDFor(t, second.NewConv)
	require.NotEqual(t, firstMsg, secondMsg, "each generation gets its own handoff row")

	agentID, err := db.AgentIDForConv(second.NewConv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "the live generation resolves to the stable actor")

	inbox, err := db.ListInboxForActor(second.NewConv, agentID, 100)
	require.NoError(t, err)
	seen := map[int64]bool{}
	for _, m := range inbox {
		seen[m.ID] = true
	}
	assert.Truef(t, seen[firstMsg],
		"generation 1's handoff must survive the second rotation in the actor inbox; got %+v", inbox)
	assert.True(t, seen[secondMsg], "generation 2's handoff is in the inbox")
}

// handoffMessageIDFor finds the reincarnation handoff row addressed to the
// successor conv. Polls: the orchestration inserts it before responding, but
// the test reads through the same DB the daemon writes.
func handoffMessageIDFor(t *testing.T, newConv string) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := db.ListAgentMessagesForConv(newConv, 50)
		require.NoError(t, err)
		for _, m := range msgs {
			if m.Subject == db.ReincarnationHandoffSubject {
				return m.ID
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no reincarnation handoff message addressed to %s", newConv)
	return 0
}

func assertHandoffDelivered(t *testing.T, msgID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := db.GetAgentMessage(msgID)
		require.NoError(t, err)
		if !m.DeliveredAt.IsZero() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	m, _ := db.GetAgentMessage(msgID)
	assert.Fail(t, "handoff message never marked delivered",
		"msg #%d state=%+v", msgID, m)
}

// assertNoSendKeysTo fails if anything was ever typed into the given tmux
// target. Scoped to one target because the PREDECESSOR's pane legitimately
// still receives its archive rename and /exit over send-keys.
func assertNoSendKeysTo(t *testing.T, f *testharness.Flow, target string) {
	t.Helper()
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotEqualf(t, target, sk.Target,
			"launch-enrolled successor must not be send-keys'd; got %q", sk.Text)
	}
}

// successorPaneTitle waits for the successor's simulated pane to hold `want` as
// its title and returns whatever it actually settled on, so the caller can
// assert on the mismatch rather than on a bare timeout.
func successorPaneTitle(t *testing.T, f *testharness.Flow, convID, want string, timeout time.Duration) string {
	t.Helper()
	return successorPaneTitleMatching(t, f, convID, func(s string) bool { return s == want }, timeout)
}

// successorPaneTitleMatching polls the successor's CCSim title until pred holds
// (or the timeout expires) and returns the last value seen. The CCSim writes
// its title the same way real Claude Code does — a custom-title turn on the
// .jsonl, written by `--name` at launch or by an injected /rename.
func successorPaneTitleMatching(t *testing.T, f *testharness.Flow, convID string, pred func(string) bool, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if cc := f.World.CCs.GetByConvID(convID); cc != nil {
			last = cc.Title()
			if pred(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
}
