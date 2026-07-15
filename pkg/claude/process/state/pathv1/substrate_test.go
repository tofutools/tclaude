package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestClosureFoldEveryCombination(t *testing.T) {
	t.Parallel()
	kinds := []TerminalKind{TerminalImpossible, TerminalCanceled, TerminalSkipped, TerminalFailed}
	for length := 1; length <= 6; length++ {
		total := 1
		for range length {
			total *= len(kinds)
		}
		for encoded := 0; encoded < total; encoded++ {
			n := encoded
			values := make([]TerminalKind, length)
			want := TerminalImpossible
			for i := range values {
				values[i] = kinds[n%len(kinds)]
				n /= len(kinds)
				switch {
				case values[i] == TerminalFailed:
					want = TerminalFailed
				case values[i] == TerminalSkipped && want != TerminalFailed:
					want = TerminalSkipped
				case values[i] == TerminalCanceled && want == TerminalImpossible:
					want = TerminalCanceled
				}
			}
			got, err := FoldTerminalKinds(values)
			if err != nil || got != want {
				t.Fatalf("fold %v = %q, %v; want %q", values, got, err, want)
			}
		}
	}
}

func TestCandidateSlotsMissingAndOpenStayOpen(t *testing.T) {
	t.Parallel()
	candidate := CandidateRecord{ID: "candidate", PossibleSlotIDs: []string{"a", "b"}}
	entry, _, _, err := FoldCandidateSlots("reservation", candidate, map[string]SlotSettlement{
		"a": {CauseIDs: []string{"cause"}, CauseKinds: []TerminalKind{TerminalFailed}},
	}, false)
	if err != nil || entry.FoldKind != CandidateFoldOpen {
		t.Fatalf("missing slot fold = %#v, %v", entry, err)
	}
	settled := map[string]SlotSettlement{
		"a": {CauseIDs: []string{"failed"}, CauseKinds: []TerminalKind{TerminalFailed}},
		"b": {CauseIDs: []string{"canceled"}, CauseKinds: []TerminalKind{TerminalCanceled}},
	}
	entry, _, _, err = FoldCandidateSlots("reservation", candidate, settled, true)
	if err != nil || entry.FoldKind != CandidateFoldOpen {
		t.Fatalf("open-descendant fold = %#v, %v", entry, err)
	}
	entry, causes, kind, err := FoldCandidateSlots("reservation", candidate, settled, false)
	if err != nil || kind != TerminalFailed || entry.FoldKind != "failed" || !slices.Equal(causes, []string{"canceled", "failed"}) {
		t.Fatalf("settled fold = %#v %v %q %v", entry, causes, kind, err)
	}
}

func TestLineageBoundAndReservationRelativeDetachment(t *testing.T) {
	t.Parallel()
	path := PathRecord{}
	parent := ""
	for i := 0; i < MaxLineageDepth; i++ {
		reservation := fmt.Sprintf("r-%04d", i)
		candidate := fmt.Sprintf("c-%04d", i)
		id, err := CandidateLineageIdentity(parent, reservation, candidate)
		if err != nil {
			t.Fatal(err)
		}
		path.CandidateLineage = append(path.CandidateLineage, CandidateLineageFrame{ID: id, ParentLineageID: parent, ReservationID: reservation, CandidateID: candidate})
		parent = id
	}
	path.CandidateLineageID = parent
	path.LineageDepth = MaxLineageDepth
	if err := ValidateLineage(path); err != nil {
		t.Fatal(err)
	}
	st := NewRoutingState()
	key, err := DetachmentKeyIdentity("r-2048", "c-2048")
	if err != nil {
		t.Fatal(err)
	}
	st.Detachments[key] = DetachmentRecord{}
	if detached, err := DetachedFrom(&st, path, "r-2048"); err != nil || !detached {
		t.Fatalf("detached = %v, %v", detached, err)
	}
	if detached, err := DetachedFrom(&st, path, "r-other"); err != nil || detached {
		t.Fatalf("unrelated detached = %v, %v", detached, err)
	}
	path.CandidateLineage = append(path.CandidateLineage, CandidateLineageFrame{})
	path.LineageDepth++
	var over *OverBudgetError
	if err := ValidateLineage(path); !errors.As(err, &over) {
		t.Fatalf("bound+1 error = %v", err)
	}
}

