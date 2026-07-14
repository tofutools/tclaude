package session

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The session row's primary key must carry the full identity, never an 8-char
// truncation of the conversation UUID. Two conversations sharing a hex prefix
// previously collapsed to the same PK and the second silently overwrote the
// first (SaveSession's ON CONFLICT(id)) — wrong-session reattach + conflated
// notify_state / session_cost_daily. See JOH-248.

func TestGenerateSessionID_FullEntropyNotTruncated(t *testing.T) {
	id := GenerateSessionID()
	assert.Len(t, id, 16, "synthetic session id is 64-bit (16 hex), not an 8-char truncation")
	for _, r := range id {
		assert.Contains(t, "0123456789abcdef", string(r), "id must be lowercase hex")
	}

	// The old UnixNano-low-32 scheme could repeat for spawns at congruent
	// nanosecond times; crypto/rand must not.
	seen := make(map[string]bool, 2000)
	for range 2000 {
		v := GenerateSessionID()
		require.False(t, seen[v], "GenerateSessionID returned a duplicate")
		seen[v] = true
	}
}

func TestSessionHandle(t *testing.T) {
	full := "d0e9fa14-1234-4abc-9def-0123456789ab"
	assert.Equal(t, "d0e9fa14", sessionHandle(&SessionState{ID: full, TmuxSession: "d0e9fa14"}),
		"the short tmux name is the human-facing handle")
	assert.Equal(t, "spwn-ab12cd", sessionHandle(&SessionState{ID: "spwn-ab12cd", TmuxSession: "spwn-ab12cd"}),
		"a labelled session shows its full label")
	assert.Equal(t, full, sessionHandle(&SessionState{ID: full}),
		"with no tmux name the full id is the fallback handle")
}

func setSessionUpdatedAt(t *testing.T, id string, updatedAt time.Time) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	result, err := d.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		updatedAt.Format(time.RFC3339Nano), id)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows, "session %q must exist", id)
}

// The core regression: two conversations sharing an 8-char prefix keep two
// distinct session rows, and short ids still resolve as CLI input.
func TestSessionPK_FullUUID_NoPrefixCollision(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	// Same first 8 hex ("d0e9fa14"), different full UUIDs — the exact shape
	// that used to collide on the truncated PK.
	convA := "d0e9fa14-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	convB := "d0e9fa14-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	require.NoError(t, SaveSessionState(&SessionState{
		ID: convA, ConvID: convA, TmuxSession: "d0e9fa14", Status: StatusIdle,
	}))
	require.NoError(t, SaveSessionState(&SessionState{
		ID: convB, ConvID: convB, TmuxSession: "d0e9fa14-2", Status: StatusIdle,
	}))

	states, err := ListSessionStates()
	require.NoError(t, err)
	assert.Len(t, states, 2, "two conversations sharing an 8-char prefix must keep two distinct session rows")

	// The bare short handle resolves to the session that owns that tmux name;
	// the disambiguated handle resolves to the other. (Short ids still work as
	// CLI input — they resolve early to the full identity.)
	got, err := findSession("d0e9fa14")
	require.NoError(t, err)
	assert.Equal(t, convA, got.ID, "the clean short handle resolves to its owning session")

	got, err = findSession("d0e9fa14-2")
	require.NoError(t, err)
	assert.Equal(t, convB, got.ID, "the disambiguated tmux handle resolves to the other session")

	// A shorter prefix matching both full ids (and no exact tmux name) is
	// ambiguous and must error rather than silently pick one.
	_, err = findSession("d0e9fa1")
	assert.Error(t, err, "an ambiguous short prefix must not resolve to a single session")

	// Full ids always resolve uniquely.
	got, err = findSession(convB)
	require.NoError(t, err)
	assert.Equal(t, convB, got.ID, "the full conversation UUID resolves to its session")
}

// tmux session names are reused after a session exits, so two rows (distinct
// full UUIDs) can share one tmux name — a stale dead row and the live owner.
// The bare tmux handle must resolve to the live owner (most-recently-updated),
// not the lingering exited row. See JOH-248.
func TestFindSession_StaleTmuxName_PrefersLiveOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	stale := "d0e9fa14-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	live := "d0e9fa14-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	require.NoError(t, SaveSessionState(&SessionState{
		ID: stale, ConvID: stale, TmuxSession: "d0e9fa14", Status: StatusExited,
	}))
	require.NoError(t, SaveSessionState(&SessionState{
		ID: live, ConvID: live, TmuxSession: "d0e9fa14", Status: StatusIdle,
	}))
	staleUpdatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setSessionUpdatedAt(t, stale, staleUpdatedAt)
	setSessionUpdatedAt(t, live, staleUpdatedAt.Add(time.Minute))

	got, err := findSession("d0e9fa14")
	require.NoError(t, err)
	assert.Equal(t, live, got.ID,
		"the bare tmux handle must resolve to the live owner, not a stale exited row")
}

