package processexec

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// contactDeferredAdapter is a deferred performer that also implements the
// contact surface, mirroring the production agent adapter shape.
type contactDeferredAdapter struct {
	mu          sync.Mutex
	assignee    string
	dispatches  int
	nudges      int
	escalations int
	contactErrs int
	failNext    bool
	observed    bool
	activity    Activity
}

func (a *contactDeferredAdapter) Validate(Request) error { return nil }

func (a *contactDeferredAdapter) Perform(context.Context, Request) (Observation, error) {
	panic("Perform should not be called on a deferred adapter")
}

func (a *contactDeferredAdapter) Dispatch(_ context.Context, request Request) (DispatchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatches++
	return DispatchResult{
		ExternalRef: "dispatch-" + request.Command.ID,
		Assignee:    a.assignee,
	}, nil
}

func (a *contactDeferredAdapter) ReconcileDeferred(context.Context, Request) (Observation, DeferredStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.observed {
		return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:done"}, DeferredObserved, nil
	}
	return Observation{}, DeferredInFlight, nil
}

func (a *contactDeferredAdapter) Contact(_ context.Context, _ Request, escalation bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext {
		a.failNext = false
		a.contactErrs++
		return fmt.Errorf("simulated contact transport crash")
	}
	if escalation {
		a.escalations++
	} else {
		a.nudges++
	}
	return nil
}

func (a *contactDeferredAdapter) Activity(context.Context, Request, time.Time) (Activity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	activity := a.activity
	a.activity = Activity{}
	return activity, nil
}

func (a *contactDeferredAdapter) setActivity(activity Activity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activity = activity
}

func (a *contactDeferredAdapter) setObserved() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observed = true
}

func (a *contactDeferredAdapter) sends() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nudges, a.escalations
}

type contactClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *contactClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *contactClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func contactExecutor(t *testing.T, adapter Adapter) (*ExclusiveV7Executor, *store.FS, string, *contactClock) {
	t.Helper()
	fs, runID := exclusiveV7Run(t)
	clock := &contactClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	executor.Now = clock.Now
	return executor, fs, runID, clock
}

func v7ContactState(t *testing.T, fs *store.FS, runID string) (pathv1.ContactRecordV7, string, bool) {
	t.Helper()
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	require.LessOrEqual(t, len(aggregate.Contacts), 1)
	for id, record := range aggregate.Contacts {
		marker, ok := aggregate.SideEffects[id]
		require.True(t, ok, "contact %q lacks marker", id)
		return record, marker.State, true
	}
	return pathv1.ContactRecordV7{}, "", false
}

func TestExclusiveV7DispatchSchedulesDefaultContact(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	executor, fs, runID, clock := contactExecutor(t, adapter)

	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	record, markerState, ok := v7ContactState(t, fs, runID)
	require.True(t, ok, "dispatch did not schedule a contact")
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Equal(t, pathv1.ContactProvenanceDispatch, record.Provenance)
	assert.Equal(t, pathv1.ContactKindAgent, record.Kind)
	assert.Equal(t, "agent:agt_worker", record.Assignee)
	assert.Equal(t, DefaultAgentContactCadence.String(), record.Cadence)
	assert.Equal(t, uint64(DefaultAgentContactBudget), record.Budget)
	assert.Equal(t, "human:operator", record.EscalationTarget)
	assert.Equal(t, pathv1.CanonicalTimestamp(clock.Now().Add(DefaultAgentContactCadence)), record.NextContactAt)
}

