package agentd

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lineageEntriesForTest(count int) []epochV8LineageEntryDTO {
	entries := make([]epochV8LineageEntryDTO, 0, count)
	for index := range count {
		entries = append(entries, epochV8LineageEntryDTO{Ordinal: uint64(index), TemplateRef: "tmpl@sha256:" + strconv.Itoa(index)})
	}
	return entries
}

func TestBoundEpochV8LineageEntriesKeepsShortChainsIntact(t *testing.T) {
	for _, count := range []int{1, 2, maxEpochV8LineageEntries} {
		entries := lineageEntriesForTest(count)
		bounded, truncated := boundEpochV8LineageEntries(entries)
		assert.False(t, truncated, count)
		assert.Equal(t, entries, bounded, count)
	}
}

func TestBoundEpochV8LineageEntriesTruncatesLongChainsToFirstAndLastHalves(t *testing.T) {
	entries := lineageEntriesForTest(40)
	bounded, truncated := boundEpochV8LineageEntries(entries)
	require.True(t, truncated)
	require.Len(t, bounded, maxEpochV8LineageEntries)
	half := maxEpochV8LineageEntries / 2
	assert.Equal(t, entries[:half], bounded[:half], "first half keeps the original oldest epochs")
	assert.Equal(t, entries[len(entries)-half:], bounded[half:], "second half keeps the newest epochs")
}