func TestUniqueTmuxSessionName_FreeBaseUnchanged(t *testing.T) {
	// No tclaude tmux server in the unit-test env, so every candidate is free
	// and the bare base is returned verbatim ("short if possible"). The -N
	// suffix fallback needs a live tmux session and is exercised end-to-end,
	// not here.
	assert.Equal(t, "d0e9fa14", UniqueTmuxSessionName("d0e9fa14"))
	assert.Equal(t, "spwn-ab12cd", UniqueTmuxSessionName("spwn-ab12cd"))
	assert.Equal(t, "", UniqueTmuxSessionName(""))
}

// fakeTmux reports the named sessions as alive (has-session exits 0) and every
// other name as dead. Only the has-session probe IsTmuxSessionAlive issues is
// modelled; it's all liveSessionOwningID needs.
type fakeTmux struct{ alive map[string]bool }

func (f fakeTmux) Command(args ...string) *exec.Cmd {
	// IsTmuxSessionAlive issues: has-session -t =<name> (exact-match form,
	// see clcommon.ExactTarget); exit 0 == alive.
	if len(args) == 3 && args[0] == "has-session" && f.alive[strings.TrimPrefix(args[2], "=")] {
		return exec.Command("true")
	}
	return exec.Command("false")
}

func (f fakeTmux) ListSessions() (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(f.alive))
	for name, live := range f.alive {
		if live {
			out[name] = struct{}{}
		}
	}
	return out, nil
}

// The launch path rejects reusing a session PK that a LIVE session already
// owns — otherwise SaveSessionState's ON CONFLICT(id) would silently overwrite
// (and orphan) that live session, the duplicate-tmux-name clean-fail the
// pre-JOH-248 code got for free once the tmux name is disambiguated apart from
// the PK. This guards BOTH launch PKs that can collide: an explicit --label,
// and a resumed conversation's full UUID (relaunching an already-live conv would
// also double `claude --resume` the same .jsonl). A PK owned only by a DEAD row,
// or a free PK, is fine to (re)create. See JOH-248.
func TestLiveSessionOwningID_GuardsLivePKReuse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	convUUID := "d0e9fa14-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"live-label": true, "d0e9fa14": true}}
	t.Cleanup(func() { clcommon.Default = prevTmux })

	// Free PK — nothing to collide with.
	assert.Nil(t, liveSessionOwningID("free-label"),
		"a PK with no existing row is free to use")

	// --label PK owned by a LIVE session — must report the collision so the
	// caller rejects the launch instead of overwriting the row.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "live-label", ConvID: "conv-aaaa", TmuxSession: "live-label", Status: StatusIdle,
	}))
	owner := liveSessionOwningID("live-label")
	require.NotNil(t, owner, "a live session already holding the label must block reuse")
	assert.Equal(t, "live-label", owner.ID)

	// Resumed-conv PK (full UUID) owned by a LIVE session — same guard: the
	// row is keyed by the conv UUID and its tmux name was disambiguated to the
	// 8-char prefix, which is the live name. Relaunching must be rejected.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: convUUID, ConvID: convUUID, TmuxSession: "d0e9fa14", Status: StatusIdle,
	}))
	owner = liveSessionOwningID(convUUID)
	require.NotNil(t, owner, "a live session already resuming this conv must block a relaunch")
	assert.Equal(t, convUUID, owner.ID)

	// PK owned only by a DEAD row (its tmux name is not alive) — recreating
	// over an exited row is fine, so no collision is reported.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "dead-label", ConvID: "conv-bbbb", TmuxSession: "dead-label", Status: StatusExited,
	}))
	assert.Nil(t, liveSessionOwningID("dead-label"),
		"an exited row sharing the PK does not block a fresh launch")
}

