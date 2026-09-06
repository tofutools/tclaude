package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/platform/conversation"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

func seedManagedBindingSession(t *testing.T, sessionID, generation, tmuxSession, paneID, harness string, pid int) {
	t.Helper()
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, TmuxSession: tmuxSession, PID: pid, Harness: harness,
		Status: "working", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, generation))
	if paneID != "" {
		require.NoError(t, SetSessionExitLaunchBinding(sessionID, generation, strings.Repeat("a", 64), paneID))
	}
}

func managedBindingAdmission(sessionID, generation, evidenceID, externalRef string, expected conversation.Revision) conversation.Admission {
	return conversation.Admission{
		Origin: conversation.Managed,
		Attempt: execution.AttemptRef{
			ExecutionID:     execution.ID(generation),
			LegacySessionID: sessionID,
		},
		ExpectedRevision: expected,
		Reference: conversation.Reference{
			Harness: "claude", Namespace: "state-root:test", Value: externalRef,
		},
		Transition: conversation.Continue,
		Evidence: conversation.Evidence{
			ID: evidenceID, ProcessInstance: "pid-start:4242:99", MainProcess: true,
			Strength: conversation.VerifiedMainProcess, Source: conversation.HostAdapter,
			PID: 4242, TmuxSession: "tmux-managed", PaneID: "%7",
		},
	}
}

func bindingTableCounts(t *testing.T) (logical, current, history int) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT count(*) FROM logical_conversations`).Scan(&logical))
	require.NoError(t, d.QueryRow(`SELECT count(*) FROM conversation_attempt_bindings`).Scan(&current))
	require.NoError(t, d.QueryRow(`SELECT count(*) FROM conversation_reference_bindings`).Scan(&history))
	return logical, current, history
}

func TestAdmitConversationBindingRejectsStaleGenerationWithoutWrites(t *testing.T) {
	setupTestDB(t)
	const (
		storedGeneration = "11111111111111111111111111111111"
		staleGeneration  = "22222222222222222222222222222222"
	)
	seedManagedBindingSession(t, "spwn-managed", storedGeneration, "tmux-managed", "%7", "claude", 4242)

	a := managedBindingAdmission("spwn-managed", staleGeneration, "evidence-stale", "ref-a", 0)
	decision, err := AdmitConversationBinding(a)
	require.NoError(t, err)
	assert.Equal(t, conversation.Rejected, decision.Outcome)
	assert.Contains(t, decision.Reason, "generation")
	assert.Equal(t, []int{0, 0, 0}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingClassifiesMissingDurableAttachmentAsAmbiguous(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "", "claude", 4242)

	decision, err := AdmitConversationBinding(managedBindingAdmission("spwn-managed", generation, "evidence-missing-pane", "ref-a", 0))
	require.NoError(t, err)
	assert.Equal(t, conversation.Ambiguous, decision.Outcome)
	assert.Equal(t, []int{0, 0, 0}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingCASReplayAndImmutableHistory(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)

	first := managedBindingAdmission("spwn-managed", generation, "evidence-1", "ref-a", 0)
	accepted, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	assert.Equal(t, conversation.Accepted, accepted.Outcome)
	assert.True(t, accepted.Changed)
	assert.Equal(t, conversation.Revision(1), accepted.Selection.Revision)
	require.NotEmpty(t, accepted.Selection.Conversation)

	duplicate, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	assert.Equal(t, conversation.Duplicate, duplicate.Outcome)
	assert.False(t, duplicate.Changed)
	assert.Equal(t, accepted.Selection, duplicate.Selection)

	changedAtSameExpected := managedBindingAdmission("spwn-managed", generation, "evidence-2", "ref-b", 0)
	conflict, err := AdmitConversationBinding(changedAtSameExpected)
	require.NoError(t, err)
	assert.Equal(t, conversation.Conflict, conflict.Outcome)
	assert.Equal(t, conversation.Revision(1), conflict.Selection.Revision)

	changedReplay := first
	changedReplay.Reference.Value = "ref-forged-replay"
	conflict, err = AdmitConversationBinding(changedReplay)
	require.NoError(t, err)
	assert.Equal(t, conversation.Conflict, conflict.Outcome)
	assert.Contains(t, conflict.Reason, "replay key")

	next := managedBindingAdmission("spwn-managed", generation, "evidence-3", "ref-b", 1)
	advanced, err := AdmitConversationBinding(next)
	require.NoError(t, err)
	assert.Equal(t, conversation.Accepted, advanced.Outcome)
	assert.Equal(t, conversation.Revision(2), advanced.Selection.Revision)
	assert.Equal(t, accepted.Selection.Conversation, advanced.Selection.Conversation, "continue preserves logical history")
	currentAdmission, found, err := CurrentConversationAdmission(execution.ID(generation))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, next, currentAdmission)
	currentSelection, found, err := CurrentConversationSelection(execution.ID(generation))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, advanced.Selection, currentSelection)

	historical, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	assert.Equal(t, conversation.Historical, historical.Outcome)
	assert.Equal(t, conversation.Revision(1), historical.Selection.Revision)

	d, err := Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT revision, expected_revision, evidence_id, external_ref, conversation_id
		FROM conversation_reference_bindings ORDER BY revision`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	type historyRow struct {
		revision, expected          int
		evidence, ref, conversation string
	}
	var history []historyRow
	for rows.Next() {
		var row historyRow
		require.NoError(t, rows.Scan(&row.revision, &row.expected, &row.evidence, &row.ref, &row.conversation))
		history = append(history, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []historyRow{
		{revision: 1, expected: 0, evidence: "evidence-1", ref: "ref-a", conversation: string(accepted.Selection.Conversation)},
		{revision: 2, expected: 1, evidence: "evidence-3", ref: "ref-b", conversation: string(accepted.Selection.Conversation)},
	}, history)
}

