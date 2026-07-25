package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: cloning an agent WITH a follow-up. Post-TCL-732 the clone's title
// and its first turn ride in as LAUNCH ARGS rather than as two tmux send-keys
// streams into a pane whose input readiness the daemon cannot observe.
//
// This is the --no-copy-conv branch, where the clone's conv-id is preset with
// `--session-id` exactly as a reincarnated successor's is.
//
// Expected: the clone is named at launch, the follow-up is its launch prompt
// (inlined — well under the cap) while still being archived in its inbox as a
// delivered message, and NOTHING is typed into its pane.
func TestClone_HandoffRidesLaunchArgs_NoCopy(t *testing.T) {
	f := newFlow(t)

	const oldConv = "chf0-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-chf0-001"
	const oldTmux = "tclaude-spwn-chf0-001"
	const handoff = "PROBE-CLONE-HANDOFF: pick up the merge conflict work"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    handoff,
	})
	require.Equal(t, http.StatusOK, c.Code, "clone: body=%s", c.Raw)

	f.AssertSpawnName(c.NewConv, "worker-c-1", 10*time.Second)
	f.AssertSpawnInitialPrompt(c.NewConv, handoff, 10*time.Second)

	// The inbox copy exists and is already accounted for: an inlined handoff is
	// born delivered, so it never re-enters the nudge queue as a duplicate.
	assertHandoffDelivered(t, cloneHandoffMessageIDFor(t, c.NewConv))

	// The title the human sees is the clean derived form, and it got there
	// without a keystroke.
	f.AssertCloneTitle(c, "alpha", "worker-c-1", 10*time.Second)
	assertNoSendKeysTo(t, f, c.TmuxTarget())
}

// Scenario: the same, on the DEFAULT copy branch — the clone forks its source's
// jsonl and resumes into it. This is the branch that needed `claude --resume`
// to accept a launch `--name` and a positional prompt at all; it is covered
// separately from the no-copy branch because the two reach different spawners
// (SpawnDetachedTclaudeResume vs SpawnDetachedTclaudeNew) and different argv
// builders (sessionResumeArgs vs sessionNewArgs).
func TestClone_HandoffRidesLaunchArgs_CopyPath(t *testing.T) {
	f := newFlow(t)

	// A full 36-char conv-id, unlike the short stand-ins the other flow tests
	// use: convops's project scan skips any .jsonl whose basename isn't
	// UUID-length, so the fork this branch performs would find nothing.
	const oldConv = "c1f1aaaa-bbbb-cccc-dddd-eeeeffff0001"
	const oldLabel = "spwn-chf1-001"
	const oldTmux = "tclaude-spwn-chf1-001"
	const handoff = "PROBE-CLONE-HANDOFF: continue from the copied context"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{"follow_up": handoff})
	require.Equal(t, http.StatusOK, c.Code, "clone: body=%s", c.Raw)
	require.True(t, c.CopyConv, "this test must exercise the copy branch")

	f.AssertSpawnName(c.NewConv, "worker-c-1", 10*time.Second)
	f.AssertSpawnInitialPrompt(c.NewConv, handoff, 10*time.Second)
	assertHandoffDelivered(t, cloneHandoffMessageIDFor(t, c.NewConv))
	assertNoSendKeysTo(t, f, c.TmuxTarget())
}

// Scenario: a clone pane that never accepts a keystroke for the whole test.
//
// Expected: the clone is named and briefed anyway, because neither travels over
// tmux. The title is EXACTLY the derived form — not the derived form with the
// handoff nudge welded onto it, which is what the pre-TCL-732 path produced.
func TestClone_UnreadyPane_TitleAndHandoffStillLand(t *testing.T) {
	f := newFlow(t)
	f.World.SpawnInputUnreadyEnters = neverReadyPaneEnters

	const oldConv = "chf2-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-chf2-001"
	const oldTmux = "tclaude-spwn-chf2-001"
	const handoff = "PROBE-CLONE-MARKER: finish the audit in pkg/bar"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    handoff,
	})
	require.Equal(t, http.StatusOK, c.Code, "clone: body=%s", c.Raw)

	f.AssertSpawnName(c.NewConv, "worker-c-1", 10*time.Second)
	f.AssertSpawnInitialPrompt(c.NewConv, handoff, 10*time.Second)

	// Read the title the way production reads it: the .jsonl custom-title turn
	// `claude --name` writes at startup.
	title := successorPaneTitle(t, f, c.NewConv, "worker-c-1", 10*time.Second)
	assert.Equal(t, "worker-c-1", title,
		"an unready pane must not merge the handoff into the clone's title")

	assertHandoffDelivered(t, cloneHandoffMessageIDFor(t, c.NewConv))
	assertNoSendKeysTo(t, f, c.TmuxTarget())
}

