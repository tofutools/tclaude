package db

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedExitAuditSession(t *testing.T, id, conv string) string {
	t.Helper()
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: id, TmuxSession: "tmux-" + id, ConvID: conv,
		Status: "working", CreatedAt: time.Now().UTC(),
	}))
	return agentID
}

func TestAgentExitAudit_DeduplicatesAndOnlyEnriches(t *testing.T) {
	setupTestDB(t)
	agentID := seedExitAuditSession(t, "spwn-exit", "conv-exit")
	const generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-exit", generation))
	require.NoError(t, SetSessionExitLaunchBinding(
		"spwn-exit", generation, strings.Repeat("b", 64), "%7"))
	require.NoError(t, MarkSessionExitLaunchReleased("spwn-exit", generation))

	first, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-exit", Observer: AgentExitObserverReconcile,
		CauseKind: AgentExitCauseUnknown, ObservedState: "working", ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	assert.True(t, first.Inserted)
	assert.False(t, first.Enriched)
	require.True(t, validEventID(first.EventID))

	dup, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-exit", Observer: AgentExitObserverReconcile,
		CauseKind: AgentExitCauseUnknown, ObservedState: "working", ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	assert.Equal(t, first.EventID, dup.EventID)
	assert.False(t, dup.Inserted)
	assert.False(t, dup.Enriched)

	zero := 0
	richer, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-exit", Observer: AgentExitObserverHook,
		CauseKind: AgentExitCauseNormal, ExitCode: &zero,
		Reason: "logout", ObservedState: "exited", ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	assert.Equal(t, first.EventID, richer.EventID)
	assert.True(t, richer.Enriched)

	// A later poorer reconciliation observation cannot erase the hook evidence.
	poorer, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-exit", Observer: AgentExitObserverReaper,
		CauseKind: AgentExitCauseDisappeared, ObservedState: "unknown", ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	assert.False(t, poorer.Enriched)

	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, agentID, rows[0].TargetAgent)
	assert.Equal(t, AgentExitCauseNormal, rows[0].CauseKind)
	assert.Equal(t, AgentExitObserverHook, rows[0].Observer)
	require.NotNil(t, rows[0].ExitCode)
	assert.Equal(t, 0, *rows[0].ExitCode)
	assert.Equal(t, "logout", rows[0].Reason)
	assert.Contains(t, rows[0].Detail, "signal=unavailable")
}

func TestAgentExitAudit_CallbackBindingReplayAndRelaunch(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-callback", "conv-callback")
	const generation1 = "11111111111111111111111111111111"
	const token1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-callback", generation1))
	require.NoError(t, SetSessionExitLaunchBinding("spwn-callback", generation1, token1, "%9"))

	// The unauthenticated API may never impersonate the tmux observer.
	_, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback",
		PaneID: "%9", Observer: AgentExitObserverTmux,
		CauseKind: AgentExitCauseSignal, Signal: "TERM",
	})
	require.ErrorIs(t, err, ErrExitCallbackRejected)

	// Forged target/session and pane claims fail without consuming the proof.
	_, err = RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-forged", PaneID: "%9",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "TERM",
	}, ExitCallbackAuth{Generation: generation1, TokenHash: token1, PaneID: "%9"})
	require.ErrorIs(t, err, ErrExitCallbackRejected)
	_, err = RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback", PaneID: "%8",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "TERM",
	}, ExitCallbackAuth{Generation: generation1, TokenHash: token1, PaneID: "%9"})
	require.ErrorIs(t, err, ErrExitCallbackRejected)

	accepted, err := RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback", PaneID: "%9",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "term",
	}, ExitCallbackAuth{Generation: generation1, TokenHash: token1, PaneID: "%9"})
	require.NoError(t, err)
	assert.True(t, accepted.Inserted)

	_, err = RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback", PaneID: "%9",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "TERM",
	}, ExitCallbackAuth{Generation: generation1, TokenHash: token1, PaneID: "%9"})
	require.ErrorIs(t, err, ErrExitCallbackRejected, "replay must fail closed")

	// A relaunch replaces the binding and creates a distinct launch event. The
	// delayed predecessor callback is stale even though the session id is reused.
	_, err = SetSessionExitIntent("spwn-callback", AgentExitActionStop,
		"evt_1234567890abcdef12345678", time.Now())
	require.NoError(t, err)
	const generation2 = "22222222222222222222222222222222"
	const token2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-callback", generation2))
	require.NoError(t, SetSessionExitLaunchBinding("spwn-callback", generation2, token2, "%12"))
	_, err = RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback", PaneID: "%9",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "KILL",
	}, ExitCallbackAuth{Generation: generation1, TokenHash: token1, PaneID: "%9"})
	require.ErrorIs(t, err, ErrExitCallbackRejected)

	code := 7
	second, err := RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-callback", TmuxSession: "tmux-spwn-callback", PaneID: "%12",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseNormal, ExitCode: &code,
	}, ExitCallbackAuth{Generation: generation2, TokenHash: token2, PaneID: "%12"})
	require.NoError(t, err)
	assert.NotEqual(t, accepted.EventID, second.EventID)
	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "separate launch generations must never deduplicate")
	for _, row := range rows {
		if row.EventID == second.EventID {
			assert.Empty(t, row.LifecycleAction, "predecessor intent must not bleed into a relaunch")
			assert.Empty(t, row.RelatedEventID)
		}
	}
}

