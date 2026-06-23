package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These tests pin the follow-up handling for clone / reincarnate
// handoffs. Since the universal inbox, EVERY successor — grouped or
// solo — has an inbox, so every handoff rides it as an agent_messages
// row: a long, multi-line brief is accepted verbatim regardless of
// group membership. A solo handoff is simply a direct message
// (group_id 0). Only the lenient 16384-byte inbox cap still bounds it.

// Scenario: a grouped worker is reincarnated with a real handoff brief
// — multi-paragraph, well over the retired 4096-byte follow-up cap.
//
// Expected: accepted. The brief lands in the successor's inbox as the
// "reincarnation handoff" message, verbatim — newlines and all.
func TestReincarnate_GroupedHandoffAcceptsLargeMultiLineBrief(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const oldConv = "old-aaaa-bbbb-cccc-dddd"
		const oldLabel = "spwn-old-001"
		const oldTmux = "tclaude-spwn-old-001"

		f.HaveConvWithTitle(oldConv, "worker")
		f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
		f.HaveGroup("alpha")
		f.HaveMember("alpha", oldConv)

		// Over the retired 4096-byte cap, under the 16384 inbox cap, and
		// multi-line — both the length AND the newlines used to be rejected.
		followUp := "Handoff brief - picking up the auth refactor.\n\n" +
			strings.Repeat("Detail line about an in-flight file and the next step.\n", 130) +
			"Final line: run the tests before opening the PR."
		require.Greater(t, len(followUp), 4096, "test brief must exceed the retired cap")
		require.Less(t, len(followUp), 16384, "test brief must stay under the inbox cap")
		require.Contains(t, followUp, "\n", "test brief must be multi-line")

		r := f.AsHuman().Reincarnate(oldConv, followUp)

		// The handoff rode the inbox — identical agent_messages path to a
		// spawn --initial-message — and survived verbatim.
		rows, err := db.ListAgentMessagesForConv(r.NewConv, 100)
		require.NoError(t, err, "ListAgentMessagesForConv")
		require.Len(t, rows, 1, "successor should have exactly one inbox message (the handoff)")
		assert.Equal(t, "reincarnation handoff", rows[0].Subject, "handoff subject")
		assert.Equal(t, followUp, rows[0].Body,
			"handoff body must survive verbatim — length and newlines intact")
	})
}

// Scenario: a solo (groupless) agent is reincarnated with a multi-line
// follow-up. With the universal inbox a solo successor HAS an inbox —
// the handoff rides it exactly like a grouped successor's, as a direct
// (group_id 0) agent_messages row, with no tmux send-keys fallback.
//
// Expected: accepted. The multi-line brief lands in the successor's
// inbox verbatim, with group_id 0.
func TestReincarnate_SoloHandoffRidesInbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const oldConv = "solo-aaaa-bbbb-cccc-dddd"
		const oldLabel = "spwn-solo-001"
		const oldTmux = "tclaude-spwn-solo-001"

		f.HaveConvWithTitle(oldConv, "loner")
		f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
		// No HaveGroup / HaveMember: this agent is solo.

		followUp := "Handoff brief - picking up where I left off.\n\n" +
			"Files: pkg/foo/bar.go, pkg/foo/baz.go.\n" +
			"Next: finish the refactor, then run the tests."
		require.Contains(t, followUp, "\n", "test brief must be multi-line")

		r := f.AsHuman().Reincarnate(oldConv, followUp)

		// The solo successor has an inbox now — the handoff rode it as a
		// direct message, the same agent_messages path a grouped handoff
		// takes.
		rows, err := db.ListAgentMessagesForConv(r.NewConv, 100)
		require.NoError(t, err, "ListAgentMessagesForConv")
		require.Len(t, rows, 1, "solo successor should have exactly one inbox message (the handoff)")
		assert.Equal(t, "reincarnation handoff", rows[0].Subject, "handoff subject")
		assert.Equal(t, followUp, rows[0].Body,
			"handoff body must survive verbatim — length and newlines intact")
		assert.Equal(t, int64(0), rows[0].GroupID,
			"a solo handoff is a direct message — group_id 0")
	})
}