// Scenario: the SAME unready pane, but with the operator's escape hatch
// (agent.spawn_legacy_injection=true) reverting clone to the legacy
// inject-after-connect path — which is also the path Codex still takes.
//
// This is the characterisation test for the bug: it pins that the simulator
// really does reproduce the merge, so the immunity asserted above is a property
// of the launch-arg path and not an artifact of a forgiving simulator.
//
// It replaces TestClone_FollowUpNudgeDoesNotCorruptTitle, which asserted the
// weaker ordering guarantee the settle gap gives: rename FIRST, nudge second,
// both typed. That ordering holds only while the pane is reading input — which
// is exactly the assumption this scenario removes.
func TestClone_LegacyInjection_UnreadyPaneMergesRenameAndHandoff(t *testing.T) {
	f := newFlow(t)
	f.World.SpawnInputUnreadyEnters = unreadyPaneEnters

	legacy := true
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLegacyInjection: &legacy},
	}))

	const oldConv = "chf3-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-chf3-001"
	const oldTmux = "tclaude-spwn-chf3-001"
	const handoff = "PROBE-CLONE-MARKER: finish the audit in pkg/bar"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    handoff,
	})
	require.Equal(t, http.StatusOK, c.Code, "clone: body=%s", c.Raw)

	// Legacy path: the title and the handoff are both typed into the pane...
	f.AssertSentContains(c.TmuxTarget(), "/rename worker-c-1", 10*time.Second)
	f.AssertSentContains(c.TmuxTarget(), "new agent message", 15*time.Second)

	// ...and against a pane that is not reading yet, they arrive as ONE line:
	// the clone's title becomes the derived name with the whole nudge welded
	// onto it, so the handoff is never delivered as a turn.
	merged := successorPaneTitleMatching(t, f, c.NewConv,
		func(s string) bool { return strings.Contains(s, "new agent message") },
		15*time.Second)
	assert.Truef(t, strings.HasPrefix(merged, "worker-c-1"),
		"expected the merged title to start with the derived name; got %q", merged)
	assert.NotEqual(t, "worker-c-1", merged,
		"this test exists to pin the MERGE; if the legacy path stopped merging, "+
			"the simulator's unready-pane model has regressed")
}

// Scenario: a clone with NO follow-up. There is nothing for it to submit as a
// first turn, and `claude --session-id <id> --name <n>` with no positional
// prompt writes no transcript at all — the conversation materialises on its
// first turn. So such a clone deliberately stays on the post-connect /rename,
// which forces that turn and gives the clone a .jsonl to be found by.
//
// Expected: the clone is renamed over tmux, exactly as before TCL-732, and no
// launch name is applied. Nothing is lost by staying here: with one injected
// stream there is no second stream to merge with.
func TestClone_NoFollowUp_KeepsPostConnectRename(t *testing.T) {
	f := newFlow(t)

	const oldConv = "chf4-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-chf4-001"
	const oldTmux = "tclaude-spwn-chf4-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneFresh(oldConv)

	f.AssertSentContains(c.TmuxTarget(), "/rename worker-c-1", 10*time.Second)
	f.AssertCloneTitle(c, "alpha", "worker-c-1", 10*time.Second)

	if name, ok := f.World.SpawnName(c.NewConv); ok {
		assert.Empty(t, name,
			"a follow-up-less clone must not be launch-enrolled: a name-only launch writes no transcript")
	}
}