func TestAgentExitAudit_CallbackCanEnrichHookRace(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-race", "conv-race")
	const generation = "cccccccccccccccccccccccccccccccc"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-race", generation))
	require.NoError(t, SetSessionExitLaunchBinding(
		"spwn-race", generation, strings.Repeat("d", 64), "%21"))
	require.NoError(t, MarkSessionExitLaunchReleased("spwn-race", generation))

	code := 143
	hook, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-race", Observer: AgentExitObserverHook,
		CauseKind: AgentExitCauseNormal, ExitCode: &code,
		Reason: "logout", ObservedState: "exited", ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	require.True(t, hook.Inserted)

	callback, err := RecordAuthenticatedAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-race", TmuxSession: "tmux-spwn-race", PaneID: "%21",
		Observer: AgentExitObserverTmux, CauseKind: AgentExitCauseSignal, Signal: "HUP",
	}, ExitCallbackAuth{
		Generation: generation, TokenHash: strings.Repeat("d", 64), PaneID: "%21",
	})
	require.NoError(t, err)
	assert.True(t, callback.Enriched)
	assert.Equal(t, hook.EventID, callback.EventID)

	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, AgentExitCauseSignal, rows[0].CauseKind)
	assert.Equal(t, "HUP", rows[0].Signal)
	assert.Nil(t, rows[0].ExitCode, "signal evidence replaces, rather than combines with, an exit code")
	assert.Equal(t, AgentExitObserverTmux, rows[0].Observer)
}

func TestAgentExitAudit_ConcurrentHookAndReaperConverge(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-concurrent", "conv-concurrent")
	const generation = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	require.NoError(t, SetSessionExitLaunchGeneration(
		"spwn-concurrent", generation))

	start := make(chan struct{})
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			observation := AgentExitObservation{
				SessionID: "spwn-concurrent", Observer: AgentExitObserverReaper,
				CauseKind: AgentExitCauseDisappeared, ObservedState: "exited",
				ExpectedGeneration: generation,
			}
			if i == 0 {
				observation.Observer = AgentExitObserverHook
				observation.CauseKind = AgentExitCauseNormal
				observation.Reason = "logout"
			}
			_, err := RecordAgentExitObservation(observation)
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, AgentExitObserverHook, rows[0].Observer)
	assert.Equal(t, AgentExitCauseNormal, rows[0].CauseKind)
}

func TestSessionExitIntent_ClearAfterFailedAttemptContract(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-intent", "conv-intent")
	const eventID = "evt_1234567890abcdef12345678"
	_, err := SetSessionExitIntent("spwn-intent", AgentExitActionStop, eventID, time.Now())
	require.NoError(t, err)
	require.NoError(t, ClearSessionExitIntent("spwn-intent"))

	result, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-intent", Observer: AgentExitObserverReaper,
		CauseKind: AgentExitCauseDisappeared,
	})
	require.NoError(t, err)
	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, result.EventID, rows[0].EventID)
	assert.Empty(t, rows[0].LifecycleAction)
	assert.Empty(t, rows[0].RelatedEventID)
}

