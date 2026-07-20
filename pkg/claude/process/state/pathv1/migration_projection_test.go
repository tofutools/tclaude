package pathv1

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestBuildProgressedInitializationDeterministicallyProjectsSettledFailEdgeAndContact(t *testing.T) {
	fixture := progressedProjectionFixture(t, false)

	first, err := BuildProgressedInitialization(t.Context(), fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildProgressedInitialization(t.Context(), fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := EncodeCheckpointV7(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := EncodeCheckpointV7(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("identical progressed projection inputs produced different checkpoints")
	}

	if first.Execution == nil || first.Execution.Revision != 1 || first.Execution.LogAdvanced {
		t.Fatalf("migration execution head = %#v", first.Execution)
	}
	if first.Execution.PreviousDigest != first.InitializeDigest() || first.Execution.Status != string(legacy.RunStatusRunning) {
		t.Fatalf("migration execution anchor/status = %#v", first.Execution)
	}
	metadata := first.Execution.LegacyProjection
	if metadata == nil || metadata.LegacyLastLogSeq != uint64(fixture.state.LastLogSeq) || metadata.LegacyLogChecksum != fixture.state.LogChecksum {
		t.Fatalf("legacy projection metadata = %#v", metadata)
	}
	if metadata.LegacyCheckpointDigest != fixture.input.UpgradeNeeded.Checkpoint.Digest {
		t.Fatalf("legacy checkpoint digest = %q", metadata.LegacyCheckpointDigest)
	}

	aggregate := first.Execution.Aggregate
	frontier, _, err := liveProjectedNode(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if frontier != "recover" {
		t.Fatalf("projected frontier = %q, want recover", frontier)
	}
	if len(aggregate.Contacts) != 1 {
		t.Fatalf("projected contacts = %#v", aggregate.Contacts)
	}
	for _, contact := range aggregate.Contacts {
		if contact.Provenance != ContactProvenanceLegacyProjection || contact.Used != 1 || contact.Budget != 3 ||
			contact.ScheduledAt != CanonicalTimestamp(fixture.scheduledAt) || contact.LastContactedAt != CanonicalTimestamp(fixture.contactedAt) ||
			contact.NextContactAt != "" || contact.LegacyPauseReason != "performer observed" || contact.PauseReason != "" {
			t.Fatalf("projected contact = %#v", contact)
		}
		marker := aggregate.SideEffects[contact.ID]
		if marker.Kind != SideEffectContact || marker.State != ContactStateCompleted {
			t.Fatalf("projected contact marker = %#v", marker)
		}
	}
	migrationRecords := 0
	for _, record := range aggregate.AdminRecords {
		if record.AdminType == LegacyProjectionAdminType {
			migrationRecords++
			if record.Actor != "system:migration" || record.EventSeq != first.Initialize.EventSeq || metadata == nil || record.EvidenceRef != metadata.ID {
				t.Fatalf("migration provenance = %#v", record)
			}
		}
	}
	if migrationRecords != 1 {
		t.Fatalf("migration provenance count = %d", migrationRecords)
	}
	if err := ValidateCheckpointV7(first); err != nil {
		t.Fatal(err)
	}
}

func TestBuildProgressedInitializationTypedRefusalForDuplicateContactSchedule(t *testing.T) {
	fixture := progressedProjectionFixture(t, true)

	_, err := BuildProgressedInitialization(t.Context(), fixture.input)
	if !errors.Is(err, ErrInitializationAmbiguous) {
		t.Fatalf("duplicate schedule error = %v, want typed ambiguity", err)
	}
	var refusal *ProjectionRefusal
	if !errors.As(err, &refusal) || refusal.Code != ProjectionRefusalContactHistory {
		t.Fatalf("duplicate schedule refusal = %#v", refusal)
	}
	if !strings.Contains(refusal.Detail, "2 schedule events") {
		t.Fatalf("duplicate schedule detail = %q", refusal.Detail)
	}
}

func TestBuildProgressedInitializationTreatsTamperedEvidenceAsCorruption(t *testing.T) {
	fixture := progressedProjectionFixture(t, false)
	fixture.input.Manifest[len(fixture.input.Manifest)-1].Checksum = "sha256:" + strings.Repeat("0", 64)

	_, err := BuildProgressedInitialization(t.Context(), fixture.input)
	if !errors.Is(err, ErrInitializationInconsistent) {
		t.Fatalf("tampered evidence error = %v, want hard inconsistency", err)
	}
	if errors.Is(err, ErrInitializationAmbiguous) {
		t.Fatalf("tampered evidence fell back as compatibility refusal: %v", err)
	}
	var refusal *ProjectionRefusal
	if errors.As(err, &refusal) {
		t.Fatalf("tampered evidence returned ProjectionRefusal: %#v", refusal)
	}
}

func TestBuildProgressedInitializationDeterministicallyProjectsSatisfiedTimerToNextFrontier(t *testing.T) {
	input := progressedWaitProjectionFixture(t)
	first, err := BuildProgressedInitialization(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildProgressedInitialization(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := EncodeCheckpointV7(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := EncodeCheckpointV7(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("identical satisfied-timer inputs produced different projections")
	}
	frontier, _, err := liveProjectedNode(first.Execution.Aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if frontier != "work" {
		t.Fatalf("satisfied timer projected frontier = %q, want work", frontier)
	}
	waits := 0
	for _, effect := range first.Execution.Aggregate.SideEffects {
		if effect.Kind == SideEffectWait {
			waits++
			if effect.State != "satisfied" || effect.WaitKind != "duration" {
				t.Fatalf("projected timer effect = %#v", effect)
			}
		}
	}
	if waits != 1 {
		t.Fatalf("projected timer effect count = %d", waits)
	}
}

func TestValidateCheckpointV7RejectsSelfConsistentRevisionOneAggregateTamper(t *testing.T) {
	fixture := progressedProjectionFixture(t, false)
	checkpoint, err := BuildProgressedInitialization(t.Context(), fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	for id, contact := range checkpoint.Execution.Aggregate.Contacts {
		contact.LegacyPauseReason = "forged historical reason"
		checkpoint.Execution.Aggregate.Contacts[id] = contact
	}
	genesisDigest, err := initializeEventDigest(checkpoint.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Digest, err = executionCheckpointDigest(genesisDigest, checkpoint.Execution)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpointV7(checkpoint); !errors.Is(err, ErrInitializationInvalid) || !strings.Contains(err.Error(), "projected aggregate hash mismatch") {
		t.Fatalf("self-consistent aggregate tamper error = %v", err)
	}
}

type progressedProjectionTestFixture struct {
	input       LegacyProjectionInput
	state       legacy.State
	scheduledAt time.Time
	contactedAt time.Time
}

func progressedProjectionFixture(t *testing.T, duplicateSchedule bool) progressedProjectionTestFixture {
	t.Helper()
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: progressed-projection
start: work
nodes:
  work:
    type: task
    performer:
      kind: agent
      prompt: do the work
      contact:
        cadence: 10m
        budget: 3
        escalationTarget: human:operator
    next:
      pass: done
      fail: recover
  recover:
    type: task
    performer:
      kind: human
      ask: recover the failure
      assignee: human:operator
    next: done
  done:
    type: end
    result: completed
`)
	parsed, err := model.ParseExactSource(source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("fixture template diagnostics = %#v", parsed.Diagnostics)
	}
	st := legacy.New("run-progressed", parsed.Ref, parsed.Ref, []legacy.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: legacy.NodeStatusReady},
		{ID: "recover", Type: model.NodeTypeTask, Status: legacy.NodeStatusPending},
		{ID: "done", Type: model.NodeTypeEnd, Status: legacy.NodeStatusPending},
	})
	st.Status = legacy.RunStatusRunning
	builder := legacyProjectionEvidenceBuilder{t: t, state: st, logs: map[string][]evidence.LogEntry{}}
	base := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)

	start := plannedLegacyCommand(t, &builder.state, parsed.Template, plan.CommandKindStartAttempt, "work")
	outstanding, err := start.OutstandingCommand(base)
	if err != nil {
		t.Fatal(err)
	}
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{Type: legacy.EventCommandIssued, Command: &outstanding}, base)
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{Type: legacy.EventNodeAttemptStarted, Attempt: 1, CommandID: start.ID}, base)
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{Type: legacy.EventCommandDispatched, CommandID: start.ID, ExternalRef: "agent:agt_worker"}, base.Add(time.Minute))

	scheduledAt := base.Add(2 * time.Minute)
	contact := legacy.ContactState{
		CommandID: start.ID, Kind: legacy.WaitKindAgent, Assignee: "agent:agt_worker",
		Cadence: "10m0s", Budget: 3, EscalationTarget: "human:operator", NextContactAt: scheduledAt.Add(10 * time.Minute),
	}
	builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventContactScheduled, Contact: &contact}, scheduledAt)
	if duplicateSchedule {
		builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventContactScheduled, Contact: &contact}, scheduledAt.Add(time.Second))
	}
	contactedAt := scheduledAt.Add(10 * time.Minute)
	contact.Used = 1
	contact.LastContactedAt = contactedAt
	contact.NextContactAt = contactedAt.Add(10 * time.Minute)
	builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventContactUpdated, Contact: &contact}, contactedAt)

	observedAt := contactedAt.Add(time.Minute)
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{
		Type: legacy.EventCommandObserved, CommandID: start.ID, Actor: "agent:agt_worker", Outcome: "fail",
		EvidenceRef: "artifact:failed-work", EvidenceHash: strings.Repeat("a", 64), ExternalRef: "agent:agt_worker",
	}, observedAt)
	contact.Paused = true
	contact.PauseReason = "performer observed"
	contact.NextContactAt = time.Time{}
	builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventContactUpdated, Contact: &contact}, observedAt)

	settle := plannedLegacyCommand(t, &builder.state, parsed.Template, plan.CommandKindSettleAttempt, "work")
	settleOutstanding, err := settle.OutstandingCommand(observedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{Type: legacy.EventCommandIssued, Command: &settleOutstanding}, observedAt.Add(time.Minute))
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{Type: legacy.EventCommandObserved, CommandID: settle.ID}, observedAt.Add(2*time.Minute))
	builder.append("work", evidence.EntryKindAttempt, legacy.Event{
		Type: legacy.EventNodeAttemptSettled, Actor: "agent:agt_worker", Outcome: "fail", NodeStatus: legacy.NodeStatusFailed,
		EvidenceRef: "artifact:failed-work", EvidenceHash: strings.Repeat("a", 64),
	}, observedAt.Add(2*time.Minute))

	activate := plannedLegacyCommand(t, &builder.state, parsed.Template, plan.CommandKindActivateNode, "work")
	activateOutstanding, err := activate.OutstandingCommand(observedAt.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventCommandIssued, Command: &activateOutstanding}, observedAt.Add(3*time.Minute))
	builder.append("work", evidence.EntryKindGate, legacy.Event{Type: legacy.EventCommandObserved, CommandID: activate.ID}, observedAt.Add(3*time.Minute))
	builder.append("recover", evidence.EntryKindStatus, legacy.Event{Type: legacy.EventNodeStatusSet, NodeStatus: legacy.NodeStatusReady}, observedAt.Add(3*time.Minute))

	checkpointJSON, err := legacy.Encode(&builder.state)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := AssessUpgradeNeeded(t.Context(), checkpointJSON, &builder.state, parsed.Ref, parsed.SourceHash, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := make([]evidence.NodeLog, 0, len(builder.logs))
	for nodeID, entries := range builder.logs {
		logs = append(logs, evidence.NodeLog{NodeID: nodeID, Entries: entries})
	}
	return progressedProjectionTestFixture{
		input: LegacyProjectionInput{
			UpgradeNeeded: needed, Template: parsed.Template, LegacyState: &builder.state,
			LegacyCheckpointJSON: checkpointJSON, Manifest: builder.manifest, NodeLogs: logs,
		},
		state: builder.state, scheduledAt: scheduledAt, contactedAt: contactedAt,
	}
}

func progressedWaitProjectionFixture(t *testing.T) LegacyProjectionInput {
	t.Helper()
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: progressed-wait-projection
start: wait
nodes:
  wait:
    type: wait
    wait: {duration: 5m}
    next: work
  work:
    type: task
    performer: {kind: agent, prompt: continue}
    next: done
  done: {type: end, result: completed}
`)
	parsed, err := model.ParseExactSource(source)
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse wait fixture: %v %#v", err, parsed.Diagnostics)
	}
	st := legacy.New("run-progressed-wait", parsed.Ref, parsed.Ref, []legacy.NodeInit{
		{ID: "wait", Type: model.NodeTypeWait, Status: legacy.NodeStatusReady},
		{ID: "work", Type: model.NodeTypeTask, Status: legacy.NodeStatusPending},
		{ID: "done", Type: model.NodeTypeEnd, Status: legacy.NodeStatusPending},
	})
	st.Status = legacy.RunStatusRunning
	builder := legacyProjectionEvidenceBuilder{t: t, state: st, logs: map[string][]evidence.LogEntry{}}
	createdAt := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	dueAt := createdAt.Add(5 * time.Minute)
	builder.append("wait", evidence.EntryKindGate, legacy.Event{
		Type:  legacy.EventTimerCreated,
		Timer: &legacy.TimerRecord{ID: "timer_wait", NodeID: "wait", CreatedAt: createdAt, DueAt: dueAt},
	}, createdAt)
	builder.append("wait", evidence.EntryKindGate, legacy.Event{
		Type: legacy.EventTimerSatisfied, TimerID: "timer_wait", NodeStatus: legacy.NodeStatusCompleted,
	}, dueAt)
	activate := plannedLegacyCommand(t, &builder.state, parsed.Template, plan.CommandKindActivateNode, "wait")
	outstanding, err := activate.OutstandingCommand(dueAt)
	if err != nil {
		t.Fatal(err)
	}
	builder.append("wait", evidence.EntryKindGate, legacy.Event{Type: legacy.EventCommandIssued, Command: &outstanding}, dueAt)
	builder.append("wait", evidence.EntryKindGate, legacy.Event{Type: legacy.EventCommandObserved, CommandID: activate.ID}, dueAt)
	builder.append("work", evidence.EntryKindStatus, legacy.Event{Type: legacy.EventNodeStatusSet, NodeStatus: legacy.NodeStatusReady}, dueAt)

	checkpointJSON, err := legacy.Encode(&builder.state)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := AssessUpgradeNeeded(t.Context(), checkpointJSON, &builder.state, parsed.Ref, parsed.SourceHash, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := make([]evidence.NodeLog, 0, len(builder.logs))
	for nodeID, entries := range builder.logs {
		logs = append(logs, evidence.NodeLog{NodeID: nodeID, Entries: entries})
	}
	return LegacyProjectionInput{
		UpgradeNeeded: needed, Template: parsed.Template, LegacyState: &builder.state,
		LegacyCheckpointJSON: checkpointJSON, Manifest: builder.manifest, NodeLogs: logs,
	}
}

type legacyProjectionEvidenceBuilder struct {
	t        *testing.T
	state    legacy.State
	manifest []evidence.ManifestEntry
	logs     map[string][]evidence.LogEntry
}

func (b *legacyProjectionEvidenceBuilder) append(nodeID string, kind evidence.EntryKind, event legacy.Event, at time.Time) {
	b.t.Helper()
	if event.Contact != nil {
		contact := *event.Contact
		event.Contact = &contact
	}
	seq := int64(len(b.manifest) + 1)
	event.Seq = seq
	event.At = at
	event.LogChecksum = ""
	if event.NodeID == "" {
		event.NodeID = nodeID
	}
	entry := evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion, Seq: seq, At: at,
		Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID}, Kind: kind, Event: &event,
	}
	previous := ""
	if len(b.manifest) > 0 {
		previous = b.manifest[len(b.manifest)-1].Checksum
	}
	manifest, err := evidence.ManifestEntryForLog(entry, previous)
	if err != nil {
		b.t.Fatal(err)
	}
	next, err := legacy.Apply(b.state, event)
	if err != nil {
		b.t.Fatalf("apply fixture event seq %d (%s): %v", seq, event.Type, err)
	}
	next.LogChecksum = manifest.Checksum
	b.state = next
	b.manifest = append(b.manifest, manifest)
	b.logs[nodeID] = append(b.logs[nodeID], entry)
}

func plannedLegacyCommand(t *testing.T, st *legacy.State, tmpl *model.Template, kind plan.CommandKind, nodeID string) plan.Command {
	t.Helper()
	commands, err := plan.Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.Kind == kind && command.NodeID == nodeID {
			return command
		}
	}
	t.Fatalf("planned commands %#v lack %s for %s", commands, kind, nodeID)
	return plan.Command{}
}

func (c *CheckpointV7) InitializeDigest() string {
	if c == nil {
		return ""
	}
	digest, _ := initializeEventDigest(c.Initialize)
	return digest
}