// Scenario: a follow-up too long to inline in the launch command.
//
// Expected: the clone is still launch-enrolled, but its prompt POINTS at the
// inbox copy by id instead of carrying the body — the same inline-vs-pointer
// rule spawn and reincarnate use. The row is marked delivered (the launch
// prompt announced it) but NOT read (the clone still has to fetch it).
func TestClone_OverCapHandoff_RidesAsInboxPointer(t *testing.T) {
	f := newFlow(t)

	const oldConv = "chf5-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-chf5-001"
	const oldTmux = "tclaude-spwn-chf5-001"
	handoff := "OVER-CAP-CLONE-MARKER " + strings.Repeat("x", 4000)

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveEnrolledAgent(oldConv)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    handoff,
	})
	require.Equal(t, http.StatusOK, c.Code, "clone: body=%s", c.Raw)

	msgID := cloneHandoffMessageIDFor(t, c.NewConv)
	f.AssertSpawnInitialPrompt(c.NewConv, "inbox read", 10*time.Second)

	prompt, ok := f.World.SpawnInitialPrompt(c.NewConv)
	require.True(t, ok, "no launch prompt recorded for %s", c.NewConv)
	assert.NotContains(t, prompt, "OVER-CAP-CLONE-MARKER",
		"an over-cap handoff must not be inlined into the launch command")

	assertHandoffDelivered(t, msgID)
	m, err := db.GetAgentMessage(msgID)
	require.NoError(t, err)
	assert.True(t, m.ReadAt.IsZero(),
		"a pointer handoff is announced, not consumed; the clone still has to read it")
}

// Scenario: launch metadata lands but the clone's pane dies at startup. A
// preset conv-id or copied jsonl is not proof that a harness survived.
//
// Expected on both launch-enrolled branches: clone returns a timeout rather
// than handing the caller a dead sibling, and the pre-fork handoff row is
// rolled back.
func TestClone_DeadPaneDoesNotReportLaunchEnrolledClone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		oldConv    string
		noCopyConv bool
	}{
		{name: "no-copy", oldConv: "chf6aaaa-bbbb-cccc-dddd-eeeeffff0001", noCopyConv: true},
		{name: "copy", oldConv: "c1f6aaaa-bbbb-cccc-dddd-eeeeffff0002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(50 * time.Millisecond))
			f := newFlow(t)

			f.HaveConvWithTitle(tc.oldConv, "worker")
			f.HaveEnrolledAgent(tc.oldConv)
			f.HaveAliveSession(tc.oldConv, "spwn-"+tc.name, "tclaude-"+tc.name, f.TestCwd("work"))
			g := f.HaveGroup("alpha")
			f.HaveMember("alpha", tc.oldConv)
			require.NoError(t, db.AddAgentGroupOwner(g.ID, tc.oldConv, "test"))
			f.World.SpawnPaneDiesAtLaunch = true

			c := f.AsAgent(tc.oldConv).CloneWith(tc.oldConv, map[string]any{
				"no_copy_conv": tc.noCopyConv,
				"follow_up":    "PROBE-DEAD-CLONE: must be rolled back",
			})
			require.Equalf(t, http.StatusGatewayTimeout, c.Code,
				"a clone whose pane died must not report success; body=%s", c.Raw)

			outbox, err := db.ListAgentMessagesFromConv(tc.oldConv, 50)
			require.NoError(t, err)
			for _, m := range outbox {
				assert.NotEqual(t, agentd.CloneHandoffSubject, m.Subject,
					"the pre-fork handoff must be rolled back when launch fails")
			}
		})
	}
}

// cloneHandoffMessageIDFor finds the clone-handoff row addressed to the new
// clone. Polls: the orchestration inserts it before responding, but the test
// reads through the same DB the daemon writes.
func cloneHandoffMessageIDFor(t *testing.T, newConv string) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := db.ListAgentMessagesForConv(newConv, 50)
		require.NoError(t, err)
		for _, m := range msgs {
			if m.Subject == agentd.CloneHandoffSubject {
				return m.ID
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no clone handoff message addressed to %s", newConv)
	return 0
}