// The resume-path double-launch guard must key on conv_id, not the PK. A
// conversation made live by a FRESH `session new` has a random synthetic PK
// (the full UUID lives only in the conv_id column); an old, pre-de-truncation
// row has a convID[:8] PK. In both cases a PK-keyed LoadSessionState(fullUUID)
// MISSES the live row, so a manual `session new -r` / `conv resume` of that
// already-live conversation used to slip past the guard and double-launch
// `claude --resume` on the same .jsonl (interleaved appends → corruption).
// LiveSessionForConv catches it whatever the PK shape. See JOH-332.
func TestLiveSessionForConv_FindsLiveByConvID_RegardlessOfPK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"spwn-abc123": true, "bbbbbbbb": true}}
	t.Cleanup(func() { clcommon.Default = prevTmux })

	// Fresh-spawn shape: synthetic PK, full UUID only in conv_id, live.
	freshConv := "aaaaaaaa-1111-4111-8111-111111111111"
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-abc123", ConvID: freshConv, TmuxSession: "spwn-abc123", Status: StatusIdle,
	}))

	// The PK-keyed lookup the old guard used misses it — that's the gap (it
	// returns no row; the production guard ignores the error and checks nil).
	pkLookup, _ := LoadSessionState(freshConv)
	assert.Nil(t, pkLookup, "PK-keyed LoadSessionState(fullUUID) misses a synthetic-PK row — the JOH-332 gap")

	// The conv_id-keyed guard catches it.
	got := LiveSessionForConv(freshConv)
	require.NotNil(t, got, "a live fresh-spawn session must be found by conv_id so resume can't double-launch")
	assert.Equal(t, "spwn-abc123", got.ID)

	// Old pre-de-truncation shape: convID[:8] PK, live — also caught.
	oldConv := "bbbbbbbb-2222-4222-8222-222222222222"
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "bbbbbbbb", ConvID: oldConv, TmuxSession: "bbbbbbbb", Status: StatusIdle,
	}))
	got = LiveSessionForConv(oldConv)
	require.NotNil(t, got, "a live pre-de-truncation session must be found by conv_id (the upgrade-window case)")
	assert.Equal(t, "bbbbbbbb", got.ID)

	// A conv whose only session is DEAD (tmux name not alive) is resumable —
	// no live session to collide with.
	deadConv := "cccccccc-3333-4333-8333-333333333333"
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-dead", ConvID: deadConv, TmuxSession: "spwn-dead", Status: StatusExited,
	}))
	assert.Nil(t, LiveSessionForConv(deadConv),
		"an exited session for the conv does not block a resume")

	// Unknown conv and empty input resolve to nil, not an error.
	assert.Nil(t, LiveSessionForConv("dddddddd-4444-4444-8444-444444444444"))
	assert.Nil(t, LiveSessionForConv(""))
}

// LiveSessionForConv must probe ALL rows for the conv, not just the freshest:
// a dead row's updated_at can be bumped above a live-but-idle row's (the
// reaper, or a stale-handle attach), so a "most-recent row only" probe would
// miss the live session and wrongly allow a second `claude --resume`. See
// JOH-332 (cold-review follow-up).
func TestLiveSessionForConv_MultiRow_PrefersLiveOverFreshDead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"livename": true}} // "spwn-stale" is dead
	t.Cleanup(func() { clcommon.Default = prevTmux })

	conv := "aaaaaaaa-1111-4111-8111-111111111111"

	// Live row written FIRST so its updated_at is older...
	require.NoError(t, SaveSessionState(&SessionState{
		ID: conv, ConvID: conv, TmuxSession: "livename", Status: StatusIdle,
	}))
	// ...then a DEAD sibling row made fresher by
	// updated_at (FindSessionByConvID's ORDER BY updated_at DESC LIMIT 1 would
	// return this one and report no live session).
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-stale", ConvID: conv, TmuxSession: "spwn-stale", Status: StatusExited,
	}))
	liveUpdatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setSessionUpdatedAt(t, conv, liveUpdatedAt)
	setSessionUpdatedAt(t, "spwn-stale", liveUpdatedAt.Add(time.Minute))

	got := LiveSessionForConv(conv)
	require.NotNil(t, got, "the live row must be found even when a dead sibling row is more recently updated")
	assert.Equal(t, conv, got.ID)
}

// findSession must resolve a shared tmux handle to the LIVE owner, not a stale
// exited row that recorded the same tmux name. The name probe reports both
// rows' tmux name alive (it belongs to the live owner), so the persisted
// Status is the disambiguator — and the live row must win even when the exited
// namesake is more recently updated. See JOH-248/JOH-332.
func TestFindSession_PrefersLiveOwnerOverExitedNamesake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"d0e9fa14": true}} // the live owner's pane
	t.Cleanup(func() { clcommon.Default = prevTmux })

	live := "d0e9fa14-1111-4111-8111-111111111111"

	// Live owner written first (older updated_at), full-UUID PK, tmux d0e9fa14.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: live, ConvID: live, TmuxSession: "d0e9fa14", Status: StatusIdle,
	}))
	// Stale exited row made newer by updated_at: an old convID[:8] PK
	// that recorded the same tmux name before it exited.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "d0e9fa14", ConvID: "d0e9fa14-2222-4222-8222-222222222222", TmuxSession: "d0e9fa14", Status: StatusExited,
	}))
	liveUpdatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setSessionUpdatedAt(t, live, liveUpdatedAt)
	setSessionUpdatedAt(t, "d0e9fa14", liveUpdatedAt.Add(time.Minute))

	got, err := findSession("d0e9fa14")
	require.NoError(t, err)
	assert.Equal(t, live, got.ID,
		"the shared handle must resolve to the live owner, not the newer exited namesake")
	assert.NotEqual(t, StatusExited, got.Status)
}
