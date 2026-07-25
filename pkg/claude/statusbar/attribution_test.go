package statusbar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The statusbar keys every per-session write on TCLAUDE_SESSION_ID, which
// every child process inherits. ownedSessionID is what stops a nested
// Claude Code — one a human or an agent starts from inside an agent's own
// pane — from rendering ITS statusline onto the parent agent's row and
// overwriting the parent's model, effort and context usage with its own.
// Reproduced on Claude Code 2.1.220: a child launched with the parent's
// TCLAUDE_SESSION_ID and `--model haiku` reported "Haiku 4.5" plus its own
// 200K/17% context against the parent's session id.

// attributionWorld seeds an isolated DB with one session row tracking
// convID (and optionally an announced pending conv).
func attributionWorld(t *testing.T, sessionID, convID, pendingConv string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	require.NoError(t, db.SaveSession(&db.SessionRow{ID: sessionID, ConvID: convID}))
	if pendingConv != "" {
		require.NoError(t, db.SetSessionPendingConv(sessionID, pendingConv))
	}
}

// The reported corruption: a foreign render naming a conversation the row
// does not track must not be allowed to write the row at all.
func TestOwnedSessionID_RejectsForeignConversation(t *testing.T) {
	attributionWorld(t, "sess-parent", "conv-parent", "")

	assert.Empty(t, ownedSessionID("sess-parent", "conv-child-haiku"),
		"a render from another conversation must not claim the parent's row")
}

// The ordinary case: the render names the conversation its row tracks.
func TestOwnedSessionID_AcceptsTrackedConversation(t *testing.T) {
	attributionWorld(t, "sess-parent", "conv-parent", "")

	assert.Equal(t, "sess-parent", ownedSessionID("sess-parent", "conv-parent"))
}

// A /clear or /resume rotates the conv-id, and the transition SessionStart
// announces the new one as pending_conv BEFORE the row advances. Renders
// carrying the announced conv must keep writing across that window —
// the same acceptance rule ApplyHook's foreign-process guard uses.
func TestOwnedSessionID_AcceptsAnnouncedPendingConversation(t *testing.T) {
	attributionWorld(t, "sess-parent", "conv-old", "conv-new")

	assert.Equal(t, "sess-parent", ownedSessionID("sess-parent", "conv-new"),
		"an announced conv-id rotation is not a foreign process")
}

// Fail-soft cases: nothing here is evidence of a mismatch, and failing
// closed would cost real agents their telemetry.
func TestOwnedSessionID_FailsSoftWithoutEvidence(t *testing.T) {
	t.Run("payload carries no session_id", func(t *testing.T) {
		attributionWorld(t, "sess-parent", "conv-parent", "")
		// Claude Code versions predating the session_id field.
		assert.Equal(t, "sess-parent", ownedSessionID("sess-parent", ""))
	})

	t.Run("row has not learned its conv yet", func(t *testing.T) {
		attributionWorld(t, "sess-fresh", "", "")
		// A freshly spawned agent renders before its first SessionStart
		// hook lands; that first snapshot must still be recorded.
		assert.Equal(t, "sess-fresh", ownedSessionID("sess-fresh", "conv-first"))
	})

	t.Run("session row does not exist", func(t *testing.T) {
		attributionWorld(t, "sess-parent", "conv-parent", "")
		assert.Equal(t, "sess-unknown", ownedSessionID("sess-unknown", "conv-whatever"))
	})
}

// A session not launched by tclaude has no row to protect.
func TestOwnedSessionID_NoEnvSession(t *testing.T) {
	attributionWorld(t, "sess-parent", "conv-parent", "")

	assert.Empty(t, ownedSessionID("", "conv-parent"))
}
