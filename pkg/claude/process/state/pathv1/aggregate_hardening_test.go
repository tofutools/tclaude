package pathv1

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIndexForestExactNodeCountLimit(t *testing.T) {
	t.Parallel()
	parents := make(map[string]string, MaxLineageDepth+1)
	parent := ""
	for n := 0; n < MaxLineageDepth; n++ {
		id := fmt.Sprintf("node-%04d", n)
		parents[id] = parent
		parent = id
	}
	var diagnostics []string
	intervals := indexForest(parents, MaxLineageDepth, func(code, id, _ string) {
		diagnostics = append(diagnostics, code+":"+id)
	})
	if len(diagnostics) != 0 || len(intervals) != MaxLineageDepth {
		t.Fatalf("exact-limit forest: intervals=%d diagnostics=%v", len(intervals), diagnostics)
	}

	overID := fmt.Sprintf("node-%04d", MaxLineageDepth)
	parents[overID] = parent
	diagnostics = nil
	intervals = indexForest(parents, MaxLineageDepth, func(code, id, _ string) {
		diagnostics = append(diagnostics, code+":"+id)
	})
	want := "ancestry_depth_over_budget:" + overID
	if len(intervals) != MaxLineageDepth+1 || !reflect.DeepEqual(diagnostics, []string{want}) {
		t.Fatalf("bound+1 forest: intervals=%d diagnostics=%v, want %q", len(intervals), diagnostics, want)
	}
}

func TestAggregateAuthorityFallbackAmbiguity(t *testing.T) {
	t.Parallel()
	t.Run("root reducer multiplicity", func(t *testing.T) {
		routing := NewRoutingState()
		routing.Reservations["reducer-a"] = ActivationReservation{ID: "reducer-a", IsReducing: true, ReducesScopeID: "root"}
		routing.Reservations["reducer-b"] = ActivationReservation{ID: "reducer-b", IsReducing: true, ReducesScopeID: "root"}
		report := InvariantReport{}
		index := aggregateIndex{view: AggregateView{Routing: &routing}, c: diagnosticCollector{report: &report}}
		index.validateRootScopeClosure("scopes.root", ScopeRecord{ID: "root"}, false)
		if !reportHasCode(report, "root_scope_close_authority") || !strings.Contains(report.Diagnostics[0].Message, "want exactly one") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
	})
	t.Run("reducing slot exact context", func(t *testing.T) {
		routing := NewRoutingState()
		routing.Scopes["child-a"] = ScopeRecord{ID: "child-a", ParentScopeID: "root"}
		routing.Scopes["child-b"] = ScopeRecord{ID: "child-b", ParentScopeID: "root"}
		routing.CauseSets["causes"] = CauseSetRecord{Digest: "causes", CauseIDs: []CauseID{}}
		routing.Reservations["reducer-a"] = ActivationReservation{ID: "reducer-a", NodeID: "join", Generation: 1, IsReducing: true, ReducesScopeID: "child-a", State: ReservationClosedNoActivation, CauseDigest: "causes"}
		routing.Reservations["reducer-b"] = ActivationReservation{ID: "reducer-b", NodeID: "join", Generation: 1, IsReducing: true, ReducesScopeID: "child-b", State: ReservationClosedNoActivation, CauseDigest: "causes"}
		slot := PossibleSlotRecord{ID: "slot", SourceNodeID: "join", SourceScopeID: "root", Generation: 1}
		index := aggregateIndex{view: AggregateView{RunID: "run", Routing: &routing}, pathsBySlot: map[PossibleSlotID][]PathID{}, reducingSlotSources: map[slotSourceKey][]ReservationID{}}
		index.indexReducingSlotSources()
		if _, ok := index.slotSettlement(slot); ok {
			t.Fatal("ambiguous reducing slot source accepted")
		}
		delete(routing.Reservations, "reducer-b")
		index.reducingSlotSources = map[slotSourceKey][]ReservationID{}
		index.indexReducingSlotSources()
		if _, ok := index.slotSettlement(slot); !ok {
			t.Fatal("unique reducing slot source rejected")
		}
	})
}

