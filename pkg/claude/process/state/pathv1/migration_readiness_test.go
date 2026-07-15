package pathv1

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestAssessUpgradeNeededClassifiesAndSortsLegacyDrainBlockers(t *testing.T) {
	ref := "demo@sha256:" + strings.Repeat("a", 64)
	st := legacy.New("run", ref, ref, nil)
	st.Status = legacy.RunStatusRunning
	st.OutstandingCommands = map[string]legacy.OutstandingCommand{
		"cmd":          {ID: "cmd", Status: legacy.CommandStatusIssued, ExternalRef: "external-issued"},
		"cmd-observed": {ID: "cmd-observed", Status: legacy.CommandStatusObserved, ExternalRef: "external-observed"},
	}
	st.Nodes = map[string]legacy.NodeState{
		"attempt": {Status: legacy.NodeStatusRunning, ActiveAttempt: &legacy.AttemptState{Attempt: 2, CommandID: "cmd"}},
		"blocked": {Status: legacy.NodeStatusBlocked},
		"resolved": {Status: legacy.NodeStatusCompleted, BlockResolution: &legacy.BlockResolution{
			NodeID: "resolved", BlockedAttempt: 1, Decision: legacy.BlockDecisionSkip, Actor: "human:operator",
			Reason: "waived", EvidenceRef: "ticket", Timestamp: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		}},
	}
	st.Waits = map[string]legacy.WaitRecord{"wait": {ID: "wait", Status: legacy.WaitStatusPending}}
	st.Timers = map[string]legacy.TimerRecord{"timer": {ID: "timer", Status: legacy.WaitStatusPending}}
	st.Contacts = map[string]legacy.ContactState{
		"cmd":          {CommandID: "cmd"},
		"cmd-observed": {CommandID: "cmd-observed", Paused: true},
	}
	st.Obligations = map[string]legacy.ObligationRecord{"obligation": {ID: "obligation", Status: legacy.WaitStatusPending}}

	needed, err := AssessUpgradeNeeded(t.Context(), []byte(`{"checkpoint":true}`), &st, ref, strings.Repeat("b", 64), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateUpgradeNeeded(needed); err != nil {
		t.Fatal(err)
	}
	if needed.Reason != UpgradeLegacyDrainRequired {
		t.Fatalf("reason = %q", needed.Reason)
	}
	wantKinds := []LegacyActiveKind{
		LegacyActiveAttempt, LegacyActiveBlockResolution, LegacyActiveBlockedNode,
		LegacyActiveCommand, LegacyActiveCommand, LegacyActiveContact, LegacyActiveContact,
		LegacyActiveObligation, LegacyActiveSideEffect, LegacyActiveSideEffect,
		LegacyActiveTimer, LegacyActiveWait,
	}
	if len(needed.ActiveLegacyIDs) != len(wantKinds) {
		t.Fatalf("active IDs = %#v", needed.ActiveLegacyIDs)
	}
	for i, want := range wantKinds {
		if needed.ActiveLegacyIDs[i].Kind != want {
			t.Fatalf("active ID %d kind = %q, want %q", i, needed.ActiveLegacyIDs[i].Kind, want)
		}
	}
}

func TestAssessUpgradeNeededStableCheckpointWaitTimerIdentityCompatibility(t *testing.T) {
	const checkpoint = `{
  "stateSchemaVersion": 6,
  "runId": "run",
  "status": "running",
  "originalTemplateRef": "demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "currentTemplateRef": "demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "nodes": {},
  "waits": {
    "wait-b": {"nodeId": "node-b", "kind": "human", "status": "pending"},
    "wait-a": {"id": "", "nodeId": "node-a", "kind": "agent", "status": "pending"}
  },
  "timers": {
    "timer-b": {"nodeId": "node-b", "status": "pending"},
    "timer-a": {"id": "", "nodeId": "node-a", "status": "pending"}
  },
  "lastLogSeq": 0,
  "logChecksum": ""
}`
	decoded, err := PredecodeLegacyState([]byte(checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	needed, err := AssessUpgradeNeeded(
		t.Context(), decoded.CanonicalJSON, decoded.State,
		"demo@sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64),
		decoded.AdminRecords, decoded.AdminResolutions,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []LegacyActiveID{
		{Kind: LegacyActiveTimer, ID: "timer-a"},
		{Kind: LegacyActiveTimer, ID: "timer-b"},
		{Kind: LegacyActiveWait, ID: "wait-a"},
		{Kind: LegacyActiveWait, ID: "wait-b"},
	}
	if !slices.Equal(needed.ActiveLegacyIDs, want) {
		t.Fatalf("active IDs = %#v, want %#v", needed.ActiveLegacyIDs, want)
	}
}

func TestAssessUpgradeNeededStableCheckpointRejectsWaitTimerIdentityMismatch(t *testing.T) {
	const checkpoint = `{
  "stateSchemaVersion": 6,
  "runId": "run",
  "status": "running",
  "originalTemplateRef": "demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "currentTemplateRef": "demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "nodes": {},
  "lastLogSeq": 0,
  "logChecksum": ""
}`
	for _, tc := range []struct {
		name, records, want string
	}{
		{
			name:    "wait",
			records: `"waits":{"wait-key":{"id":"wait-record","nodeId":"node","kind":"human","status":"pending"}},`,
			want:    `legacy wait map key "wait-key" differs from embedded identity "wait-record"`,
		},
		{
			name:    "timer",
			records: `"timers":{"timer-key":{"id":"timer-record","nodeId":"node","status":"pending"}},`,
			want:    `legacy timer map key "timer-key" differs from embedded identity "timer-record"`,
		},
		{
			name:    "satisfied wait",
			records: `"waits":{"wait-key":{"id":"wait-record","nodeId":"node","kind":"human","status":"satisfied"}},`,
			want:    `legacy wait map key "wait-key" differs from embedded identity "wait-record"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := strings.Replace(checkpoint, `"nodes": {},`, `"nodes": {},`+tc.records, 1)
			decoded, err := PredecodeLegacyState([]byte(fixture))
			if err != nil {
				t.Fatal(err)
			}
			_, err = AssessUpgradeNeeded(
				t.Context(), decoded.CanonicalJSON, decoded.State,
				"demo@sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64),
				decoded.AdminRecords, decoded.AdminResolutions,
			)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAssessUpgradeNeededActiveIDBoundAndCancellation(t *testing.T) {
	ref := "demo@sha256:" + strings.Repeat("a", 64)
	build := func(count int) *legacy.State {
		st := legacy.New("run", ref, ref, nil)
		st.Waits = make(map[string]legacy.WaitRecord, count)
		for i := range count {
			id := fmt.Sprintf("%08d", i)
			st.Waits[id] = legacy.WaitRecord{ID: id, Status: legacy.WaitStatusPending}
		}
		return &st
	}
	if needed, err := AssessUpgradeNeeded(t.Context(), []byte(`{}`), build(MaxActiveLegacyIDs), ref, strings.Repeat("b", 64), nil, nil); err != nil || len(needed.ActiveLegacyIDs) != MaxActiveLegacyIDs {
		t.Fatalf("bound result = %d, err = %v", len(needed.ActiveLegacyIDs), err)
	}
	_, err := AssessUpgradeNeeded(t.Context(), []byte(`{}`), build(MaxActiveLegacyIDs+1), ref, strings.Repeat("b", 64), nil, nil)
	var over *UpgradeNeededOverBudgetError
	if !errors.As(err, &over) || over.Value != MaxActiveLegacyIDs+1 || over.Maximum != MaxActiveLegacyIDs {
		t.Fatalf("bound+1 error = %#v (%v)", over, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AssessUpgradeNeeded(canceled, []byte(`{}`), build(1), ref, strings.Repeat("b", 64), nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestAssessAndValidateUpgradeNeededFailClosedTampering(t *testing.T) {
	ref := "demo@sha256:" + strings.Repeat("a", 64)
	st := legacy.New("run", ref, ref, nil)
	if _, err := AssessUpgradeNeeded(t.Context(), nil, &st, ref, strings.Repeat("b", 64), nil, nil); err == nil {
		t.Fatal("empty checkpoint was accepted")
	}
	if _, err := AssessUpgradeNeeded(t.Context(), []byte(`{}`), &st, ref, strings.Repeat("B", 64), nil, nil); err == nil {
		t.Fatal("noncanonical source hash was accepted")
	}
	needed, err := AssessUpgradeNeeded(t.Context(), []byte(`{}`), &st, ref, strings.Repeat("b", 64), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed.ActiveLegacyIDs = []LegacyActiveID{{Kind: LegacyActiveWait, ID: "z"}, {Kind: LegacyActiveCommand, ID: "a"}}
	needed.Reason = UpgradeLegacyDrainRequired
	if err := ValidateUpgradeNeeded(needed); err == nil {
		t.Fatal("unsorted active IDs were accepted")
	}
}

func TestValidateUpgradeNeededRejectsForgedCheckpointAdminProvenance(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*UpgradeNeeded)
		rebind           bool
		rebindResolution bool
	}{
		{
			name: "resolution omitted from active ids",
			mutate: func(needed *UpgradeNeeded) {
				needed.ActiveLegacyIDs = nil
				needed.Reason = UpgradeMigrationRequired
			},
		},
		{
			name: "cross-run record",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.RunID = "other-run"
			},
			rebind: true,
		},
		{
			name: "positive legacy event sequence rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EventSeq = 1
			},
			rebind: true,
		},
		{
			name: "missing admin type",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = ""
			},
			rebind: true,
		},
		{
			name: "missing actor",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = ""
			},
			rebind: true,
		},
		{
			name: "missing timestamp",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = ""
			},
			rebind: true,
		},
		{
			name: "block-resolution type without payload rebound",
			mutate: func(needed *UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "resolution digest mismatch",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "human:forged"
			},
		},
		{
			name: "zero blocked attempt rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.BlockedAttempt = 0
			},
			rebindResolution: true,
		},
		{
			name: "invalid resolution actor rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "operator"
			},
			rebindResolution: true,
		},
		{
			name: "engine resolution actor rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "engine:forged"
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution node rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.NodeID = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution reason rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Reason = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution evidence rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.EvidenceRef = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution timestamp rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Timestamp = ""
			},
			rebindResolution: true,
		},
		{
			name: "wrong resolution admin type rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = "admin_repair_recorded"
			},
			rebind: true,
		},
		{
			name: "repair type swap clears resolution authority rebound",
			mutate: func(needed *UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_repair_recorded"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "programs-allowed type swap clears resolution authority rebound",
			mutate: func(needed *UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_programs_allowed"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "unknown nonresolution admin type rebound",
			mutate: func(needed *UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "invented"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "outer actor mismatch rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = "human:other"
			},
			rebind: true,
		},
		{
			name: "outer reason mismatch rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.ReasonCode = "other"
			},
			rebind: true,
		},
		{
			name: "outer evidence mismatch rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EvidenceRef = "ticket:other"
			},
			rebind: true,
		},
		{
			name: "outer timestamp mismatch rebound",
			mutate: func(needed *UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = "2026-07-15T00:00:01Z"
			},
			rebind: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			needed := validUpgradeNeededWithCheckpointAdmin(t)
			test.mutate(&needed)
			if test.rebindResolution {
				rebindCheckpointAdminResolution(t, &needed)
			} else if test.rebind {
				rebindCheckpointAdminIdentity(t, &needed)
			}
			if err := ValidateUpgradeNeeded(needed); err == nil {
				t.Fatal("forged checkpoint admin provenance was accepted")
			}
		})
	}
}

func TestAssessUpgradeNeededCarriesBoundAdminResolution(t *testing.T) {
	want := validUpgradeNeededWithCheckpointAdmin(t)
	admin := want.CheckpointAdminRecords[0]
	ref := want.TemplateRef
	st := legacy.New(want.RunID, ref, ref, nil)

	needed, err := AssessUpgradeNeeded(
		t.Context(),
		[]byte(`{"checkpoint":true}`),
		&st,
		ref,
		want.TemplateSourceHash,
		map[string]PathV1AdminRecord{admin.LegacyID: admin.Record},
		map[string]BlockResolution{admin.LegacyID: *admin.Resolution},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(needed.CheckpointAdminRecords) != 1 || needed.CheckpointAdminRecords[0].Resolution == nil {
		t.Fatalf("checkpoint admin records = %#v", needed.CheckpointAdminRecords)
	}
	if err := ValidateUpgradeNeeded(needed); err != nil {
		t.Fatal(err)
	}
}

func TestAssessUpgradeNeededClassifiesNonResolutionAdminAsDrain(t *testing.T) {
	ref := "demo@sha256:" + strings.Repeat("a", 64)
	st := legacy.New("run", ref, ref, nil)
	record := PathV1AdminRecord{
		RunID: "run", AdminType: string(legacy.EventAdminRepairRecorded), Actor: "human:operator",
		ReasonCode: "repaired", EvidenceRef: "ticket:TCL-507", Timestamp: "2026-07-15T00:00:00Z",
	}
	var err error
	record.ID, err = LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}

	needed, err := AssessUpgradeNeeded(
		t.Context(), []byte(`{"checkpoint":true}`), &st, ref, strings.Repeat("c", 64),
		map[string]PathV1AdminRecord{record.ID: record}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if needed.Reason != UpgradeLegacyDrainRequired || len(needed.ActiveLegacyIDs) != 1 ||
		needed.ActiveLegacyIDs[0] != (LegacyActiveID{Kind: LegacyActiveAdminRecord, ID: needed.CheckpointAdminRecords[0].ID}) {
		t.Fatalf("nonresolution admin classification = %#v", needed)
	}
	if err := ValidateUpgradeNeeded(needed); err != nil {
		t.Fatal(err)
	}
}

func validUpgradeNeededWithCheckpointAdmin(t *testing.T) UpgradeNeeded {
	t.Helper()
	resolution := BlockResolution{
		NodeID: "review", BlockedAttempt: 2, Decision: "skip", Actor: "human:operator",
		Reason: "waived", EvidenceRef: "ticket:TCL-507", Timestamp: "2026-07-15T00:00:00Z",
	}
	digest, err := ValidateBlockResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	record := PathV1AdminRecord{
		RunID: "run", AdminType: "block_resolution_recorded", Actor: resolution.Actor,
		ReasonCode: resolution.Reason, EvidenceRef: resolution.EvidenceRef,
		Timestamp: resolution.Timestamp, ResolutionDigest: digest,
	}
	record.ID, err = LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := CheckpointBinding{Generation: 12, Digest: strings.Repeat("b", 64)}
	checkpointID, err := CheckpointLegacyAdminRecordIdentity(checkpoint, record)
	if err != nil {
		t.Fatal(err)
	}
	return UpgradeNeeded{
		Reason: UpgradeLegacyDrainRequired, RunID: record.RunID, LegacyStateSchema: 6,
		Checkpoint: checkpoint, TemplateRef: "demo@sha256:" + strings.Repeat("a", 64),
		TemplateSourceHash: strings.Repeat("c", 64),
		ActiveLegacyIDs:    []LegacyActiveID{{Kind: LegacyActiveBlockResolution, ID: checkpointID}},
		CheckpointAdminRecords: []CheckpointLegacyAdminRecord{{
			ID: checkpointID, LegacyID: record.ID, Record: record, Resolution: &resolution,
		}},
	}
}

func rebindCheckpointAdminIdentity(t *testing.T, needed *UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	oldID := admin.ID
	legacyID, err := LegacyAdminRecordIdentity(admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	admin.Record.ID = legacyID
	admin.LegacyID = legacyID
	admin.ID, err = CheckpointLegacyAdminRecordIdentity(needed.Checkpoint, admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	for i := range needed.ActiveLegacyIDs {
		if needed.ActiveLegacyIDs[i] == (LegacyActiveID{Kind: LegacyActiveBlockResolution, ID: oldID}) {
			needed.ActiveLegacyIDs[i].ID = admin.ID
		}
	}
}

func rebindCheckpointAdminResolution(t *testing.T, needed *UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	admin.Record.Actor = admin.Resolution.Actor
	admin.Record.ReasonCode = admin.Resolution.Reason
	admin.Record.EvidenceRef = admin.Resolution.EvidenceRef
	admin.Record.Timestamp = admin.Resolution.Timestamp
	var err error
	admin.Record.ResolutionDigest, err = BlockResolutionIdentity(*admin.Resolution)
	if err != nil {
		t.Fatal(err)
	}
	rebindCheckpointAdminIdentity(t, needed)
}

func TestCheckpointLegacyAdminIdentityDuplicateGoldenCompatibility(t *testing.T) {
	checkpoint := CheckpointBinding{Generation: 12, Digest: strings.Repeat("a", 64)}
	record := PathV1AdminRecord{
		RunID: "run-7", EventSeq: 12, AdminType: "branch_skip", Actor: "human:johan", ReasonCode: "waived",
		EvidenceRef: "ticket-9", Timestamp: "2026-07-15T00:00:00.123456789Z", ResolutionDigest: "resolution-d",
	}
	legacy0, err := LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	if legacy0 != "21da51f6e6b9cab4fd1f61c619a693121fd31b726eaa32033ea1e123f68f3fd3" {
		t.Fatalf("published legacy index-0 identity changed: %s", legacy0)
	}
	checkpoint0, err := CheckpointLegacyAdminRecordIdentity(checkpoint, record)
	if err != nil {
		t.Fatal(err)
	}
	record.OriginalArrayIndex = 1
	legacy1, err := LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	if legacy1 != "e9dabfc1c60cad9ca3e0b3eff0a00823362d83b4cefe25e4a7910f1f58316724" {
		t.Fatalf("published legacy index-1 identity changed: %s", legacy1)
	}
	checkpoint1, err := CheckpointLegacyAdminRecordIdentity(checkpoint, record)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint0 == checkpoint1 {
		t.Fatal("duplicate legacy admin bodies collapsed across checkpoint indexes")
	}
	// Fill these fixtures from the first run; subsequent runs lock the new
	// checkpoint-composed bytes while the assertions above preserve old bytes.
	const want0 = "81e40593fc3f7d1ce08ac14b224eb0ef944f93a4811db46bd9e50d19f441b0a4"
	const want1 = "0f1ae4b4d94a36a9a14b4fa7e6a5999b81cc86a7ec562de21b0afae69a04fd83"
	if checkpoint0 != want0 || checkpoint1 != want1 {
		t.Fatalf("checkpoint legacy admin identities = %s, %s", checkpoint0, checkpoint1)
	}
	if independent := alternateHash("checkpoint-legacy-admin-record/v1", alternateUint(checkpoint.Generation), alternateString(checkpoint.Digest), alternateString(legacy0)); independent != want0 {
		t.Fatalf("independent checkpoint index-0 identity = %s", independent)
	}
	if independent := alternateHash("checkpoint-legacy-admin-record/v1", alternateUint(checkpoint.Generation), alternateString(checkpoint.Digest), alternateString(legacy1)); independent != want1 {
		t.Fatalf("independent checkpoint index-1 identity = %s", independent)
	}
}
