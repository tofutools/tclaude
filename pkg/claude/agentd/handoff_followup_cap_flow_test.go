package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These tests pin the per-delivery-path follow-up cap for clone /
// reincarnate handoffs:
//
//   - A GROUPED successor receives the handoff as an agent_messages
//     inbox row — the same path a spawn --initial-message takes — so a
//     long, multi-line brief (≤16384 bytes) is accepted verbatim.
//   - A SOLO (groupless) successor's handoff is typed into the new pane
//     via tmux send-keys, so it keeps the strict ≤4096-byte, no-newline
//     limit.
//
// Before this change the strict limit was applied uniformly, needlessly
// capping a grouped handoff that was always going to ride the inbox.

// Scenario: a grouped worker is reincarnated with a real handoff brief
// — multi-paragraph, well over the retired 4096-byte follow-up cap.
//
// Expected: accepted. The brief lands in the successor's inbox as the
// "reincarnation handoff" message, verbatim — newlines and all.
func TestReincarnate_GroupedHandoffAcceptsLargeMultiLineBrief(t *testing.T) {
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
}

// Scenario: a solo (groupless) agent is reincarnated with a multi-line
// follow-up. A solo successor has no inbox; its handoff is typed into
// the new pane via send-keys, where each newline would submit early.
//
// Expected: 400 invalid_follow_up, naming the solo/send-keys reason.
func TestReincarnate_SoloHandoffRejectsMultiLineFollowUp(t *testing.T) {
	f := newFlow(t)

	const oldConv = "solo-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-solo-001"
	const oldTmux = "tclaude-spwn-solo-001"

	f.HaveConvWithTitle(oldConv, "loner")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	// No HaveGroup / HaveMember: this agent is solo.

	got := f.AsHuman().ReincarnateWith(oldConv, map[string]any{
		"follow_up": "first line\nsecond line",
	})
	assert.Equal(t, http.StatusBadRequest, got.Code,
		"solo reincarnate with a multi-line follow-up should be rejected; body=%s", got.Raw)
	assert.Contains(t, string(got.Raw), "invalid_follow_up",
		"error body should name the invalid_follow_up code")
	assert.Contains(t, string(got.Raw), "no group",
		"rejection should explain the solo/send-keys reason")
}

// Scenario: a follow-up exceeds even the lenient 16384-byte inbox cap.
//
// Expected: 400 — rejected at decode time, before the membership
// snapshot. The cap is generous but still bounded.
func TestReincarnate_RejectsFollowUpOverInboxCap(t *testing.T) {
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
}

// Scenario: a grouped worker is cloned with a large multi-line handoff
// brief — over the retired 4096-byte cap, under the 16384 inbox cap.
//
// Expected: accepted. The brief lands in the clone's inbox as the
// "clone handoff" message, verbatim.
func TestClone_GroupedHandoffAcceptsLargeMultiLineBrief(t *testing.T) {
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
}

// Scenario: a solo (groupless) agent is cloned with a multi-line
// follow-up.
//
// Expected: 400 invalid_follow_up — rejected before the rate-limit
// claim, so a corrected retry isn't blocked by the clone cooldown.
func TestClone_SoloHandoffRejectsMultiLineFollowUp(t *testing.T) {
	f := newFlow(t)

	const oldConv = "solo-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-solo-001"
	const oldTmux = "tclaude-spwn-solo-001"

	f.HaveConvWithTitle(oldConv, "loner")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	// No HaveGroup / HaveMember: this agent is solo.

	got := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    "first line\nsecond line",
	})
	assert.Equal(t, http.StatusBadRequest, got.Code,
		"solo clone with a multi-line follow-up should be rejected; body=%s", got.Raw)
	assert.Contains(t, string(got.Raw), "invalid_follow_up",
		"error body should name the invalid_follow_up code")
	assert.Contains(t, string(got.Raw), "no group",
		"rejection should explain the solo/send-keys reason")
}
