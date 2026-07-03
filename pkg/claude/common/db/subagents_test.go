package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSubagentSet_EncodeParseRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second) // RFC3339 JSON round-trips to the second cleanly

	assert.Equal(t, "", SubagentSet(nil).Encode(), "nil set encodes to the column default")
	assert.Equal(t, "", SubagentSet{}.Encode(), "empty set encodes to the column default")
	assert.Nil(t, ParseSubagentSet(""), "column default parses to an empty set")
	assert.Nil(t, ParseSubagentSet("{not json"), "malformed JSON degrades to an empty set, never an error")

	set := SubagentSet{
		"ag-1": {Type: "Explore", Seen: now},
		"ag-2": {Seen: now.Add(-time.Minute)},
	}
	got := ParseSubagentSet(set.Encode())
	assert.Len(t, got, 2)
	assert.Equal(t, "Explore", got["ag-1"].Type)
	assert.True(t, got["ag-2"].Seen.Equal(now.Add(-time.Minute)))
}

func TestSubagentSet_SweepAndLiveCount(t *testing.T) {
	now := time.Now()
	set := SubagentSet{
		"ag-live":    {Seen: now.Add(-time.Minute)},
		"ag-edge":    {Seen: now.Add(-SubagentTTL)}, // exactly at the TTL: still live
		"ag-phantom": {Seen: now.Add(-SubagentTTL - time.Second)},
	}

	assert.Equal(t, 2, set.LiveCount(now), "LiveCount filters without mutating")
	assert.Len(t, set, 3, "LiveCount must not mutate the set")

	set.Sweep(now)
	assert.Len(t, set, 2)
	assert.NotContains(t, set, "ag-phantom")

	// nil-safety: both are read/mutate paths hooks hit on every event.
	assert.Equal(t, 0, SubagentSet(nil).LiveCount(now))
	SubagentSet(nil).Sweep(now) // must not panic
}

func TestSubagentSet_AddSightRemove(t *testing.T) {
	now := time.Now()

	// Add with an id, and allocate-on-nil.
	var set SubagentSet
	set = set.Add("ag-1", "Explore", now)
	assert.Len(t, set, 1)

	// Id-less Adds get unique synthetic keys.
	set = set.Add("", "", now)
	set = set.Add("", "", now)
	assert.Len(t, set, 3, "two anon entries must not collide")

	// Sight of a NEW id consumes the oldest anon placeholder (that anon
	// entry was this sub-agent, counted at an id-less Start).
	set = set.Sight("ag-2", "Plan", now.Add(time.Second))
	assert.Len(t, set, 3, "sighted id replaces an anon entry, not added on top")
	assert.Contains(t, set, "ag-2")

	// Sight of a KNOWN id refreshes Seen and consumes nothing.
	set = set.Sight("ag-1", "Explore", now.Add(2*time.Second))
	assert.Len(t, set, 3)
	assert.True(t, set["ag-1"].Seen.After(now), "sight refreshes last-seen")

	// Sight with no id is a no-op (nothing to key on).
	set = set.Sight("", "Explore", now)
	assert.Len(t, set, 3)

	// Remove: unknown non-empty id is a no-op; empty id removes the
	// (one remaining) anon entry first; known id removes exactly itself.
	set.Remove("ag-never")
	assert.Len(t, set, 3, "unknown-id remove is a no-op")
	set.Remove("")
	assert.Len(t, set, 2)
	assert.Contains(t, set, "ag-1")
	assert.Contains(t, set, "ag-2", "anon-first: identified entries survive an id-less remove")
	set.Remove("ag-1")
	assert.NotContains(t, set, "ag-1")

	// Id-less remove with no anon entries falls back to the oldest entry.
	set.Remove("")
	assert.Empty(t, set)
	set.Remove("") // and is nil/empty-safe
}
