package db

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinRouteLaunchIsExactGenerationBound(t *testing.T) {
	setupTestDB(t)
	agentID := NewAgentID()
	require.NoError(t, RegisterDarwinRouteLaunch(agentID, "conv-a", "gen-a", []int{41001, 41002}))
	launch, err := GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchPending, launch.State)
	require.Equal(t, []int{41001, 41002}, launch.Slots)
	require.NoError(t, ActivateDarwinRouteLaunch(agentID, "conv-a", "gen-a"))
	launch, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchActive, launch.State)

	// A relaunch generation cannot observe or mutate its predecessor's
	// contract, even when the stable agent and conversation are unchanged.
	_, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-b")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Error(t, ActivateDarwinRouteLaunch(agentID, "conv-a", "gen-b"))

	d, err := Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, MarkDarwinRouteLaunchClosedTx(tx, agentID, "conv-a", "gen-a", time.Now()))
	require.NoError(t, tx.Commit())
	launch, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchClosed, launch.State)
}

func TestDarwinRouteLaunchSlotsAreCollisionExclusiveAndGenerationScoped(t *testing.T) {
	setupTestDB(t)
	firstAgent, secondAgent := NewAgentID(), NewAgentID()
	require.NoError(t, RegisterDarwinRouteLaunch(firstAgent, "conv-a", "gen-a", []int{42001, 42002}))
	// Pending claims reserve the slot before a launch can render or bind.
	require.Error(t, RegisterDarwinRouteLaunch(secondAgent, "conv-b", "gen-b", []int{42002}))
	require.NoError(t, ActivateDarwinRouteLaunch(firstAgent, "conv-a", "gen-a"))
	// An incorrect generation cannot release a newer launch's claims.
	require.NoError(t, DeleteDarwinRouteLaunch(firstAgent, "conv-a", "stale-generation"))
	require.Error(t, RegisterDarwinRouteLaunch(secondAgent, "conv-b", "gen-b", []int{42002}))
	require.NoError(t, DeleteDarwinRouteLaunch(firstAgent, "conv-a", "gen-a"))
	require.NoError(t, RegisterDarwinRouteLaunch(secondAgent, "conv-b", "gen-b", []int{42002}))
}

func TestDarwinRouteLaunchConcurrentSlotClaimsHaveOneWinner(t *testing.T) {
	setupTestDB(t)
	type result struct{ err error }
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, agentID := range []string{NewAgentID(), NewAgentID()} {
		wg.Add(1)
		go func(i int, agentID string) {
			defer wg.Done()
			results <- result{RegisterDarwinRouteLaunch(agentID, "conv-concurrent", "gen-"+string(rune('a'+i)), []int{42101})}
		}(i, agentID)
	}
	wg.Wait()
	close(results)
	winners := 0
	losers := 0
	for got := range results {
		if got.err == nil {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("concurrent slot claims winners=%d losers=%d, want one each", winners, losers)
	}
}