func TestAdmitConversationBindingResumeRequiresExactOperationAndPreservesLogicalConversation(t *testing.T) {
	setupTestDB(t)
	const (
		predecessor = "31313131313131313131313131313131"
		successor   = "32323232323232323232323232323232"
		convID      = "resume-logical-conv"
	)
	seedManagedBindingSession(t, "resume-predecessor", predecessor, "tmux-managed", "%7", "claude", 4242)
	first := managedBindingAdmission("resume-predecessor", predecessor, "resume-predecessor-evidence", convID, 0)
	accepted, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, accepted.Outcome)
	preRow, err := LoadSession("resume-predecessor")
	require.NoError(t, err)
	_, err = MarkSessionExitedIfUnchanged(preRow.ID, preRow.Status, preRow.UpdatedAt, "resume")
	require.NoError(t, err)

	seedManagedBindingSession(t, "resume-successor", successor, "tmux-managed", "%7", "claude", 4242)
	opID := execution.NewOperationID()
	secret := []byte("resume-logical-secret")
	require.NoError(t, CreateResumeOperation(ResumeOperationRow{
		ID: opID, Kind: "manual_resume", ConvID: convID,
		Attempt:               execution.AttemptRef{ExecutionID: execution.ID(successor)},
		LogicalConversationID: string(accepted.Selection.Conversation), ClaimHash: ResumeClaimHash(secret),
		State: execution.ResumeRequested, LaunchPhase: "requested", Revision: 1,
	}))
	require.NoError(t, TransitionResumeOperation(opID, 1, execution.ResumeAccepted, "accepted", ""))
	claimed, err := ClaimResumeOperation(opID, execution.ID(successor), secret, convID, "resume-successor", 77, "start-77")
	require.NoError(t, err)
	require.True(t, claimed)
	registered, err := RegisterResumeLaunch(opID, execution.ID(successor), convID, "resume-successor", "tmux-managed", "%7", "/private/resume-gate", 77, "start-77")
	require.NoError(t, err)
	require.True(t, registered)
	granted, err := GrantResumeRelease(opID, execution.ID(successor), "resume-successor", "tmux-managed", "%7", "/private/resume-gate")
	require.NoError(t, err)
	require.True(t, granted)
	started, err := MarkResumeReleased(opID, execution.ID(successor), "resume-successor", "tmux-managed", "%7")
	require.NoError(t, err)
	require.True(t, started)

	resume := managedBindingAdmission("resume-successor", successor, "resume-successor-evidence", convID, 0)
	resume.Transition = conversation.Resume
	ready, err := AdmitConversationBinding(resume)
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, ready.Outcome)
	assert.Equal(t, accepted.Selection.Conversation, ready.Selection.Conversation)
	op, err := GetResumeOperation(opID)
	require.NoError(t, err)
	require.NotNil(t, op)
	assert.Equal(t, execution.ResumeReady, op.State)
}