func TestAggregateAmbiguityHardeningPreservesConstructorFixtures(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name string
		make func(*testing.T) AggregateView
	}{
		{"genesis", validGenesisFixture},
		{"parallel-any", validAnyFixture},
		{"parallel-all-non-success", validAllArrivedNonSuccessFixture},
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			if report := ValidateAggregate(fixture.make(t)); !report.Valid() {
				t.Fatalf("constructor fixture rejected: %#v", report.Diagnostics)
			}
		})
	}
}

func TestAggregateDiagnosticsAndCompletionOwnersDeterministic(t *testing.T) {
	t.Parallel()
	view := validGenesisFixture(t)
	for n := 0; n < 256; n++ {
		id := fmt.Sprintf("malformed-%04d", n)
		view.Routing.Paths[id] = PathRecord{ID: "wrong", Kind: "bad", State: "bad", CreatedSeq: -1}
	}
	want := ValidateAggregate(view)
	for run := 0; run < 100; run++ {
		if got := ValidateAggregate(view); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d diagnostics changed\nwant=%#v\n got=%#v", run, want, got)
		}
	}

	completionView, _, _ := validOpenArrivalFixture(t)
	addOpenAuthorityReservation(t, &completionView, "another-target")
	_, err := AssessAggregateCompletion(completionView)
	if err == nil {
		t.Fatal("unsettled aggregate accepted")
	}
	for run := 0; run < 100; run++ {
		_, got := AssessAggregateCompletion(completionView)
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("run %d completion owner changed: want %v, got %v", run, err, got)
		}
	}
}

func TestAggregateSequenceFieldsRejectNegative(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code string
		view func(*testing.T) AggregateView
		edit func(*AggregateView)
	}{
		{"scope", "scope_sequence", validGenesisFixture, func(view *AggregateView) {
			id := view.Authority.Genesis.RootScopeID
			record := view.Routing.Scopes[id]
			record.EventSeq = -1
			view.Routing.Scopes[id] = record
		}},
		{"child scope", "scope_sequence", validAnyFixture, func(view *AggregateView) {
			for _, id := range sortedMapKeys(view.Routing.Scopes) {
				if id == view.Authority.Genesis.RootScopeID {
					continue
				}
				record := view.Routing.Scopes[id]
				record.EventSeq = -1
				view.Routing.Scopes[id] = record
				return
			}
		}},
		{"reservation", "reservation_sequence", validGenesisFixture, func(view *AggregateView) {
			id := view.Authority.Genesis.ReservationID
			record := view.Routing.Reservations[id]
			record.EventSeq = -1
			view.Routing.Reservations[id] = record
		}},
		{"close receipt", "reservation_sequence", validAllArrivedNonSuccessFixture, func(view *AggregateView) {
			for _, id := range sortedMapKeys(view.Routing.Reservations) {
				record := view.Routing.Reservations[id]
				if record.CloseReceipt == nil {
					continue
				}
				record.CloseReceipt.EventSeq = -1
				view.Routing.Reservations[id] = record
				return
			}
		}},
		{"activation", "activation_sequence", validGenesisFixture, func(view *AggregateView) {
			id := view.Authority.Genesis.ActivationID
			record := view.Routing.Activations[id]
			record.EventSeq = -1
			view.Routing.Activations[id] = record
		}},
		{"activation receipt", "activation_sequence", validGenesisFixture, func(view *AggregateView) {
			id := view.Authority.Genesis.ActivationID
			record := view.Routing.Activations[id]
			record.Receipt.EventSeq = -1
			view.Routing.Activations[id] = record
		}},
		{"path", "path_sequence", validGenesisFixture, func(view *AggregateView) {
			id := view.Authority.Genesis.OutputPathID
			record := view.Routing.Paths[id]
			record.CreatedSeq = -1
			view.Routing.Paths[id] = record
		}},
		{"disposition", "disposition_fields", func(t *testing.T) AggregateView {
			view, _, _ := validOpenArrivalFixture(t)
			return view
		}, func(view *AggregateView) {
			id := view.Authority.Genesis.OutputPathID
			record := view.Routing.Paths[id]
			record.Disposition.EventSeq = -1
			view.Routing.Paths[id] = record
		}},
		{"candidate closure", "closure_sequence", validAllArrivedNonSuccessFixture, func(view *AggregateView) {
			id := sortedMapKeys(view.Routing.CandidateClosures)[0]
			record := view.Routing.CandidateClosures[id]
			record.EventSeq = -1
			view.Routing.CandidateClosures[id] = record
		}},
		{"cause", "cause_shape", validAllArrivedNonSuccessFixture, func(view *AggregateView) {
			id := sortedMapKeys(view.Routing.CauseRecords)[0]
			record := view.Routing.CauseRecords[id]
			record.EventSeq = -1
			view.Routing.CauseRecords[id] = record
		}},
		{"detachment", "detachment_sequence", validAnyFixture, func(view *AggregateView) {
			id := sortedMapKeys(view.Routing.Detachments)[0]
			record := view.Routing.Detachments[id]
			record.EventSeq = -1
			view.Routing.Detachments[id] = record
		}},
		{"detached sink receipt", "detached_sink_event", validAnyFixture, func(view *AggregateView) {
			for _, id := range sortedMapKeys(view.Routing.Paths) {
				record := view.Routing.Paths[id]
				if record.DetachedSink == nil {
					continue
				}
				record.DetachedSink.EventSeq = -1
				view.Routing.Paths[id] = record
				return
			}
		}},
		{"admin", "admin_invalid", validGenesisFixture, func(view *AggregateView) {
			view.AdminRecords["admin"] = PathV1AdminRecord{ID: "admin", RunID: view.RunID, EventSeq: -1, AdminType: "test", Actor: "human:test", Timestamp: "2026-07-17T12:00:00Z"}
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			view := test.view(t)
			test.edit(&view)
			if report := ValidateAggregate(view); !reportHasCode(report, test.code) {
				t.Fatalf("diagnostics = %#v, want %q", report.Diagnostics, test.code)
			}
		})
	}
}