func TestOperationalBoundsAndCheckedFormulas(t *testing.T) {
	t.Parallel()
	checks := []struct {
		name  string
		limit int
		set   func(*Usage, int)
	}{
		{"paths", MaxPathRecords, func(u *Usage, n int) { u.Paths = n }},
		{"records", MaxRoutingRecords, func(u *Usage, n int) { u.Records = n }},
		{"references", MaxIDReferences, func(u *Usage, n int) { u.References = n }},
		{"list", MaxRoutingList, func(u *Usage, n int) { u.LargestList = n }},
		{"mutations", MaxRoutingMutations, func(u *Usage, n int) { u.Mutations = n }},
		{"logs", MaxRoutingLogEntries, func(u *Usage, n int) { u.LogEntries = n }},
		{"payload", MaxCommandPayloadBytes, func(u *Usage, n int) { u.PayloadBytes = n }},
		{"checkpoint", MaxCheckpointBytes, func(u *Usage, n int) { u.CheckpointBytes = n }},
	}
	for _, check := range checks {
		check := check
		t.Run(check.name, func(t *testing.T) {
			var usage Usage
			check.set(&usage, check.limit)
			if err := usage.Validate(); err != nil {
				t.Fatalf("at limit: %v", err)
			}
			check.set(&usage, check.limit+1)
			var over *OverBudgetError
			if err := usage.Validate(); !errors.As(err, &over) {
				t.Fatalf("limit+1 = %v", err)
			}
			check.set(&usage, -1)
			if err := usage.Validate(); err == nil {
				t.Fatal("negative usage accepted")
			}
		})
	}
	if n, err := MutationCountAny(MaxAnyCandidates, MaxAnyCandidates-1); err != nil || n != 4094 {
		t.Fatalf("M_any max = %d, %v", n, err)
	}
	if _, err := MutationCountAny(MaxAnyCandidates+1, 0); err == nil {
		t.Fatal("any candidate bound+1 accepted")
	}
	if n, err := MutationCountExclusive(MaxOutgoingOrAllCandidates); err != nil || n != 4093 {
		t.Fatalf("M_exclusive max = %d, %v", n, err)
	}
	if _, err := MutationCountExclusive(MaxOutgoingOrAllCandidates + 1); err == nil {
		t.Fatal("outgoing bound+1 accepted")
	}
	if _, err := Decode(make([]byte, MaxCheckpointBytes+1)); err == nil {
		t.Fatal("oversize checkpoint accepted")
	}
}

func TestStrictEncodeDecodeEnvelopeAndClone(t *testing.T) {
	t.Parallel()
	st := NewRoutingState()
	data, err := Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st, *decoded) {
		t.Fatalf("round trip differs\n%#v\n%#v", st, *decoded)
	}
	unknown := []byte(`{"protocol":"path_v1","encoding":1,"paths":{},"scopes":{},"reservations":{},"activations":{},"candidateClosures":{},"causeRecords":{},"causeSets":{},"detachmentSets":{},"detachments":{},"propagation":{},"unknown":true}`)
	if _, err := Decode(unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
	invalid := st
	invalid.Protocol = "other"
	if _, err := Encode(&invalid); err == nil {
		t.Fatal("invalid envelope accepted")
	}
	st.Paths["p"] = PathRecord{ID: "p", ProducedPathIDs: []string{"child"}, Edge: &EdgeKey{ID: "edge"}, Disposition: &DispositionReceipt{ID: "receipt"}}
	clone := Clone(st)
	record := clone.Paths["p"]
	record.ProducedPathIDs[0] = "changed"
	record.Edge.ID = "changed"
	record.Disposition.ID = "changed"
	clone.Paths["p"] = record
	if st.Paths["p"].ProducedPathIDs[0] != "child" || st.Paths["p"].Edge.ID != "edge" || st.Paths["p"].Disposition.ID != "receipt" {
		t.Fatal("clone aliases source")
	}
}

