package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The monitor ledger (MonitorSet) is the background-shell ledger's sibling
// with one extra retirement signal: a non-persistent watch carries the
// deadline the harness itself will enforce. These tests pin the behaviours
// the "👁+N" badge's honesty rests on — that deadline, the TTL backstop,
// the Refresh that keeps a long-running watch from ageing out (without
// extending its deadline), and the websocket flag the liveness reconcile
// reads to decline having an opinion.

func TestMonitorSet_EncodeRoundTripsAndEmptyStaysEmpty(t *testing.T) {
	assert.Equal(t, "", MonitorSet(nil).Encode(), "nil encodes to the column default")
	assert.Equal(t, "", MonitorSet{}.Encode(), "an emptied ledger encodes to the column default")
	assert.Nil(t, ParseMonitorSet(""), "the column default decodes to an empty ledger")
	assert.Nil(t, ParseMonitorSet("{not json"), "malformed JSON is never a reason to fail a hook")

	now := time.Now().Truncate(time.Second)
	deadline := now.Add(5 * time.Minute)
	set := MonitorSet(nil).Add("task-1", "tail -f build.log", "errors in build.log", false, now, deadline)
	back := ParseMonitorSet(set.Encode())
	require.Len(t, back, 1)
	assert.Equal(t, "tail -f build.log", back["task-1"].Command)
	assert.Equal(t, "errors in build.log", back["task-1"].Label)
	assert.False(t, back["task-1"].WS)
	assert.True(t, back["task-1"].Seen.Equal(now), "Seen survives the round-trip")
	assert.True(t, back["task-1"].Deadline.Equal(deadline), "Deadline survives the round-trip")
}

func TestMonitorSet_PersistentEntryEncodesWithoutADeadline(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	set := MonitorSet(nil).Add("task-1", "tail -f app.log", "app errors", false, now, time.Time{})
	assert.NotContains(t, set.Encode(), "deadline",
		"a persistent watch has no deadline, so the field stays out of the stored JSON")
	assert.True(t, set["task-1"].live(now.Add(MonitorTTL-time.Minute)),
		"an unbounded watch is bounded only by the TTL")
}

func TestMonitorSet_DeadlineRetiresBeforeTheTTLWould(t *testing.T) {
	now := time.Now()
	set := MonitorSet{
		"timed":      {Command: "gh pr checks 1", Seen: now, Deadline: now.Add(time.Minute)},
		"persistent": {Command: "tail -f app.log", Seen: now},
	}

	assert.Equal(t, 2, set.LiveCount(now), "both are running now")
	// Well inside the TTL, but past the deadline the harness enforces.
	past := now.Add(2 * time.Minute)
	assert.Equal(t, 1, set.LiveCount(past), "the timed watch is over even though the TTL has not expired")
	assert.Len(t, set, 2, "LiveCount must not mutate: the dashboard may not be able to write back")

	set.Sweep(past)
	assert.Len(t, set, 1)
	_, stillThere := set["timed"]
	assert.False(t, stillThere)
}

func TestMonitorSet_TTLBoundsAGhostWithNoDeadline(t *testing.T) {
	now := time.Now()
	set := MonitorSet{
		"fresh": {Command: "tail -f a.log", Seen: now.Add(-time.Minute)},
		"ghost": {Command: "tail -f b.log", Seen: now.Add(-MonitorTTL - time.Minute)},
	}
	assert.Equal(t, 1, set.LiveCount(now))
	assert.Len(t, set.Live(now), 1)

	set.Sweep(now)
	require.Len(t, set, 1)
	_, kept := set["fresh"]
	assert.True(t, kept)
}

func TestMonitorSet_RefreshKeepsAProvenAliveWatchFromExpiringButNotItsDeadline(t *testing.T) {
	now := time.Now()
	deadline := now.Add(30 * time.Minute)
	set := MonitorSet{
		"watch": {Command: "tail -f app.log", Seen: now.Add(-MonitorTTL + time.Minute), Deadline: deadline},
	}

	assert.True(t, set.Refresh("watch", now), "a positive liveness verdict re-stamps the entry")
	assert.True(t, set["watch"].Seen.Equal(now))
	assert.True(t, set["watch"].Deadline.Equal(deadline),
		"the harness-enforced deadline is absolute and must not be extended by liveness")

	assert.False(t, set.Refresh("watch", now.Add(-time.Hour)), "never re-stamp backwards")
	assert.False(t, set.Refresh("unknown", now), "the reconcile never invents entries")
}

func TestMonitorSet_HasRoutesAStopToTheOwningLedger(t *testing.T) {
	now := time.Now()
	set := MonitorSet{"mon-1": {Command: "tail -f a.log", Seen: now}}
	assert.True(t, set.Has("mon-1"))
	assert.False(t, set.Has("shell-1"), "a task id this ledger does not own is not claimed")
	assert.False(t, set.Has(""), "an id-less stop is never routed here")
	assert.False(t, MonitorSet(nil).Has("mon-1"))
}

func TestMonitorSet_RemoveByIDAndEmptyIDFallback(t *testing.T) {
	now := time.Now()
	set := MonitorSet{
		"mon-1": {Command: "tail -f a.log", Seen: now.Add(-time.Minute)},
		"mon-2": {Command: "tail -f b.log", Seen: now},
	}

	set.Remove("unknown")
	assert.Len(t, set, 2, "an unknown non-empty id is a no-op")

	set.Remove("mon-2")
	assert.Len(t, set, 1)

	// An empty id drops the oldest rather than leaking an entry the ledger
	// knows ended.
	set.Remove("")
	assert.Empty(t, set)

	MonitorSet(nil).Remove("mon-1") // must not panic
}

func TestMonitorSet_AnonEntriesAreDistinctAndDroppedFirst(t *testing.T) {
	now := time.Now()
	set := MonitorSet(nil).Add("", "tail -f a.log", "a", false, now, time.Time{})
	set = set.Add("", "tail -f b.log", "b", false, now, time.Time{})
	require.Len(t, set, 2, "two payloads with no taskId at the same instant get distinct keys")

	set = set.Add("keyed", "tail -f c.log", "c", false, now.Add(-time.Hour), time.Time{})
	set.Remove("")
	assert.Len(t, set, 2)
	_, keyedKept := set["keyed"]
	assert.True(t, keyedKept, "anon entries are dropped before an older keyed one")
}

func TestMonitorSet_CommandAndLabelAreBounded(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("x", monitorCommandMax*2)
	set := MonitorSet(nil).Add("mon-1", long, long, false, now, time.Time{})
	assert.Len(t, set["mon-1"].Command, monitorCommandMax)
	assert.Len(t, set["mon-1"].Label, monitorCommandMax)
}

func TestMonitorSet_WSEntryCarriesNoCommand(t *testing.T) {
	now := time.Now()
	set := MonitorSet(nil).Add("mon-1", "", "wss://events.example.com/stream", true, now, time.Time{})
	require.Len(t, set, 1)
	assert.True(t, set["mon-1"].WS, "the reconcile reads this to decline having an opinion")
	assert.Equal(t, "", set["mon-1"].Command)
	assert.Equal(t, "wss://events.example.com/stream", set["mon-1"].Label)
}
