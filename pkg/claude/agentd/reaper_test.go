package agentd

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// reaperFallbackExitReason must only stamp "unexpected" on harnesses that
// have a graceful-exit hook whose absence is genuinely suspicious. Claude
// Code does; Codex and a plain shell do not, so a deliberate exit in either
// must stay reasonless (no spurious "Exited" banner).
func TestReaperFallbackExitReason(t *testing.T) {
	assert.Equal(t, unexpectedExitReason, reaperFallbackExitReason("claude"))
	assert.Equal(t, unexpectedExitReason, reaperFallbackExitReason(""), "unknown harness is treated like Claude")
	assert.Equal(t, "", reaperFallbackExitReason("opencode"),
		"a deliberate attach-client exit is normal; the server-loss branch stamps crashes explicitly")
	assert.Equal(t, "", reaperFallbackExitReason("codex"))
	assert.Equal(t, "", reaperFallbackExitReason(session.ShellHarnessName), "clean shell exit is normal, not unexpected")
}

func TestSessionReaper_OpenCodeServerLossOverridesLivePane(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const sessionID = "spwn-opencode-loss"
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, ConvID: "ses_server_lost", Status: session.StatusWorking,
		Harness: "opencode", Cwd: dir, PID: os.Getpid(),
	}))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID,
		"11111111111111111111111111111111"))

	r := &sessionReaper{
		aliveLastTick: map[string]bool{sessionID: true},
		seeded:        true,
		grace:         0,
		notify:        func(*session.SessionState, string) {},
	}
	require.Equal(t, 1, r.tick(time.Now()),
		"a live attach PID must not hide a missing authoritative server")
	state, err := session.LoadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusExited, state.Status)
	reason, err := db.GetSessionExitReason(sessionID)
	require.NoError(t, err)
	assert.Equal(t, unexpectedExitReason, reason)
}

// Codex sessions have no SessionEnd hook, so their alive→dead transition
// is detected by the reaper (tmux has-session → PID liveness), not the
// event switch. This drives the production reaper over a harness=codex
// row: tick one sees it alive (seeding the witnessed-alive set), the
// process then goes, and tick two marks it exited AND fires the offline
// notification — carrying the harness so the banner reads "Codex: Exited".
func TestSessionReaper_ReapsDeadCodexSessionAndNotifies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	// tick() sees the alive session and fires a debounced goBackground
	// flush (maybeFlushUndelivered → drainNudgeLoop) that touches the
	// singleton DB under $HOME/.tclaude. Drain it before t.TempDir's
	// RemoveAll runs (this Cleanup is registered after TempDir's, so LIFO
	// runs it first) so no orphaned goroutine races the teardown — the
	// ENOTEMPTY that bgWG exists to prevent (see background.go).
	t.Cleanup(bgWG.Wait)

	const sessionID = "agent-codex-reap"
	const convID = "019ec004-4250-79b1-9ade-ebaea4159777"

	type offline struct {
		id      string
		prev    string
		harness string
	}
	var fired []offline
	r := &sessionReaper{
		aliveLastTick: map[string]bool{},
		grace:         0, // no grace window: reap as soon as it looks dead
		notify: func(st *session.SessionState, prevStatus string) {
			fired = append(fired, offline{st.ID, prevStatus, st.Harness})
		},
	}

	// A live Codex session: no tmux pane recorded, but this test process's
	// PID stands in for a live agent process so RefreshSessionStatus keeps
	// it alive on the first sweep.
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:          sessionID,
		ConvID:      convID,
		Status:      session.StatusWorking,
		Harness:     "codex",
		Cwd:         "/home/u/proj",
		TmuxSession: "",
		PID:         os.Getpid(),
	}))

	// Tick 1: witnessed alive, seeds the alive set, nothing reaped.
	require.Equal(t, 0, r.tick(time.Now()))
	require.Empty(t, fired, "a live session is not reaped")
	require.True(t, r.aliveLastTick[sessionID], "the live Codex session was witnessed alive")
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID,
		"11111111111111111111111111111111"))
	_, err := db.SetSessionExitIntent(sessionID, db.AgentExitActionStop,
		"evt_1234567890abcdef12345678", time.Now())
	require.NoError(t, err)

	// The process goes away (PID cleared). Status is still 'working' in
	// the DB — the reaper, not a hook, is what will flip it.
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:          sessionID,
		ConvID:      convID,
		Status:      session.StatusWorking,
		Harness:     "codex",
		Cwd:         "/home/u/proj",
		TmuxSession: "",
		PID:         0,
	}))

	// Tick 2: looks dead → marked exited + offline notification fired.
	require.Equal(t, 1, r.tick(time.Now()))

	st, err := session.LoadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusExited, st.Status, "dead Codex session reaped to exited")
	reason, err := db.GetSessionExitReason(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "", reason, "plain Codex disappearance is offline, not crashed")

	require.Len(t, fired, 1, "exactly one offline notification")
	assert.Equal(t, sessionID, fired[0].id)
	assert.Equal(t, session.StatusWorking, fired[0].prev, "transition is working→exited")
	assert.Equal(t, "codex", fired[0].harness, "harness carried into the notification for correct attribution")
	audit, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, audit, 1)
	assert.Equal(t, db.AgentExitObserverReaper, audit[0].Observer)
	assert.Equal(t, db.AgentExitCauseDisappeared, audit[0].CauseKind)
	assert.Equal(t, db.AgentExitActionStop, audit[0].LifecycleAction)
	assert.Equal(t, "evt_1234567890abcdef12345678", audit[0].RelatedEventID)
}

func TestSessionReaper_FirstTickRecordsReconciliation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	t.Cleanup(bgWG.Wait)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "dead-before-start", ConvID: "dead-conv-12345678",
		Status: session.StatusWorking, PID: 0,
	}))
	r := &sessionReaper{aliveLastTick: map[string]bool{}, grace: 0, notify: func(*session.SessionState, string) {}}
	require.Equal(t, 1, r.tick(time.Now()))

	audit, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, audit, 1)
	assert.Equal(t, db.AgentExitObserverReconcile, audit[0].Observer)
	assert.Equal(t, db.AgentExitCauseDisappeared, audit[0].CauseKind)
}
