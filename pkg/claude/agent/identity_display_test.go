package agent

import (
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestCronTargetLabel covers the `cron ls` TARGET column rendering (PR3c): a
// group target shows "group:<name>", a solo target shows the target actor's
// short stable agent_id, falling back to the conv-id prefix when the target
// isn't an enrolled agent.
func TestCronTargetLabel(t *testing.T) {
	const agentID = "agt_0123456789abcdef0123456789abcdef"
	const convID = "11112222-3333-4444-5555-666677778888"

	assert.Equal(t, agentID[:12], cronTargetLabel(cronJobJSON{
		TargetKind: "conv", TargetAgent: agentID, TargetConv: convID,
	}), "solo target → short stable agent_id")

	assert.Equal(t, convID[:8], cronTargetLabel(cronJobJSON{
		TargetKind: "conv", TargetConv: convID,
	}), "no agent_id → conv-id prefix fallback")

	assert.Equal(t, "group:devs", cronTargetLabel(cronJobJSON{
		TargetKind: "group", GroupName: "devs", GroupID: 7,
	}), "group target → group:<name>")

	assert.Equal(t, "group:#7", cronTargetLabel(cronJobJSON{
		TargetKind: "group", GroupID: 7,
	}), "group target without a resolved name → group:#<id>")
}

// TestActorID covers the inbox-header identifier helper: stable agent_id when
// present, conv-id otherwise (a non-actor conv, or an older daemon that didn't
// send the agent field).
func TestActorID(t *testing.T) {
	assert.Equal(t, "agt_0123456789abcdef", actorID("agt_0123456789abcdef", "conv-xyz"),
		"agent_id wins when present")
	assert.Equal(t, "conv-xyz", actorID("", "conv-xyz"),
		"falls back to conv-id when no agent_id")
	assert.Equal(t, "", actorID("", ""), "both empty stays empty")
}

// TestActorHeader covers the "name (agent_id)" / bare-id rendering used in
// inbox-read From/To headers.
func TestActorHeader(t *testing.T) {
	assert.Equal(t, "po-coordinator (agt_0123456789abcdef)",
		actorHeader("po-coordinator", "agt_0123456789abcdef", "conv-xyz"),
		"name + agent_id")
	assert.Equal(t, "agt_0123456789abcdef",
		actorHeader("", "agt_0123456789abcdef", "conv-xyz"),
		"bare id when no title")
	assert.Equal(t, "planner (conv-xyz)",
		actorHeader("planner", "", "conv-xyz"),
		"falls back to conv-id when the actor has no agent_id")
	assert.Equal(t, "human operator",
		actorHeader("human operator", "", ""),
		"named actor without an id has no dangling parentheses")
	assert.Equal(t, "human operator",
		actorHeader("", "", ""),
		"senderless messages identify the operator")
}

// TestMessageSenderLabel exercises the tmux-nudge sender label: the (truncated)
// current title plus the stable short agent_id, with graceful fallbacks.
func TestMessageSenderLabel(t *testing.T) {
	setupTestDB(t)

	const senderConv = "aaaaaaaa-1111-2222-3333-444444444444"
	upsertConvIndex(t, senderConv, "po-coordinator", "", "")
	agentID, _, err := db.EnsureAgentForConv(senderConv, "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	require.GreaterOrEqual(t, len(agentID), 12, "agent_id long enough to shorten")

	// Title + agent_id → "name (agt_xxxxxxxx)".
	assert.Equal(t, "po-coordinator ("+agentID[:12]+")",
		MessageSenderLabel(senderConv, agentID),
		"names the sender with its title and stable short id")

	// No durable agent_id (e.g. a non-actor conv) → title plus the conv prefix.
	assert.Equal(t, "po-coordinator ("+senderConv[:8]+")",
		MessageSenderLabel(senderConv, ""),
		"falls back to the conv prefix when no agent_id")

	// Agent with no indexed title → the bare short id (never empty).
	const namelessConv = "bbbbbbbb-1111-2222-3333-444444444444"
	namelessAgent, _, err := db.EnsureAgentForConv(namelessConv, "spawn")
	require.NoError(t, err)
	assert.Equal(t, namelessAgent[:12], MessageSenderLabel(namelessConv, namelessAgent),
		"bare short id when the sender has no title")
}

// TestMessageSenderLabel_TruncatesLongName pins that a pathologically long
// /rename title can't blow up the bracketed nudge line: the name is capped at
// nudgeSenderNameMax (with an ellipsis), while the stable short id is untouched.
func TestMessageSenderLabel_TruncatesLongName(t *testing.T) {
	setupTestDB(t)

	const senderConv = "cccccccc-1111-2222-3333-444444444444"
	longName := "this-is-an-absurdly-long-agent-name-that-should-be-truncated"
	require.Greater(t, len(longName), nudgeSenderNameMax, "precondition: name exceeds the cap")
	upsertConvIndex(t, senderConv, longName, "", "")
	agentID, _, err := db.EnsureAgentForConv(senderConv, "spawn")
	require.NoError(t, err)

	label := MessageSenderLabel(senderConv, agentID)
	// Name segment is the part before " (".
	name := label[:len(label)-len(" ("+agentID[:12]+")")]
	assert.LessOrEqual(t, len(name), nudgeSenderNameMax, "name capped at nudgeSenderNameMax")
	assert.Contains(t, name, "...", "truncated name carries an ellipsis")
	assert.Contains(t, label, "("+agentID[:12]+")", "stable short id survives intact")
}

// TestMessageSenderLabel_SanitizesControlChars pins the send-keys safety
// invariant: a title carrying control characters (a newline/tab — possible
// from the un-charset-gated Summary/FirstPrompt title fallback, e.g. a
// multi-line spawn brief used before /rename lands) must not survive into the
// nudge label, or it would inject a premature Enter into the recipient's pane.
func TestMessageSenderLabel_SanitizesControlChars(t *testing.T) {
	setupTestDB(t)

	const senderConv = "dddddddd-1111-2222-3333-444444444444"
	upsertConvIndex(t, senderConv, "line one\nline two\twith tab", "", "")
	agentID, _, err := db.EnsureAgentForConv(senderConv, "spawn")
	require.NoError(t, err)

	label := MessageSenderLabel(senderConv, agentID)
	for _, r := range label {
		require.Falsef(t, unicode.IsControl(r), "control rune %q survived into label %q", r, label)
	}
	assert.Contains(t, label, "("+agentID[:12]+")", "stable short id intact after sanitization")
}
