package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Inline delivery pastes the sender's subject, body and attachment names into
// the recipient agent's pane (tmux bracketed paste). Text a terminal or the
// recipient harness could interpret rather than show, or that reads
// differently from what it is, is not pasted: the recipient gets the plain
// inbox pointer instead, and the message reaches it unaltered through
// `inbox read` (awb tcl-dih161).
var pasteHostile = []struct{ name, text string }{
	{"ESC", "a\x1b[201~b"},
	{"raw 8-bit C1 CSI", "a\x9b201~b"},
	{"encoded C1 CSI", "a\u009b201~b"},
	{"other C1", "a\u0085b"},
	{"bidi override", "invoice\u202egpj.exe"},
	{"bidi isolate", "a\u2066b\u2069c"},
	{"bidi mark", "a\u200fb"},
	{"zero-width joiner", "a\u200db"},
	{"BOM", "\ufeffa"},
	{"invalid UTF-8", "a\xffb"},
}

func requirePointerNudge(t *testing.T, id int64, hostile string) {
	t.Helper()
	nudge := messageNudgeText(id)
	assert.Contains(t, nudge, "tclaude agent inbox read", "falls back to the inbox pointer")
	assert.NotContains(t, nudge, "; delivery: inline")
	assert.NotContains(t, nudge, hostile, "none of the sender's text is pasted")
	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.Contains(t, m.Body+m.Subject, hostile, "the stored message is unaltered")
}

func TestInlineNudgeFallsBackToThePointerForPasteHostileText(t *testing.T) {
	for _, tc := range pasteHostile {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("body", func(t *testing.T) {
				setupTestDB(t)
				id, err := db.InsertAgentMessage(&db.AgentMessage{
					FromConv: "peer-aaaa-bbbb-cccc", ToConv: "me", Body: "please " + tc.text,
				})
				require.NoError(t, err)
				requirePointerNudge(t, id, tc.text)
			})
			t.Run("subject", func(t *testing.T) {
				setupTestDB(t)
				id, err := db.InsertAgentMessage(&db.AgentMessage{
					FromConv: "peer-aaaa-bbbb-cccc", ToConv: "me", Subject: "re " + tc.text, Body: "plain body",
				})
				require.NoError(t, err)
				requirePointerNudge(t, id, tc.text)
			})
			t.Run("attachment name", func(t *testing.T) {
				setupTestDB(t)
				id, err := db.InsertAgentMessageWithAttachments(&db.AgentMessage{
					FromConv: "peer-aaaa-bbbb-cccc", ToConv: "me", Body: "plain body",
				}, []db.AgentMessageAttachment{{
					Filename: "report" + tc.text + ".txt", ContentType: "text/plain", SizeBytes: 1,
					StoragePath: "/nonexistent/attachment",
				}})
				require.NoError(t, err)
				nudge := messageNudgeText(id)
				assert.Contains(t, nudge, "tclaude agent inbox read")
				assert.NotContains(t, nudge, tc.text)
			})
		})
	}
}

func TestInlineNudgeKeepsOrdinaryUnicodeInline(t *testing.T) {
	setupTestDB(t)
	body := "Smörgåsbord, 日本語, שלום, emoji 🙂, and\nlines\twith tabs."
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		FromConv: "peer-aaaa-bbbb-cccc", ToConv: "me", Subject: "ünïcode", Body: body,
	})
	require.NoError(t, err)
	nudge := messageNudgeText(id)
	assert.Contains(t, nudge, "; delivery: inline")
	assert.Contains(t, nudge, "] "+body)
}

// A long sender title with multi-byte characters still names the sender
// inline: the label is cut on a rune boundary, so it stays paste-safe.
func TestInlineNudgeKeepsALongMultiByteSenderTitle(t *testing.T) {
	setupTestDB(t)
	const sender = "cjk-sender-conv"
	_, _, err := db.EnsureAgentForConv(sender, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: sender, CustomTitle: "\u30b3\u30fc\u30c9\u30ec\u30d3\u30e5\u30fc\u62c5\u5f53\u30a8\u30fc\u30b8\u30a7\u30f3\u30c8",
	}))
	id, err := db.InsertAgentMessage(&db.AgentMessage{FromConv: sender, ToConv: "me", Body: "hello"})
	require.NoError(t, err)
	nudge := messageNudgeText(id)
	assert.Contains(t, nudge, "; delivery: inline")
	assert.Contains(t, nudge, "\u30b3\u30fc\u30c9", "the sender is named")
}

// A sender whose title carries a format character (the Summary/FirstPrompt
// fallback is not charset-gated like /rename) is not named in anything pasted
// into the recipient's pane: not the nudge, not the pointer, not the unread
// reminder.
func TestPastedTextNeverNamesAPasteHostileSender(t *testing.T) {
	setupTestDB(t)
	const sender = "bidi-sender-conv"
	_, _, err := db.EnsureAgentForConv(sender, "test")
	require.NoError(t, err)
	const hostile = "invoice\u202egpj.exe"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: sender, FirstPrompt: hostile}))
	id, err := db.InsertAgentMessage(&db.AgentMessage{FromConv: sender, ToConv: "me", Body: "hello"})
	require.NoError(t, err)
	nudge := messageNudgeText(id)
	assert.Contains(t, nudge, "tclaude agent inbox read")
	assert.NotContains(t, nudge, "\u202e")
	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	reminder := unreadReminderText([]*db.AgentMessage{m})
	assert.Contains(t, reminder, "tclaude agent inbox read")
	assert.NotContains(t, reminder, "\u202e")
}
