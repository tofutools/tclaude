package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestNudge_NamesSenderByStableIdentity pins JOH-27 PR3b: the tmux nudge a
// recipient receives now names the sender as "name (agt_xxxxxxxx)" — the
// stable agent_id, not a rotation-prone conv-id prefix. The sender is enrolled
// (HaveMember → agent_id) and titled (HaveConvWithTitle), so both halves of the
// label appear in the bracketed line injected into the recipient's live pane.
func TestNudge_NamesSenderByStableIdentity(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "nid1-send-bbbb-cccc-000000000001"
	const recipient = "nid1-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po-coordinator")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, "spwn-nid1-r", "tclaude-spwn-nid1-r", f.TestCwd("work"))

	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The sender's stable agent_id is what the nudge must surface — the short
	// form (agt_ + first 8 hex of the suffix) for a terse line.
	senderAgent, err := db.AgentIDForConv(sender)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(senderAgent), 12, "sender has a stable agent_id")

	f.AssertSentContains("tclaude-spwn-nid1-r:0.0",
		"from po-coordinator ("+senderAgent[:12]+")", 2*time.Second)
}