// Scenario: a follow-up exceeds even the lenient 16384-byte inbox cap.
//
// Expected: 400 — rejected at decode time, before the membership
// snapshot. The cap is generous but still bounded.
func TestReincarnate_RejectsFollowUpOverInboxCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const oldConv = "old-aaaa-bbbb-cccc-dddd"
		const oldLabel = "spwn-old-001"
		const oldTmux = "tclaude-spwn-old-001"

		f.HaveConvWithTitle(oldConv, "worker")
		f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
		f.HaveGroup("alpha")
		f.HaveMember("alpha", oldConv)

		got := f.AsHuman().ReincarnateWith(oldConv, map[string]any{
			"follow_up": strings.Repeat("a", 20000),
		})
		assert.Equal(t, http.StatusBadRequest, got.Code,
			"a follow-up over the 16384 inbox cap should be rejected; body=%s", got.Raw)
		assert.Contains(t, string(got.Raw), "invalid_follow_up",
			"error body should name the invalid_follow_up code")
	})
}

// Scenario: a grouped worker is cloned with a large multi-line handoff
// brief — over the retired 4096-byte cap, under the 16384 inbox cap.
//
// Expected: accepted. The brief lands in the clone's inbox as the
// "clone handoff" message, verbatim.
func TestClone_GroupedHandoffAcceptsLargeMultiLineBrief(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const oldConv = "old-aaaa-bbbb-cccc-dddd"
		const oldLabel = "spwn-old-001"
		const oldTmux = "tclaude-spwn-old-001"

		f.HaveConvWithTitle(oldConv, "worker")
		f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
		f.HaveGroup("alpha")
		f.HaveMember("alpha", oldConv)

		followUp := "Clone handoff - explore the parallel approach.\n\n" +
			strings.Repeat("Background paragraph the clone should read first.\n", 120) +
			"Report back when the spike is done."
		require.Greater(t, len(followUp), 4096, "test brief must exceed the retired cap")
		require.Less(t, len(followUp), 16384, "test brief must stay under the inbox cap")
		require.Contains(t, followUp, "\n", "test brief must be multi-line")

		// no_copy_conv keeps the test off convops.CopyConversationToPath.
		c := f.AsHuman().CloneWith(oldConv, map[string]any{
			"no_copy_conv": true,
			"follow_up":    followUp,
		})
		require.Equal(t, http.StatusOK, c.Code, "clone should succeed; body=%s", c.Raw)

		rows, err := db.ListAgentMessagesForConv(c.NewConv, 100)
		require.NoError(t, err, "ListAgentMessagesForConv")
		require.Len(t, rows, 1, "clone should have exactly one inbox message (the handoff)")
		assert.Equal(t, "clone handoff", rows[0].Subject, "handoff subject")
		assert.Equal(t, followUp, rows[0].Body,
			"handoff body must survive verbatim — length and newlines intact")
	})
}

// Scenario: a solo (groupless) agent is cloned with a multi-line
// follow-up. As with reincarnate, the universal inbox gives the solo
// clone an inbox; the handoff rides it as a direct (group_id 0)
// agent_messages row.
//
// Expected: accepted. The brief lands in the clone's inbox verbatim.
func TestClone_SoloHandoffRidesInbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const oldConv = "solo-aaaa-bbbb-cccc-dddd"
		const oldLabel = "spwn-solo-001"
		const oldTmux = "tclaude-spwn-solo-001"

		f.HaveConvWithTitle(oldConv, "loner")
		f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
		// No HaveGroup / HaveMember: this agent is solo.

		followUp := "Clone handoff - take the parallel spike.\n\n" +
			"Start from pkg/foo/spike.go; report back when done."
		require.Contains(t, followUp, "\n", "test brief must be multi-line")

		c := f.AsHuman().CloneWith(oldConv, map[string]any{
			"no_copy_conv": true,
			"follow_up":    followUp,
		})
		require.Equal(t, http.StatusOK, c.Code, "solo clone should succeed; body=%s", c.Raw)

		rows, err := db.ListAgentMessagesForConv(c.NewConv, 100)
		require.NoError(t, err, "ListAgentMessagesForConv")
		require.Len(t, rows, 1, "solo clone should have exactly one inbox message (the handoff)")
		assert.Equal(t, "clone handoff", rows[0].Subject, "handoff subject")
		assert.Equal(t, followUp, rows[0].Body,
			"handoff body must survive verbatim — length and newlines intact")
		assert.Equal(t, int64(0), rows[0].GroupID,
			"a solo handoff is a direct message — group_id 0")
	})
}