func TestBindManagedAttemptMainPIDIsGenerationAndExitFenced(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4000)
	attempt := execution.AttemptRef{ExecutionID: execution.ID(generation), LegacySessionID: "spwn-managed"}

	bound, err := BindManagedAttemptMainPID(attempt, "tmux-managed", "%7", 4000, 4242)
	require.NoError(t, err)
	require.True(t, bound)
	row, err := LoadSession("spwn-managed")
	require.NoError(t, err)
	assert.Equal(t, 4242, row.PID)

	stale := attempt
	stale.ExecutionID = execution.ID("22222222222222222222222222222222")
	bound, err = BindManagedAttemptMainPID(stale, "tmux-managed", "%7", 4242, 5000)
	require.NoError(t, err)
	assert.False(t, bound)

	exited, err := MarkSessionExitedIfUnchanged(row.ID, row.Status, row.UpdatedAt, "unexpected")
	require.NoError(t, err)
	require.True(t, exited)
	bound, err = BindManagedAttemptMainPID(attempt, "tmux-managed", "%7", 4242, 5000)
	require.NoError(t, err)
	assert.False(t, bound)
}

func TestAdmitConversationBindingClearMintsNextConversation(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)

	first, err := AdmitConversationBinding(managedBindingAdmission("spwn-managed", generation, "evidence-1", "ref-a", 0))
	require.NoError(t, err)
	clear := managedBindingAdmission("spwn-managed", generation, "evidence-clear", "ref-b", 1)
	clear.Transition = conversation.Clear
	second, err := AdmitConversationBinding(clear)
	require.NoError(t, err)
	assert.Equal(t, conversation.Accepted, second.Outcome)
	assert.Equal(t, conversation.Revision(2), second.Selection.Revision)
	assert.NotEqual(t, first.Selection.Conversation, second.Selection.Conversation)
	assert.Equal(t, []int{2, 1, 2}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingRefusesSupersededReferenceWithFreshEvidence(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)

	first := managedBindingAdmission("spwn-managed", generation, "evidence-a", "ref-a", 0)
	first.Transition = conversation.Clear
	accepted, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, accepted.Outcome)

	second := managedBindingAdmission("spwn-managed", generation, "evidence-b", "ref-b", 1)
	second.Transition = conversation.Clear
	current, err := AdmitConversationBinding(second)
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, current.Outcome)

	stale := managedBindingAdmission("spwn-managed", generation, "fresh-but-unproven-event", "ref-a", 2)
	stale.Transition = conversation.Clear
	decision, err := AdmitConversationBinding(stale)
	require.NoError(t, err)
	assert.Equal(t, conversation.Historical, decision.Outcome)
	assert.False(t, decision.Admitted())
	selection, found, err := CurrentConversationSelection(execution.ID(generation))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, current.Selection, selection)
	assert.Equal(t, []int{2, 1, 2}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingDoesNotAdvanceAfterDurableExit(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)

	firstAdmission := managedBindingAdmission("spwn-managed", generation, "evidence-1", "ref-a", 0)
	first, err := AdmitConversationBinding(firstAdmission)
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, first.Outcome)

	// The adapter captured this evidence while the process was live, but the
	// production durable-exit writer wins before admission reaches the store.
	delayedClear := managedBindingAdmission("spwn-managed", generation, "evidence-delayed", "ref-b", 1)
	delayedClear.Transition = conversation.Clear
	row, err := LoadSession("spwn-managed")
	require.NoError(t, err)
	exited, err := MarkSessionExitedIfUnchanged(row.ID, row.Status, row.UpdatedAt, "unexpected")
	require.NoError(t, err)
	require.True(t, exited)

	decision, err := AdmitConversationBinding(delayedClear)
	require.NoError(t, err)
	assert.Equal(t, conversation.Historical, decision.Outcome)
	assert.False(t, decision.Admitted())
	assert.False(t, decision.Changed)
	assert.Equal(t, first.Selection, decision.Selection)
	assert.Equal(t, []int{1, 1, 1}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())

	// A byte-for-byte retry would normally be Duplicate (and therefore
	// admitted for caller effects). Once durable exit wins it is Historical,
	// so a retry cannot revive status or conversation state.
	replay, err := AdmitConversationBinding(firstAdmission)
	require.NoError(t, err)
	assert.Equal(t, conversation.Historical, replay.Outcome)
	assert.False(t, replay.Admitted())
	assert.False(t, replay.Changed)
	assert.Equal(t, first.Selection, replay.Selection)
	assert.Equal(t, []int{1, 1, 1}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingExitIntentIsNotObservedCompletion(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)
	first, err := AdmitConversationBinding(managedBindingAdmission("spwn-managed", generation, "evidence-1", "ref-a", 0))
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, first.Outcome)

	_, err = SetSessionExitIntent("spwn-managed", AgentExitActionStop, "", time.Now().UTC())
	require.NoError(t, err)
	next := managedBindingAdmission("spwn-managed", generation, "evidence-2", "ref-b", 1)
	decision, err := AdmitConversationBinding(next)
	require.NoError(t, err)
	assert.Equal(t, conversation.Accepted, decision.Outcome)
	assert.True(t, decision.Admitted())
	assert.Equal(t, conversation.Revision(2), decision.Selection.Revision)
	assert.Equal(t, first.Selection.Conversation, decision.Selection.Conversation)
}