func TestAggregateSequenceDomainPreservesLegitimateZeroAndPositive(t *testing.T) {
	t.Parallel()
	view, _, reservationID := validOpenArrivalFixture(t)
	if view.Routing.Reservations[reservationID].EventSeq != 0 {
		t.Fatal("fixture no longer exercises a legitimate zero open-reservation event")
	}
	if report := ValidateAggregate(view); !report.Valid() {
		t.Fatalf("zero open-reservation event rejected: %#v", report.Diagnostics)
	}
	genesis := validGenesisFixture(t)
	root := genesis.Routing.Scopes[genesis.Authority.Genesis.RootScopeID]
	if root.EventSeq <= 0 {
		t.Fatal("fixture no longer exercises a positive open-root event")
	}
	if report := ValidateAggregate(genesis); !report.Valid() {
		t.Fatalf("positive open-root event rejected: %#v", report.Diagnostics)
	}
}

func TestOpenChildScopeEventMatchesExactForkOutput(t *testing.T) {
	t.Parallel()
	source := parallelSplitSource(2)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyParallelInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.base.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	command, err := PlanParallelSplit(t.Context(), input, pathID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceParallelSplit(t.Context(), input, pathID, command)
	if err != nil {
		t.Fatal(err)
	}
	view := projection.aggregate.View()
	for _, id := range sortedMapKeys(view.Routing.Scopes) {
		scope := view.Routing.Scopes[id]
		if scope.ParentScopeID == "" {
			continue
		}
		if scope.State != ScopeOpen || scope.EventSeq != view.Routing.Paths[scope.ForkOutputPathID].UpdatedSeq {
			t.Fatalf("constructor child scope chronology = %#v", scope)
		}
		scope.EventSeq = 0
		view.Routing.Scopes[id] = scope
		if report := ValidateAggregate(view); !reportHasCode(report, "scope_open_event") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
		return
	}
	t.Fatal("parallel split did not create an open child scope")
}

func TestPropagationCursorRequiresExactProcessedPrefix(t *testing.T) {
	t.Parallel()
	base := func(cursor uint32, state PropagationState) (*aggregateIndex, CandidateClosureKey, PropagationIntentID) {
		routing := NewRoutingState()
		reservationID, candidateID := "reservation", "candidate"
		key, _ := CandidateClosureKeyIdentity(reservationID, candidateID)
		frontier := []CandidateClosureKey{key}
		plan, _ := PropagationPlanIdentity(reservationID, candidateID, "cause", 0, frontier)
		intentID, _ := PropagationIntentIdentity("cause", 0, plan)
		intent := PropagationIntent{ID: intentID, RootReservationID: reservationID, RootCandidateID: candidateID, RootCauseDigest: "cause", Cursor: cursor, Frontier: frontier, PlanDigest: plan, State: state, CommandID: "command", EventSeq: 7}
		routing.Propagation[intentID] = intent
		routing.CauseSets["cause"] = CauseSetRecord{Digest: "cause", CauseIDs: []CauseID{}}
		routing.Reservations[reservationID] = ActivationReservation{ID: reservationID, Generation: 1}
		report := InvariantReport{}
		index := &aggregateIndex{
			view:                  AggregateView{Routing: &routing, Commands: map[string]CommandRecord{"command": {ID: "command", Identity: CommandIdentity{Kind: CommandPropagateCandidateClosure, CauseDigest: "cause"}}}},
			c:                     diagnosticCollector{report: &report},
			candidates:            map[candidateKey]CandidateRecord{{reservationID, candidateID}: {ID: candidateID, PossibleSlotIDs: []PossibleSlotID{"slot"}}},
			candidateByClosureKey: map[CandidateClosureKey]candidateKey{key: {reservationID, candidateID}},
			slots:                 map[PossibleSlotID]PossibleSlotRecord{"slot": {ID: "slot"}},
			pathsBySlot:           map[PossibleSlotID][]PathID{},
			pathsByTarget:         map[candidateKey][]PathID{},
			openDescendants:       map[candidateKey]bool{},
			commandRefs:           map[string]struct{}{},
		}
		return index, key, intentID
	}

	t.Run("missing closure", func(t *testing.T) {
		index, _, _ := base(1, PropagationComplete)
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_frontier_unclosed") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("out of order closure", func(t *testing.T) {
		index, key, _ := base(0, PropagationPending)
		index.view.Routing.CandidateClosures[key] = CandidateClosure{CommandID: "command", EventSeq: 7}
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_frontier_out_of_order") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("existing closure can predate cursor advancement", func(t *testing.T) {
		index, key, _ := base(1, PropagationComplete)
		index.view.Routing.CandidateClosures[key] = CandidateClosure{CommandID: "other", EventSeq: 6}
		index.validatePropagation()
		if reportHasCode(*index.c.report, "propagation_frontier_unclosed") || reportHasCode(*index.c.report, "propagation_frontier_out_of_order") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("cursor partitions a multi-entry frontier", func(t *testing.T) {
		setup := func() (*aggregateIndex, CandidateClosureKey, CandidateClosureKey) {
			index, first, oldID := base(1, PropagationPending)
			second, err := CandidateClosureKeyIdentity("reservation", "candidate-2")
			if err != nil {
				t.Fatal(err)
			}
			frontier := []CandidateClosureKey{first, second}
			plan, err := PropagationPlanIdentity("reservation", "candidate", "cause", 0, frontier)
			if err != nil {
				t.Fatal(err)
			}
			intentID, err := PropagationIntentIdentity("cause", 0, plan)
			if err != nil {
				t.Fatal(err)
			}
			intent := index.view.Routing.Propagation[oldID]
			delete(index.view.Routing.Propagation, oldID)
			intent.ID, intent.Frontier, intent.PlanDigest = intentID, frontier, plan
			index.view.Routing.Propagation[intentID] = intent
			candidate := candidateKey{"reservation", "candidate-2"}
			index.candidates[candidate] = CandidateRecord{ID: "candidate-2"}
			index.candidateByClosureKey[second] = candidate
			return index, first, second
		}

		index, _, second := setup()
		index.view.Routing.CandidateClosures[second] = CandidateClosure{}
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_frontier_unclosed") || !reportHasCode(*index.c.report, "propagation_frontier_out_of_order") {
			t.Fatalf("misordered diagnostics = %#v", index.c.report.Diagnostics)
		}

		index, first, _ := setup()
		index.view.Routing.CandidateClosures[first] = CandidateClosure{}
		index.validatePropagation()
		if reportHasCode(*index.c.report, "propagation_frontier_unclosed") || reportHasCode(*index.c.report, "propagation_frontier_out_of_order") {
			t.Fatalf("valid partition diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("arrival is exact no-closure step", func(t *testing.T) {
		index, key, _ := base(1, PropagationComplete)
		candidate := index.candidateByClosureKey[key]
		index.view.Routing.Paths["arrival"] = PathRecord{ID: "arrival", Kind: PathEdge, State: PathConsumed}
		index.pathsByTarget[candidate] = []PathID{"arrival"}
		index.pathsBySlot["slot"] = []PathID{"arrival"}
		index.validatePropagation()
		if reportHasCode(*index.c.report, "propagation_frontier_unclosed") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("terminal fold cannot use arrival exception", func(t *testing.T) {
		index, key, _ := base(1, PropagationComplete)
		candidate := index.candidateByClosureKey[key]
		index.view.Routing.CauseRecords["terminal-cause"] = CauseRecord{ID: "terminal-cause", TerminalKind: TerminalFailed}
		index.view.Routing.Paths["terminal"] = PathRecord{ID: "terminal", Kind: PathEdge, State: PathFailed, TerminalCauseID: "terminal-cause"}
		index.pathsByTarget[candidate] = []PathID{"terminal"}
		index.pathsBySlot["slot"] = []PathID{"terminal"}
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_frontier_unclosed") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("negative sequence", func(t *testing.T) {
		index, _, intentID := base(0, PropagationPending)
		intent := index.view.Routing.Propagation[intentID]
		intent.EventSeq = -1
		index.view.Routing.Propagation[intentID] = intent
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_sequence") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("zero sequence domain", func(t *testing.T) {
		index, _, intentID := base(0, PropagationPending)
		intent := index.view.Routing.Propagation[intentID]
		intent.EventSeq = 0
		index.view.Routing.Propagation[intentID] = intent
		index.validatePropagation()
		if reportHasCode(*index.c.report, "propagation_sequence") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
	t.Run("duplicate frontier", func(t *testing.T) {
		index, key, oldID := base(0, PropagationPending)
		delete(index.view.Routing.Propagation, oldID)
		frontier := []CandidateClosureKey{key, key}
		plan, _ := PropagationPlanIdentity("reservation", "candidate", "cause", 0, frontier)
		intentID, _ := PropagationIntentIdentity("cause", 0, plan)
		index.view.Routing.Propagation[intentID] = PropagationIntent{ID: intentID, RootReservationID: "reservation", RootCandidateID: "candidate", RootCauseDigest: "cause", Frontier: frontier, PlanDigest: plan, State: PropagationPending, CommandID: "command", EventSeq: 7}
		index.validatePropagation()
		if !reportHasCode(*index.c.report, "propagation_frontier_duplicate") {
			t.Fatalf("diagnostics = %#v", index.c.report.Diagnostics)
		}
	})
}

func TestAggregateRejectsEveryAdminResolution(t *testing.T) {
	t.Parallel()
	valid := BlockResolution{NodeID: "node", BlockedAttempt: 1, Decision: "retry", Actor: "human:test", Reason: "retry", EvidenceRef: "ticket:TCL-479", Timestamp: "2026-07-17T12:00:00Z"}
	tests := []struct {
		name       string
		resolution BlockResolution
		codes      []string
	}{
		{"orphan", valid, []string{"admin_resolution_orphan"}},
		{"invalid orphan", BlockResolution{}, []string{"admin_resolution_invalid", "admin_resolution_orphan"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			view := validGenesisFixture(t)
			view.AdminResolutions["unowned"] = test.resolution
			report := ValidateAggregate(view)
			for _, code := range test.codes {
				if !reportHasCode(report, code) {
					t.Fatalf("diagnostics = %#v, want %q", report.Diagnostics, code)
				}
			}
		})
	}
	t.Run("unreferenced by owner", func(t *testing.T) {
		view := validGenesisFixture(t)
		record := PathV1AdminRecord{RunID: view.RunID, AdminType: "block_resolution_recorded", Actor: valid.Actor, ReasonCode: "retry", EvidenceRef: valid.EvidenceRef, Timestamp: valid.Timestamp}
		record.ID, _ = AdminRecordIdentity(record)
		view.AdminRecords[record.ID] = record
		view.AdminResolutions[record.ID] = valid
		if report := ValidateAggregate(view); !reportHasCode(report, "admin_invalid") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
	})
	t.Run("wrong owner type", func(t *testing.T) {
		view := validGenesisFixture(t)
		digest, err := ValidateBlockResolution(valid)
		if err != nil {
			t.Fatal(err)
		}
		record := PathV1AdminRecord{RunID: view.RunID, AdminType: "admin_repair_recorded", Actor: valid.Actor, ReasonCode: "retry", EvidenceRef: valid.EvidenceRef, Timestamp: valid.Timestamp, ResolutionDigest: digest}
		record.ID, err = AdminRecordIdentity(record)
		if err != nil {
			t.Fatal(err)
		}
		view.AdminRecords[record.ID] = record
		view.AdminResolutions[record.ID] = valid
		if report := ValidateAggregate(view); !reportHasCode(report, "admin_invalid") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
	})
	t.Run("block-resolution owner requires resolution", func(t *testing.T) {
		view := validGenesisFixture(t)
		record := PathV1AdminRecord{RunID: view.RunID, AdminType: "block_resolution_recorded", Actor: valid.Actor, ReasonCode: "retry", EvidenceRef: valid.EvidenceRef, Timestamp: valid.Timestamp}
		record.ID, _ = AdminRecordIdentity(record)
		view.AdminRecords[record.ID] = record
		if report := ValidateAggregate(view); !reportHasCode(report, "admin_invalid") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
	})
	t.Run("invalid automatic authority", func(t *testing.T) {
		view := validGenesisFixture(t)
		digest, err := ValidateBlockResolution(valid)
		if err != nil {
			t.Fatal(err)
		}
		record := PathV1AdminRecord{RunID: view.RunID, AdminType: "block_resolution_recorded", Actor: "system", ReasonCode: "retry", EvidenceRef: valid.EvidenceRef, Timestamp: valid.Timestamp, ResolutionDigest: digest}
		record.ID, err = AdminRecordIdentity(record)
		if err != nil {
			t.Fatal(err)
		}
		view.AdminRecords[record.ID] = record
		view.AdminResolutions[record.ID] = valid
		if report := ValidateAggregate(view); !reportHasCode(report, "admin_actor_invalid") {
			t.Fatalf("diagnostics = %#v", report.Diagnostics)
		}
	})
}

func TestMeasureAggregateIncludesEveryRegistryAndExternalReference(t *testing.T) {
	t.Parallel()
	routing := NewRoutingState()
	routing.Paths["path"] = PathRecord{}
	routing.Scopes["scope"] = ScopeRecord{}
	routing.Reservations["reservation"] = ActivationReservation{}
	routing.Activations["activation"] = ActivationRecord{}
	routing.CandidateClosures["closure"] = CandidateClosure{}
	routing.CauseRecords["cause"] = CauseRecord{}
	routing.CauseSets["set"] = CauseSetRecord{}
	routing.DetachmentSets["detachment-set"] = DetachmentSetRecord{}
	routing.Detachments["detachment"] = DetachmentRecord{}
	routing.Propagation["propagation"] = PropagationIntent{}
	view := AggregateView{
		Routing:          &routing,
		Commands:         map[string]CommandRecord{"command": {IdempotencyKey: "idempotency", PayloadHash: "payload", Identity: CommandIdentity{SourceActivationID: "activation", SourcePathID: "path", TargetReservationID: "reservation", InputDigest: "input", CauseDigest: "cause", PlanDigest: "plan"}}},
		SideEffects:      map[string]SideEffectIdentity{"effect": {ActivationID: "activation", SourceCommandID: "command"}},
		AdminRecords:     map[string]PathV1AdminRecord{"admin": {EvidenceRef: "evidence", ResolutionDigest: "resolution"}},
		AdminResolutions: map[string]BlockResolution{"admin": {NodeID: "node", EvidenceRef: "evidence"}},
	}
	usage, err := MeasureAggregate(view)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Records != 14 {
		t.Fatalf("records = %d, want all 14 registries", usage.Records)
	}
	if usage.References != 15 {
		t.Fatalf("external references = %d, want 15", usage.References)
	}
}

func TestMeasureAggregateExternalRecordExactLimitAndBoundPlusOne(t *testing.T) {
	// This fixture deliberately uses empty authoritative records so the test
	// isolates registry cardinality from reference accounting.
	routing := NewRoutingState()
	commands := make(map[string]CommandRecord, MaxRoutingRecords)
	for n := 0; n < MaxRoutingRecords; n++ {
		commands[fmt.Sprintf("command-%06d", n)] = CommandRecord{}
	}
	view := AggregateView{Routing: &routing, Commands: commands}
	usage, err := MeasureAggregate(view)
	if err != nil || usage.Records != MaxRoutingRecords || usage.Validate() != nil {
		t.Fatalf("exact record limit: usage=%#v err=%v validate=%v", usage, err, usage.Validate())
	}
	view.AdminResolutions = map[string]BlockResolution{"over": {NodeID: strings.Repeat("n", 1)}}
	usage, err = MeasureAggregate(view)
	if err != nil || usage.Records != MaxRoutingRecords+1 || usage.References != 0 {
		t.Fatalf("bound+1 measurement: usage=%#v err=%v", usage, err)
	}
	var over *OverBudgetError
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "records" {
		t.Fatalf("bound+1 accepted: %v", err)
	}
}

func TestMeasureAggregateExternalReferenceExactLimitAndBoundPlusOne(t *testing.T) {
	routing := NewRoutingState()
	record := CommandRecord{
		IdempotencyKey: "idempotency",
		PayloadHash:    "payload",
		Identity: CommandIdentity{
			SourceActivationID:  "activation",
			SourcePathID:        "path",
			TargetReservationID: "reservation",
			InputDigest:         "input",
			CauseDigest:         "cause",
			PlanDigest:          "plan",
		},
	}
	commands := make(map[string]CommandRecord, MaxIDReferences/8)
	for n := 0; n < MaxIDReferences/8; n++ {
		commands[fmt.Sprintf("command-%05d", n)] = record
	}
	view := AggregateView{Routing: &routing, Commands: commands}
	usage, err := MeasureAggregate(view)
	if err != nil || usage.References != MaxIDReferences || usage.Validate() != nil {
		t.Fatalf("exact reference limit: usage=%#v err=%v validate=%v", usage, err, usage.Validate())
	}
	view.SideEffects = map[string]SideEffectIdentity{"over": {ActivationID: "over"}}
	usage, err = MeasureAggregate(view)
	if err != nil || usage.References != MaxIDReferences+1 {
		t.Fatalf("bound+1 measurement: usage=%#v err=%v", usage, err)
	}
	var over *OverBudgetError
	if err := usage.Validate(); !errors.As(err, &over) || over.Limit != "references" {
		t.Fatalf("bound+1 accepted: %v", err)
	}
}

func TestMeasureAggregateAdminResolutionOwnerReferenceAtExactLimit(t *testing.T) {
	routing := NewRoutingState()
	resolutions := make(map[string]BlockResolution, 50_000)
	for n := 0; n < 50_000; n++ {
		resolutions[fmt.Sprintf("resolution-%05d", n)] = BlockResolution{NodeID: "node", EvidenceRef: "evidence"}
	}
	record := CommandRecord{
		IdempotencyKey: "idempotency",
		PayloadHash:    "payload",
		Identity: CommandIdentity{
			SourceActivationID:  "activation",
			SourcePathID:        "path",
			TargetReservationID: "reservation",
			InputDigest:         "input",
			CauseDigest:         "cause",
			PlanDigest:          "plan",
		},
	}
	commands := make(map[string]CommandRecord, 31_250)
	for n := 0; n < 31_250; n++ {
		commands[fmt.Sprintf("command-%05d", n)] = record
	}
	view := AggregateView{Routing: &routing, Commands: commands, AdminResolutions: resolutions}
	usage, err := MeasureAggregate(view)
	if err != nil || usage.References != MaxIDReferences || usage.Validate() != nil {
		t.Fatalf("exact resolution-owner reference limit: usage=%#v err=%v validate=%v", usage, err, usage.Validate())
	}
	view.SideEffects = map[string]SideEffectIdentity{"over": {ActivationID: "over"}}
	usage, err = MeasureAggregate(view)
	if err != nil || usage.References != MaxIDReferences+1 {
		t.Fatalf("resolution-owner bound+1: usage=%#v err=%v", usage, err)
	}
}

func BenchmarkIndexForestExactLimit(b *testing.B) {
	for _, count := range []int{MaxLineageDepth, MaxLineageDepth + 1} {
		parents := make(map[string]string, count)
		parent := ""
		for n := 0; n < count; n++ {
			id := fmt.Sprintf("node-%04d", n)
			parents[id] = parent
			parent = id
		}
		b.Run(fmt.Sprintf("nodes-%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				intervals := indexForest(parents, MaxLineageDepth, func(_, _, _ string) {})
				if len(intervals) != count {
					b.Fatal(len(intervals))
				}
			}
		})
	}
}

func BenchmarkMeasureAggregatePreflight(b *testing.B) {
	routing := NewRoutingState()
	emptyCommands := make(map[string]CommandRecord, MaxRoutingRecords+1)
	for n := 0; n <= MaxRoutingRecords; n++ {
		emptyCommands[fmt.Sprintf("record-%06d", n)] = CommandRecord{}
	}
	b.Run("record-overflow", func(b *testing.B) {
		view := AggregateView{Routing: &routing, Commands: emptyCommands}
		b.ReportAllocs()
		for range b.N {
			usage, err := MeasureAggregate(view)
			if err != nil || usage.Records != MaxRoutingRecords+1 || usage.References != 0 {
				b.Fatalf("usage=%#v err=%v", usage, err)
			}
		}
	})

	full := CommandRecord{IdempotencyKey: "idempotency", PayloadHash: "payload", Identity: CommandIdentity{SourceActivationID: "activation", SourcePathID: "path", TargetReservationID: "reservation", InputDigest: "input", CauseDigest: "cause", PlanDigest: "plan"}}
	referenceCommands := make(map[string]CommandRecord, MaxIDReferences/8+1)
	for n := 0; n <= MaxIDReferences/8; n++ {
		referenceCommands[fmt.Sprintf("reference-%05d", n)] = full
	}
	trailing := make(map[string]BlockResolution, 100_000)
	for n := 0; n < 100_000; n++ {
		trailing[fmt.Sprintf("trailing-%06d", n)] = BlockResolution{NodeID: "node", EvidenceRef: "evidence"}
	}
	for _, fixture := range []struct {
		name        string
		resolutions map[string]BlockResolution
	}{{"reference-overflow", nil}, {"reference-overflow-with-100k-trailing-records", trailing}} {
		b.Run(fixture.name, func(b *testing.B) {
			view := AggregateView{Routing: &routing, Commands: referenceCommands, AdminResolutions: fixture.resolutions}
			b.ReportAllocs()
			for range b.N {
				usage, err := MeasureAggregate(view)
				if err != nil || usage.References != MaxIDReferences+1 {
					b.Fatalf("usage=%#v err=%v", usage, err)
				}
			}
		})
	}
}
