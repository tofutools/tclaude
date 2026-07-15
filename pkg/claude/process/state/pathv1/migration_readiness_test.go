package pathv1

import (
	"context"
	"errors"
	"fmt"
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