func TestAdmitConversationBindingOwnershipConflictRollsBack(t *testing.T) {
	setupTestDB(t)
	const (
		generationA = "11111111111111111111111111111111"
		generationB = "22222222222222222222222222222222"
	)
	seedManagedBindingSession(t, "spwn-a", generationA, "tmux-managed", "%7", "claude", 4242)
	seedManagedBindingSession(t, "spwn-b", generationB, "tmux-managed", "%7", "claude", 4242)

	first := managedBindingAdmission("spwn-a", generationA, "evidence-a", "shared-ref", 0)
	accepted, err := AdmitConversationBinding(first)
	require.NoError(t, err)
	assert.Equal(t, conversation.Accepted, accepted.Outcome)

	foreign := managedBindingAdmission("spwn-b", generationB, "evidence-b", "shared-ref", 0)
	decision, err := AdmitConversationBinding(foreign)
	require.NoError(t, err)
	assert.Equal(t, conversation.Conflict, decision.Outcome)
	assert.Contains(t, decision.Reason, "another current managed attempt")
	assert.Equal(t, []int{1, 1, 1}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestConversationBindingHistoryOutlivesLegacySessionLocator(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)
	decision, err := AdmitConversationBinding(managedBindingAdmission("spwn-managed", generation, "evidence-1", "ref-a", 0))
	require.NoError(t, err)
	require.Equal(t, conversation.Accepted, decision.Outcome)

	require.NoError(t, DeleteSession("spwn-managed"), "the legacy session is an ephemeral locator, not binding-history ownership")
	assert.Equal(t, []int{1, 1, 1}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}

func TestAdmitConversationBindingSeparatesMalformedInputFromDecisions(t *testing.T) {
	setupTestDB(t)
	const generation = "11111111111111111111111111111111"
	seedManagedBindingSession(t, "spwn-managed", generation, "tmux-managed", "%7", "claude", 4242)

	malformed := managedBindingAdmission("spwn-managed", generation, "", "ref-a", 0)
	_, err := AdmitConversationBinding(malformed)
	require.Error(t, err)

	unverified := managedBindingAdmission("spwn-managed", generation, "evidence-unverified", "ref-a", 0)
	unverified.Evidence.MainProcess = false
	decision, err := AdmitConversationBinding(unverified)
	require.NoError(t, err)
	assert.Equal(t, conversation.Rejected, decision.Outcome)
	assert.Equal(t, []int{0, 0, 0}, func() []int { a, b, c := bindingTableCounts(t); return []int{a, b, c} }())
}