func TestClonePreservesEmptySlicesAndDeepCopiesEveryNestedValue(t *testing.T) {
	t.Parallel()
	st := NewRoutingState()
	st.Paths["p"] = PathRecord{
		ProducedPathIDs: []PathID{}, CandidateLineage: []CandidateLineageFrame{},
		Edge: &EdgeKey{ID: "edge"}, ConsumedBy: &ActivationRef{ID: "consumer"},
		Disposition:  &DispositionReceipt{ID: "disposition"},
		DetachedSink: &DetachedSinkReceipt{DetachmentID: "detachment"},
	}
	st.Scopes["s"] = ScopeRecord{ExpectedBranchEdgeIDs: []EdgeID{}}
	st.Reservations["r"] = ActivationReservation{
		Candidates:    []CandidateRecord{{ID: "candidate", PossibleSlotIDs: []PossibleSlotID{}}},
		PossibleSlots: []PossibleSlotRecord{}, Activation: &ActivationRef{ID: "activation"},
		CloseReceipt: &ActivationReceipt{ID: "receipt"},
	}
	st.Activations["a"] = ActivationRecord{InputPathIDs: []PathID{}}
	st.CauseSets["c"] = CauseSetRecord{CauseIDs: []CauseID{}}
	st.Propagation["i"] = PropagationIntent{Frontier: []CandidateClosureKey{}}

	clone := Clone(st)
	if !reflect.DeepEqual(st, clone) {
		t.Fatalf("clone changed exact shape\nsource: %#v\nclone:  %#v", st, clone)
	}
	if zero := Clone(RoutingState{}); !reflect.DeepEqual(zero, RoutingState{}) {
		t.Fatalf("clone changed nil maps in zero value: %#v", zero)
	}
	if clone.Paths["p"].ProducedPathIDs == nil || clone.Paths["p"].CandidateLineage == nil ||
		clone.Scopes["s"].ExpectedBranchEdgeIDs == nil || clone.Reservations["r"].Candidates == nil ||
		clone.Reservations["r"].Candidates[0].PossibleSlotIDs == nil || clone.Reservations["r"].PossibleSlots == nil ||
		clone.Activations["a"].InputPathIDs == nil || clone.CauseSets["c"].CauseIDs == nil ||
		clone.Propagation["i"].Frontier == nil {
		t.Fatal("clone collapsed a non-nil empty slice")
	}
	data, err := Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	// Optional empty path lists are omitted by their schema tags. Every
	// required list must remain [] rather than being collapsed to null.
	if decoded.Scopes["s"].ExpectedBranchEdgeIDs == nil || decoded.Reservations["r"].Candidates == nil ||
		decoded.Reservations["r"].Candidates[0].PossibleSlotIDs == nil || decoded.Reservations["r"].PossibleSlots == nil ||
		decoded.Activations["a"].InputPathIDs == nil || decoded.CauseSets["c"].CauseIDs == nil ||
		decoded.Propagation["i"].Frontier == nil {
		t.Fatal("encode changed a required non-nil empty list to null")
	}

	// Populate every nested slice and pointer, clone again, then mutate only
	// the clone to prove that no backing storage or pointed-to record aliases.
	path := st.Paths["p"]
	path.ProducedPathIDs = []PathID{"child"}
	path.CandidateLineage = []CandidateLineageFrame{{ID: "frame"}}
	st.Paths["p"] = path
	scope := st.Scopes["s"]
	scope.ExpectedBranchEdgeIDs = []EdgeID{"branch"}
	st.Scopes["s"] = scope
	reservation := st.Reservations["r"]
	reservation.Candidates[0].PossibleSlotIDs = []PossibleSlotID{"slot"}
	reservation.PossibleSlots = []PossibleSlotRecord{{ID: "slot"}}
	st.Reservations["r"] = reservation
	activation := st.Activations["a"]
	activation.InputPathIDs = []PathID{"input"}
	st.Activations["a"] = activation
	causeSet := st.CauseSets["c"]
	causeSet.CauseIDs = []CauseID{"cause"}
	st.CauseSets["c"] = causeSet
	intent := st.Propagation["i"]
	intent.Frontier = []CandidateClosureKey{"closure"}
	st.Propagation["i"] = intent

	clone = Clone(st)
	clonePath := clone.Paths["p"]
	clonePath.ProducedPathIDs[0] = "changed"
	clonePath.CandidateLineage[0].ID = "changed"
	clonePath.Edge.ID = "changed"
	clonePath.ConsumedBy.ID = "changed"
	clonePath.Disposition.ID = "changed"
	clonePath.DetachedSink.DetachmentID = "changed"
	clone.Paths["p"] = clonePath
	cloneScope := clone.Scopes["s"]
	cloneScope.ExpectedBranchEdgeIDs[0] = "changed"
	clone.Scopes["s"] = cloneScope
	cloneReservation := clone.Reservations["r"]
	cloneReservation.Candidates[0].PossibleSlotIDs[0] = "changed"
	cloneReservation.PossibleSlots[0].ID = "changed"
	cloneReservation.Activation.ID = "changed"
	cloneReservation.CloseReceipt.ID = "changed"
	clone.Reservations["r"] = cloneReservation
	cloneActivation := clone.Activations["a"]
	cloneActivation.InputPathIDs[0] = "changed"
	clone.Activations["a"] = cloneActivation
	cloneCauseSet := clone.CauseSets["c"]
	cloneCauseSet.CauseIDs[0] = "changed"
	clone.CauseSets["c"] = cloneCauseSet
	cloneIntent := clone.Propagation["i"]
	cloneIntent.Frontier[0] = "changed"
	clone.Propagation["i"] = cloneIntent
	if st.Paths["p"].ProducedPathIDs[0] != "child" || st.Paths["p"].CandidateLineage[0].ID != "frame" ||
		st.Paths["p"].Edge.ID != "edge" || st.Paths["p"].ConsumedBy.ID != "consumer" ||
		st.Paths["p"].Disposition.ID != "disposition" || st.Paths["p"].DetachedSink.DetachmentID != "detachment" ||
		st.Scopes["s"].ExpectedBranchEdgeIDs[0] != "branch" ||
		st.Reservations["r"].Candidates[0].PossibleSlotIDs[0] != "slot" ||
		st.Reservations["r"].PossibleSlots[0].ID != "slot" || st.Reservations["r"].Activation.ID != "activation" ||
		st.Reservations["r"].CloseReceipt.ID != "receipt" || st.Activations["a"].InputPathIDs[0] != "input" ||
		st.CauseSets["c"].CauseIDs[0] != "cause" || st.Propagation["i"].Frontier[0] != "closure" {
		t.Fatal("clone aliases source nested storage")
	}

	clone.Paths["new"] = PathRecord{}
	clone.Scopes["new"] = ScopeRecord{}
	clone.Reservations["new"] = ActivationReservation{}
	clone.Activations["new"] = ActivationRecord{}
	clone.CandidateClosures["new"] = CandidateClosure{}
	clone.CauseRecords["new"] = CauseRecord{}
	clone.CauseSets["new"] = CauseSetRecord{}
	clone.DetachmentSets["new"] = DetachmentSetRecord{}
	clone.Detachments["new"] = DetachmentRecord{}
	clone.Propagation["new"] = PropagationIntent{}
	if len(st.Paths) != 1 || len(st.Scopes) != 1 || len(st.Reservations) != 1 || len(st.Activations) != 1 ||
		len(st.CandidateClosures) != 0 || len(st.CauseRecords) != 0 || len(st.CauseSets) != 1 ||
		len(st.DetachmentSets) != 0 || len(st.Detachments) != 0 || len(st.Propagation) != 1 {
		t.Fatal("clone aliases a source map")
	}
}

