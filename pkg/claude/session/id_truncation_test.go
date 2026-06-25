package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, "d0e9fa14", shortTmuxBase(full, ""), "a long id renders as its 8-char prefix")
	assert.Equal(t, "spwn-ab12cd", shortTmuxBase(full, "spwn-ab12cd"), "an explicit label wins verbatim, never truncated")
	assert.Equal(t, "abc", shortTmuxBase("abc", ""), "an already-short id is left as-is")
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
