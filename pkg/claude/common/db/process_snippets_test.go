package db

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessSnippetsCRUDOrderingAndCAS(t *testing.T) {
	setupTestDB(t)

	beta, generation, err := CreateProcessSnippet("Beta", "beta", `{"kind":"beta"}`)
	require.NoError(t, err)
	assert.Equal(t, int64(1), generation)
	alpha, generation, err := CreateProcessSnippet("Alpha", "alpha", `{"kind":"alpha"}`)
	require.NoError(t, err)
	assert.Equal(t, int64(2), generation)

	library, err := ListProcessSnippets()
	require.NoError(t, err)
	assert.Equal(t, int64(2), library.Generation)
	require.Len(t, library.Snippets, 2)
	assert.Equal(t, []string{alpha.ID, beta.ID}, []string{library.Snippets[0].ID, library.Snippets[1].ID})

	renamed, generation, err := RenameProcessSnippet(beta.ID, "Aardvark", "aardvark", beta.Revision)
	require.NoError(t, err)
	assert.Equal(t, int64(2), renamed.Revision)
	assert.Equal(t, int64(3), generation)
	_, _, err = RenameProcessSnippet(beta.ID, "Stale", "stale", beta.Revision)
	assert.ErrorIs(t, err, ErrProcessSnippetConflict)
	_, _, err = RenameProcessSnippet("psn_00000000000000000000000000000000", "Missing", "missing", 1)
	assert.ErrorIs(t, err, ErrProcessSnippetNotFound)

	_, _, err = RenameProcessSnippet(alpha.ID, "AARDVARK", "aardvark", alpha.Revision)
	assert.ErrorIs(t, err, ErrProcessSnippetNameExists)
	generation, err = DeleteProcessSnippet(beta.ID, renamed.Revision)
	require.NoError(t, err)
	assert.Equal(t, int64(4), generation)
	_, err = DeleteProcessSnippet(beta.ID, renamed.Revision)
	assert.ErrorIs(t, err, ErrProcessSnippetNotFound)
}

func TestProcessSnippetsAggregateLimitExactAndPlusOne(t *testing.T) {
	setupTestDB(t)
	payload := strings.Repeat("x", MaxProcessSnippetEnvelopeBytes)
	for index := 0; index < MaxProcessSnippetAggregateBytes/MaxProcessSnippetEnvelopeBytes; index++ {
		_, _, err := CreateProcessSnippet(fmt.Sprintf("item-%02d", index), fmt.Sprintf("item-%02d", index), payload)
		require.NoError(t, err, "item %d must fill exact aggregate boundary", index)
	}
	_, _, err := CreateProcessSnippet("one-byte-over", "one-byte-over", "x")
	assert.ErrorIs(t, err, ErrProcessSnippetByteLimit)
	library, err := ListProcessSnippets()
	require.NoError(t, err)
	assert.Len(t, library.Snippets, MaxProcessSnippetAggregateBytes/MaxProcessSnippetEnvelopeBytes)
}

func TestProcessSnippetsCountLimit(t *testing.T) {
	setupTestDB(t)
	for index := 0; index < MaxProcessSnippetCount; index++ {
		_, _, err := CreateProcessSnippet(fmt.Sprintf("item-%03d", index), fmt.Sprintf("item-%03d", index), "x")
		require.NoError(t, err)
	}
	_, _, err := CreateProcessSnippet("overflow", "overflow", "x")
	assert.ErrorIs(t, err, ErrProcessSnippetCountLimit)
}

func TestProcessSnippetsConcurrentQuotaAndCAS(t *testing.T) {
	setupTestDB(t)
	// Leave exactly one byte of aggregate capacity.
	chunk := strings.Repeat("x", MaxProcessSnippetEnvelopeBytes)
	for index := 0; index < MaxProcessSnippetAggregateBytes/MaxProcessSnippetEnvelopeBytes-1; index++ {
		_, _, err := CreateProcessSnippet(fmt.Sprintf("seed-%02d", index), fmt.Sprintf("seed-%02d", index), chunk)
		require.NoError(t, err)
	}
	remaining := MaxProcessSnippetAggregateBytes - (MaxProcessSnippetAggregateBytes/MaxProcessSnippetEnvelopeBytes-1)*MaxProcessSnippetEnvelopeBytes - 1
	_, _, err := CreateProcessSnippet("almost-full", "almost-full", strings.Repeat("y", remaining))
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _, err := CreateProcessSnippet(fmt.Sprintf("racer-%d", index), fmt.Sprintf("racer-%d", index), "z")
			results <- err
		}(index)
	}
	wg.Wait()
	close(results)
	var successes, limited int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrProcessSnippetByteLimit):
			limited++
		default:
			t.Fatalf("unexpected concurrent quota result: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, limited)

	// Two writers with the same revision cannot both rename one item.
	_, _, err = CreateProcessSnippet("cas", "cas", "x")
	assert.ErrorIs(t, err, ErrProcessSnippetByteLimit, "aggregate remains full")
	// Free one existing row, then create the CAS target.
	library, err := ListProcessSnippets()
	require.NoError(t, err)
	_, err = DeleteProcessSnippet(library.Snippets[0].ID, library.Snippets[0].Revision)
	require.NoError(t, err)
	item, _, err := CreateProcessSnippet("cas", "cas", "x")
	require.NoError(t, err)

	results = make(chan error, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _, err := RenameProcessSnippet(item.ID, fmt.Sprintf("cas-%d", index), fmt.Sprintf("cas-%d", index), item.Revision)
			results <- err
		}(index)
	}
	wg.Wait()
	close(results)
	successes, limited = 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrProcessSnippetConflict) {
			limited++
		} else {
			t.Fatalf("unexpected concurrent CAS result: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, limited)
}

func TestProcessSnippetsCorruptIDsAndRenamePayloadStayBounded(t *testing.T) {
	setupTestDB(t)
	database, err := Open()
	require.NoError(t, err)
	now := "2026-07-18T00:00:00Z"
	for index, id := range []string{
		"psn_invalid",
		"psn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"psn_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		_, err := database.Exec(`INSERT INTO process_snippets
			(id, name, name_key, envelope_json, revision, created_at, updated_at)
			VALUES (?, ?, ?, '{}', 1, ?, ?)`, id, fmt.Sprintf("bad-%d", index), fmt.Sprintf("bad-%d", index), now, now)
		require.NoError(t, err)
	}
	oversizedID := "psn_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oversized := strings.Repeat("x", MaxProcessSnippetEnvelopeBytes+1)
	_, err = database.Exec(`INSERT INTO process_snippets
		(id, name, name_key, envelope_json, revision, created_at, updated_at)
		VALUES (?, 'oversized', 'oversized', ?, 1, ?, ?)`, oversizedID, oversized, now, now)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO process_snippets
		(id, name, name_key, envelope_json, revision, created_at, updated_at)
		VALUES ('psn_cccccccccccccccccccccccccccccccc', ?, 'oversized-name', '{}', 1, ?, ?)`,
		strings.Repeat("n", MaxProcessSnippetNameBytes+1), now, now)
	require.NoError(t, err)

	library, err := ListProcessSnippets()
	require.NoError(t, err)
	require.Len(t, library.Snippets, 5)
	for _, snippet := range library.Snippets {
		assert.True(t, snippet.Corrupt, snippet.ID)
	}

	renamed, _, err := RenameProcessSnippet(oversizedID, "renamed", "renamed", 1)
	require.NoError(t, err)
	assert.True(t, renamed.Corrupt)
	assert.Empty(t, renamed.EnvelopeJSON, "rename must not materialize an oversized corrupt payload")
}