func TestExclusiveV7ContactNudgesEscalatesExactlyOnceAndCompletesAtomically(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	executor, fs, runID, clock := contactExecutor(t, adapter)

	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	for expected := 1; expected <= DefaultAgentContactBudget; expected++ {
		clock.Advance(DefaultAgentContactCadence)
		_, err := executor.Drive(t.Context(), runID)
		require.NoError(t, err)
		nudges, escalations := adapter.sends()
		assert.Equal(t, expected, nudges)
		assert.Zero(t, escalations)
	}
	record, _, _ := v7ContactState(t, fs, runID)
	assert.Equal(t, uint64(DefaultAgentContactBudget), record.Used)

	// Budget exhausted: the next due tick escalates exactly once; later ticks
	// stay silent because the schedule is cleared.
	clock.Advance(DefaultAgentContactCadence)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	clock.Advance(24 * time.Hour)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	nudges, escalations := adapter.sends()
	assert.Equal(t, DefaultAgentContactBudget, nudges)
	assert.Equal(t, 1, escalations)
	record, markerState, _ := v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.NotEmpty(t, record.EscalatedAt)
	assert.Empty(t, record.NextContactAt)

	// Settlement completes the contact in the same sealed transition that
	// observes the attempt.
	adapter.setObserved()
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	_, markerState, ok := v7ContactState(t, fs, runID)
	require.True(t, ok)
	assert.Equal(t, pathv1.ContactStateCompleted, markerState)
}

func TestExclusiveV7LateInitializationBackfillsContact(t *testing.T) {
	adapter := &contactDeferredAdapter{}
	executor, fs, runID, clock := contactExecutor(t, adapter)

	// Dispatch succeeds but yields no durable assignee: no contact identity
	// may be synthesized, and the run keeps ticking without one.
	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	_, _, ok := v7ContactState(t, fs, runID)
	assert.False(t, ok, "contact created without a durable assignee")

	clock.Advance(time.Minute)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	_, _, ok = v7ContactState(t, fs, runID)
	assert.False(t, ok)

	// Once the idempotent Dispatch authority can resolve the assignee, the
	// next in-flight tick back-fills the schedule with late provenance.
	adapter.mu.Lock()
	adapter.assignee = "agent:agt_worker"
	adapter.mu.Unlock()
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	record, markerState, ok := v7ContactState(t, fs, runID)
	require.True(t, ok, "late initialization did not schedule a contact")
	assert.Equal(t, pathv1.ContactProvenanceLateInitialization, record.Provenance)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
}

func TestExclusiveV7ContactRecoveryResetsBudget(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	executor, fs, runID, clock := contactExecutor(t, adapter)
	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	for range 2 {
		clock.Advance(DefaultAgentContactCadence)
		_, err := executor.Drive(t.Context(), runID)
		require.NoError(t, err)
	}
	record, _, _ := v7ContactState(t, fs, runID)
	require.Equal(t, uint64(2), record.Used)

	adapter.setActivity(Activity{Recovered: true, At: clock.Now().Add(time.Second)})
	clock.Advance(time.Minute)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	record, markerState, _ := v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Zero(t, record.Used)
	assert.Empty(t, record.EscalatedAt)
	assert.NotEmpty(t, record.LastRecoveredAt)
	assert.Equal(t, pathv1.CanonicalTimestamp(clock.Now().Add(DefaultAgentContactCadence)), record.NextContactAt)
}

func TestExclusiveV7HumanPreemptionPausesAndDeliveryReleases(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	executor, fs, runID, clock := contactExecutor(t, adapter)
	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	adapter.setActivity(Activity{HumanInteracted: true, At: clock.Now()})
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	record, markerState, _ := v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.NotEmpty(t, record.HumanInteractedAt)

	// After the grace the contact pauses, and a due instant sends nothing.
	clock.Advance(ContactHumanPreemptionGrace)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	_, markerState, _ = v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStatePaused, markerState)
	clock.Advance(DefaultAgentContactCadence * 4)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	nudges, escalations := adapter.sends()
	assert.Zero(t, nudges)
	assert.Zero(t, escalations)

	// Delivery metadata proves the activity was automation: latch clears,
	// pause releases, and due nudging resumes.
	adapter.setActivity(Activity{AutomatedDelivery: true, At: clock.Now()})
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	record, markerState, _ = v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Empty(t, record.PauseReason)
	assert.Empty(t, record.HumanInteractedAt)
	nudges, _ = adapter.sends()
	assert.Equal(t, 1, nudges)
}