func TestDecodeRejectsDuplicateKeysAndMalformedUnicode(t *testing.T) {
	t.Parallel()
	envelope := func(paths string) []byte {
		return []byte(fmt.Sprintf(`{"protocol":"path_v1","encoding":1,"paths":%s,"scopes":{},"reservations":{},"activations":{},"candidateClosures":{},"causeRecords":{},"causeSets":{},"detachmentSets":{},"detachments":{},"propagation":{}}`, paths))
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"duplicate envelope key", []byte(`{"protocol":"path_v1","protocol":"path_v1","encoding":1,"paths":{},"scopes":{},"reservations":{},"activations":{},"candidateClosures":{},"causeRecords":{},"causeSets":{},"detachmentSets":{},"detachments":{},"propagation":{}}`)},
		{"duplicate map key", envelope(`{"p":{},"p":{}}`)},
		{"duplicate nested member", envelope(`{"p":{"id":"p","id":"q"}}`)},
		{"unpaired high surrogate", envelope(`{"\ud800":{}}`)},
		{"unpaired low surrogate", envelope(`{"p":{"id":"\udc00"}}`)},
	}
	valid := envelope(`{}`)
	invalidUTF8 := append([]byte(nil), valid...)
	invalidUTF8[2] = 0xff
	tests = append(tests, struct {
		name string
		data []byte
	}{"invalid UTF-8", invalidUTF8})
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode(test.data); err == nil {
				t.Fatal("malformed JSON accepted")
			}
		})
	}
	if _, err := Decode(valid); err != nil {
		t.Fatalf("valid strict JSON rejected: %v", err)
	}
}

