package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The background-shell ledger (BgShellSet) is strictly lossier than the
// sub-agent one: Claude Code announces a launch and never an exit. These
// tests pin the behaviours the badge's honesty rests on — the TTL
// backstop, the Refresh that keeps a genuinely long-running shell from
// ageing out, and the anon fallback for a payload with no task id.

func TestBgShellSet_EncodeRoundTripsAndEmptyStaysEmpty(t *testing.T) {
	assert.Equal(t, "", BgShellSet(nil).Encode(), "nil encodes to the column default")
	assert.Equal(t, "", BgShellSet{}.Encode(), "an emptied ledger encodes to the column default")
	assert.Nil(t, ParseBgShellSet(""), "the column default decodes to an empty ledger")
	assert.Nil(t, ParseBgShellSet("{not json"), "malformed JSON is never a reason to fail a hook")

	now := time.Now().Truncate(time.Second)
	set := BgShellSet(nil).Add("task-1", "npm run dev", now)
	back := ParseBgShellSet(set.Encode())
	require.Len(t, back, 1)
	assert.Equal(t, "npm run dev", back["task-1"].Command)
	assert.True(t, back["task-1"].Seen.Equal(now), "Seen survives the round-trip")
}

func TestBgShellSet_TTLBoundsAGhost(t *testing.T) {
	now := time.Now()
	set := BgShellSet{
		"fresh": {Command: "npm run dev", Seen: now.Add(-time.Minute)},
		"ghost": {Command: "old build", Seen: now.Add(-BgShellTTL - time.Minute)},
	}

	// LiveCount must not mutate: the dashboard reads it on a row it may
	// not be able to write back.
	assert.Equal(t, 1, set.LiveCount(now), "the expired entry stops being displayed")
	assert.Len(t, set, 2, "LiveCount does not mutate the stored ledger")
	assert.Len(t, set.Live(now), 1, "Live() filters the same way")

	set.Sweep(now)
	assert.Len(t, set, 1, "Sweep drops it from storage")
	_, stillThere := set["ghost"]
	assert.False(t, stillThere)
}

// The reconcile re-stamps what it proved alive. Without that, a dev
// server left running for longer than BgShellTTL would silently stop
// being badged even though the process is right there.
func TestBgShellSet_RefreshKeepsAProvenAliveShellFromExpiring(t *testing.T) {
	now := time.Now()
	old := now.Add(-BgShellTTL + time.Minute)
	set := BgShellSet{"task-1": {Command: "npm run dev", Seen: old}}

	assert.True(t, set.Refresh("task-1", now), "a newer stamp is a change")
	assert.False(t, set.Refresh("task-1", old), "going backwards in time is not")
	assert.False(t, set.Refresh("unknown", now), "the reconcile never invents entries")
	assert.Len(t, set, 1)

	set.Sweep(now.Add(BgShellTTL - time.Minute))
	assert.Len(t, set, 1, "the refreshed entry outlives the original TTL window")
}

func TestBgShellSet_RemoveByIDAndEmptyIDFallback(t *testing.T) {
	now := time.Now()
	set := BgShellSet(nil).
		Add("task-1", "first", now.Add(-time.Minute)).
		Add("task-2", "second", now)

	set.Remove("nope")
	assert.Len(t, set, 2, "an unknown id is a no-op, not a blind decrement")

	set.Remove("task-2")
	assert.Len(t, set, 1)
	_, ok := set["task-1"]
	assert.True(t, ok, "Remove took the named entry")

	// An empty id is a TaskStop whose payload lacked a task_id: a kill
	// that definitely happened, so drop the oldest rather than leak.
	set.Remove("")
	assert.Empty(t, set)
	set.Remove("") // safe on an empty ledger
}

// A tool_response with no backgroundTaskId still has to count: the launch
// demonstrably happened. Anon entries are keyed uniquely so two of them
// never collapse into one.
func TestBgShellSet_AnonEntriesAreDistinctAndDroppedFirst(t *testing.T) {
	now := time.Now()
	set := BgShellSet(nil).Add("", "one", now).Add("", "two", now)
	require.Len(t, set, 2, "same-nanosecond anon adds must not collide")
	for id := range set {
		assert.True(t, strings.HasPrefix(id, "anon-"), "unexpected key %q", id)
	}

	set = set.Add("task-real", "three", now.Add(-time.Hour))
	set.Remove("")
	assert.Len(t, set, 2)
	_, keptReal := set["task-real"]
	assert.True(t, keptReal, "the empty-id fallback prefers anon entries even when a real one is older")
}

// The ledger is re-serialised onto the sessions row on every hook tick, so
// an agent must not be able to bloat that write path with one pathological
// command.
func TestBgShellSet_CommandIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 10_000)
	set := BgShellSet(nil).Add("task-1", huge, time.Now())
	assert.LessOrEqual(t, len(set["task-1"].Command), bgShellCommandMax)
	assert.True(t, strings.HasPrefix(huge, set["task-1"].Command), "the kept part is a prefix")

	// Truncation must not split a rune, or the stored JSON stops being
	// valid UTF-8 and the whole ledger fails to decode.
	multibyte := strings.Repeat("é", 10_000)
	set = BgShellSet(nil).Add("task-2", multibyte, time.Now())
	round := ParseBgShellSet(set.Encode())
	require.Len(t, round, 1, "a truncated multi-byte command still round-trips")
	assert.True(t, strings.HasPrefix(multibyte, round["task-2"].Command))
}