func TestSessionExitIntent_ExpiredIntentIsNotAttributed(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-expired-intent", "conv-expired-intent")
	_, err := SetSessionExitIntent("spwn-expired-intent", AgentExitActionStop,
		"evt_1234567890abcdef12345678", time.Now().Add(-agentExitIntentMaxAge-time.Minute))
	require.NoError(t, err)
	_, err = RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-expired-intent", Observer: AgentExitObserverReaper,
		CauseKind: AgentExitCauseDisappeared,
	})
	require.NoError(t, err)
	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].LifecycleAction)
	assert.Empty(t, rows[0].RelatedEventID)
}

func TestSessionExitIntent_CompareAndClearDoesNotEraseOverlappingOwner(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-overlap", "conv-overlap")
	const generation = "99999999999999999999999999999999"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-overlap", generation))
	first, err := SetSessionExitIntent("spwn-overlap", AgentExitActionStop,
		"evt_111111111111111111111111", time.Now())
	require.NoError(t, err)
	second, err := SetSessionExitIntent("spwn-overlap", AgentExitActionRetire,
		"evt_222222222222222222222222", time.Now())
	require.NoError(t, err)
	cleared, err := ClearSessionExitIntentIfCurrent(first)
	require.NoError(t, err)
	assert.False(t, cleared, "an older attempt cannot clear a newer action/event owner")

	_, err = RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-overlap", Observer: AgentExitObserverReaper,
		CauseKind: AgentExitCauseDisappeared, ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	rows, err := ListAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, second.Action, rows[0].LifecycleAction)
	assert.Equal(t, second.RelatedEventID, rows[0].RelatedEventID)
}

func TestClearSessionExitLaunchBinding_DoesNotClearSuccessor(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-clear-binding", "conv-clear-binding")
	const predecessor = "11111111111111111111111111111111"
	const successor = "22222222222222222222222222222222"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-clear-binding", predecessor))
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-clear-binding", successor))
	require.NoError(t, SetSessionExitLaunchBinding(
		"spwn-clear-binding", successor, strings.Repeat("a", 64), "%12"))

	require.NoError(t, ClearSessionExitLaunchBinding("spwn-clear-binding", predecessor))
	d, err := Open()
	require.NoError(t, err)
	var tokenHash, paneID string
	require.NoError(t, d.QueryRow(`SELECT exit_callback_token_hash, exit_callback_pane_id
		FROM sessions WHERE id = ?`, "spwn-clear-binding").Scan(&tokenHash, &paneID))
	assert.Equal(t, strings.Repeat("a", 64), tokenHash)
	assert.Equal(t, "%12", paneID)
}

func TestReaperExitCASAndAuditRejectPredecessorAfterRelaunch(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-stale-reaper", "conv-stale-reaper")
	const predecessor = "11111111111111111111111111111111"
	const successor = "22222222222222222222222222222222"
	require.NoError(t, SetSessionExitLaunchGeneration("spwn-stale-reaper", predecessor))
	observed, err := LoadSession("spwn-stale-reaper")
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "spwn-stale-reaper", TmuxSession: "tmux-spwn-stale-reaper",
		ConvID: "conv-stale-reaper", Status: "working", CreatedAt: time.Now().UTC(),
		ExitLaunchGeneration: successor, ExitLaunchGateState: SessionExitGateUngated,
	}))
	ok, _, err := MarkSessionExitedAndRecordObservationIfUnchanged(
		"spwn-stale-reaper", observed.Status, observed.UpdatedAt, "unexpected",
		AgentExitObservation{
			SessionID: "spwn-stale-reaper", Observer: AgentExitObserverReaper,
			CauseKind: AgentExitCauseDisappeared, ExpectedGeneration: predecessor,
		},
	)
	require.NoError(t, err)
	assert.False(t, ok)
	current, err := LoadSession("spwn-stale-reaper")
	require.NoError(t, err)
	assert.Equal(t, "working", current.Status)
	n, err := CountAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestAgentExitAudit_RejectsInvalidOrConflictingEvidence(t *testing.T) {
	setupTestDB(t)
	seedExitAuditSession(t, "spwn-invalid", "conv-invalid")
	code := 143
	_, err := RecordAgentExitObservation(AgentExitObservation{
		SessionID: "spwn-invalid", Observer: AgentExitObserverHook,
		CauseKind: AgentExitCauseSignal, ExitCode: &code, Signal: "TERM",
	})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrExitCallbackRejected))
	n, err := CountAuditLog(AuditLogFilter{Verb: AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Zero(t, n)
}