func TestJCSProjectionAndDuplicateRejection(t *testing.T) {
	t.Parallel()
	input := []byte(`{"status":"running","lastLogSeq":9,"logChecksum":"x","outstandingCommands":{"self":{"state":"issued"},"other":{"n":1E-7}},"z":"<>&","a":0.000001}`)
	got, err := CanonicalCheckpointProjection(input, "self")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":0.000001,"outstandingCommands":{"other":{"n":1e-7}},"z":"<>&"}`
	if string(got) != want {
		t.Fatalf("projection = %s, want %s", got, want)
	}
	if _, err := CanonicalCheckpointProjection([]byte(`{"a":1,"a":2}`), ""); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := CanonicalCheckpointProjection([]byte(`{"bad":"\ud800"}`), ""); err == nil {
		t.Fatal("unpaired surrogate accepted")
	}
}

func TestCanonicalTimestamp(t *testing.T) {
	t.Parallel()
	value := time.Date(2026, 7, 15, 2, 0, 0, 123456789, time.FixedZone("x", 2*60*60))
	canonical := CanonicalTimestamp(value)
	if canonical != "2026-07-15T00:00:00.123456789Z" {
		t.Fatalf("canonical = %q", canonical)
	}
	if _, err := ParseCanonicalTimestamp(canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalTimestamp("2026-07-15T02:00:00+02:00"); err == nil {
		t.Fatal("noncanonical offset accepted")
	}
}

func TestLineageAppendAndCommonPop(t *testing.T) {
	t.Parallel()
	root := PathRecord{}
	leftFrames, leftID, err := AppendCandidateLineage(root, "outer", "left")
	if err != nil {
		t.Fatal(err)
	}
	left := PathRecord{CandidateLineage: leftFrames, CandidateLineageID: leftID, LineageDepth: 1}
	aFrames, aID, err := AppendCandidateLineage(left, "inner", "a")
	if err != nil {
		t.Fatal(err)
	}
	bFrames, bID, err := AppendCandidateLineage(left, "inner", "b")
	if err != nil {
		t.Fatal(err)
	}
	a := PathRecord{CandidateLineage: aFrames, CandidateLineageID: aID, LineageDepth: 2}
	b := PathRecord{CandidateLineage: bFrames, CandidateLineageID: bID, LineageDepth: 2}
	remainder, remainderID, err := PopConsumedLineage([]PathRecord{a, b}, "inner")
	if err != nil {
		t.Fatal(err)
	}
	if len(remainder) != 1 || remainderID != leftID {
		t.Fatalf("remainder = %#v, %q", remainder, remainderID)
	}
	if _, _, err := PopConsumedLineage([]PathRecord{a, left}, "inner"); err == nil {
		t.Fatal("mismatched reservation pop accepted")
	}
}

func TestAdminResolutionAndDuplicateLegacyIndices(t *testing.T) {
	t.Parallel()
	resolution := BlockResolution{NodeID: "node", BlockedAttempt: 2, Decision: "skip", Actor: "human:johan", Reason: "waived", EvidenceRef: "ticket", Timestamp: "2026-07-15T00:00:00.123456789Z"}
	digest, err := ValidateBlockResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]PathV1AdminRecord, 2)
	for i := range records {
		records[i] = PathV1AdminRecord{RunID: "run", OriginalArrayIndex: uint64(i), AdminType: "branch_skip", Actor: "human:johan", ReasonCode: "waived", EvidenceRef: "ticket", Timestamp: resolution.Timestamp, ResolutionDigest: digest}
		records[i].ID, err = LegacyAdminRecordIdentity(records[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateAdminRecord(records[i], true, &resolution); err != nil {
			t.Fatal(err)
		}
	}
	if records[0].ID == records[1].ID {
		t.Fatal("duplicate legacy indices collapsed")
	}
}

func TestCommandAndSideEffectIdentityRules(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeOf(CommandRecord{}).FieldByName("Completion"); ok {
		t.Fatal("primitive command record exposes an unauthenticated completion basis")
	}
	identity := CommandIdentity{RunID: "run", Kind: CommandPerformAttempt, PayloadSchema: 1, SourceActivationID: "activation", SourceGeneration: 1, Attempt: 2, PlanDigest: "plan"}
	id, err := CommandIdentityDigest(identity)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"x":1}`)
	sum := sha256.Sum256(payload)
	record := CommandRecord{ID: id, IdempotencyKey: CommandIdempotencyKey(identity.Kind, id), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: CommandIssued}
	if err := ValidateCommand(record); err != nil {
		t.Fatal(err)
	}
	record.Identity.ResultCode = "unexpected"
	if err := ValidateCommand(record); err == nil {
		t.Fatal("noncanonical unused command field accepted")
	}
	completeIdentity := CommandIdentity{RunID: "run", Kind: CommandCompleteRun, PayloadSchema: 1, InputDigest: "aggregate", PlanDigest: "active-commands", ResultCode: "completed"}
	completeID, err := CommandIdentityDigest(completeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	completePayload := json.RawMessage(`{"completionBasis":{}}`)
	completeSum := sha256.Sum256(completePayload)
	complete := CommandRecord{ID: completeID, IdempotencyKey: CommandIdempotencyKey(completeIdentity.Kind, completeID), Identity: completeIdentity, Payload: completePayload, PayloadHash: hex.EncodeToString(completeSum[:]), State: CommandIssued}
	if err := ValidateCommand(complete); err == nil {
		t.Fatal("primitive validator accepted an unauthenticated completion command")
	}
	effect := SideEffectIdentity{Kind: SideEffectWait, RunID: "run", ActivationID: "activation", Attempt: 2, WaitKind: "human", State: "pending"}
	effect.ID, err = WaitIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.WaitKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSideEffect(effect); err != nil {
		t.Fatal(err)
	}
	effect.Assignee = "unexpected"
	if err := ValidateSideEffect(effect); err == nil {
		t.Fatal("noncanonical unused side-effect field accepted")
	}
}
