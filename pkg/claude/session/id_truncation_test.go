package session

import (
	"os/exec"
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

func TestShortTmuxBase(t *testing.T) {
	full := "d0e9fa14-1234-4abc-9def-0123456789ab"
	assert.Equal(t, "d0e9fa14", ShortTmuxBase(full, ""), "a long id renders as its 8-char prefix")
	assert.Equal(t, "spwn-ab12cd", ShortTmuxBase(full, "spwn-ab12cd"), "an explicit label wins verbatim, never truncated")
	assert.Equal(t, "abc", ShortTmuxBase("abc", ""), "an already-short id is left as-is")
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
	// Force a distinct, later updated_at for the live row (SaveSession stamps
	// updated_at = now on each write).
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: live, ConvID: live, TmuxSession: "d0e9fa14", Status: StatusIdle,
	}))

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
	// IsTmuxSessionAlive issues: has-session -t <name>; exit 0 == alive.
	if len(args) == 3 && args[0] == "has-session" && f.alive[args[2]] {
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