func TestExclusiveV7ContactSendCrashResendsFromDurableDue(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	executor, fs, runID, clock := contactExecutor(t, adapter)
	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	// The transport crashes after the durable due mark: the tick surfaces the
	// error, the due state persists, and no progress is falsely recorded.
	adapter.mu.Lock()
	adapter.failNext = true
	adapter.mu.Unlock()
	clock.Advance(DefaultAgentContactCadence)
	_, err = executor.Drive(t.Context(), runID)
	require.Error(t, err)
	record, markerState, _ := v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateDue, markerState)
	assert.Zero(t, record.Used)

	// The next tick resends from the durable due state and seals the nudge —
	// the accepted single-duplicate window, bounded to due.
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	record, markerState, _ = v7ContactState(t, fs, runID)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Equal(t, uint64(1), record.Used)
	nudges, _ := adapter.sends()
	assert.Equal(t, 1, nudges)
}

// TestExclusiveV7PreflightRefusesBeforeAnySideEffect is the P1 focused test:
// a claimed-eligible run whose performer's contact fields exceed the durable
// bounds must fail closed BEFORE the claim append and BEFORE any external
// dispatch — never after creating work it cannot seal a contact for.
func TestExclusiveV7PreflightRefusesBeforeAnySideEffect(t *testing.T) {
	adapter := &contactDeferredAdapter{assignee: "agent:agt_worker"}
	fs, runID := exclusiveV7RunAt(t, t.TempDir(), &model.Performer{
		Kind: model.PerformerAgent, Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "5m", Budget: 2, EscalationTarget: "human:" + strings.Repeat("x", 300)},
	}, nil)
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	_, err := executor.Drive(t.Context(), runID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preflight path-v1 contact")
	adapter.mu.Lock()
	dispatches := adapter.dispatches
	adapter.mu.Unlock()
	assert.Zero(t, dispatches, "no external dispatch may precede contact preflight")
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	for _, command := range aggregate.Commands {
		assert.NotEqual(t, pathv1.CommandPerformAttempt, command.Identity.Kind,
			"no attempt claim may be appended for an unpreflightable performer")
	}
}

func TestPreflightSchema7ContactBounds(t *testing.T) {
	base := model.Performer{Kind: model.PerformerAgent, Prompt: "work"}
	require.NoError(t, PreflightSchema7Contact(base))
	human := model.Performer{Kind: model.PerformerHuman, Ask: "review"}
	require.NoError(t, PreflightSchema7Contact(human))
	longTarget := base
	longTarget.Contact = &model.ContactSchedule{Cadence: "5m", Budget: 1, EscalationTarget: "human:" + strings.Repeat("x", 300)}
	require.Error(t, PreflightSchema7Contact(longTarget))
	longHuman := human
	longHuman.Assignee = strings.Repeat("y", 300)
	require.Error(t, PreflightSchema7Contact(longHuman))
	badCadence := base
	badCadence.Contact = &model.ContactSchedule{Cadence: "-1s", Budget: 1, EscalationTarget: "human:operator"}
	require.Error(t, PreflightSchema7Contact(badCadence))
}

// bareDeferredAdapter proves adapters without the contact surface no-op
// servicing entirely (F5).
type bareDeferredAdapter struct {
	inner *contactDeferredAdapter
}

func (a *bareDeferredAdapter) Validate(Request) error { return nil }
func (a *bareDeferredAdapter) Perform(context.Context, Request) (Observation, error) {
	panic("Perform should not be called on a deferred adapter")
}
func (a *bareDeferredAdapter) Dispatch(ctx context.Context, request Request) (DispatchResult, error) {
	return a.inner.Dispatch(ctx, request)
}
func (a *bareDeferredAdapter) ReconcileDeferred(ctx context.Context, request Request) (Observation, DeferredStatus, error) {
	return a.inner.ReconcileDeferred(ctx, request)
}

func TestExclusiveV7NonContactAdapterNoOpsServicing(t *testing.T) {
	adapter := &bareDeferredAdapter{inner: &contactDeferredAdapter{assignee: "agent:agt_worker"}}
	executor, fs, runID, clock := contactExecutor(t, adapter)

	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	clock.Advance(time.Hour)
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)

	// A contact is still scheduled at dispatch (the schedule is engine state,
	// not adapter capability), but no servicing sends ever happen.
	record, markerState, ok := v7ContactState(t, fs, runID)
	require.True(t, ok)
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Zero(t, record.Used)
}
