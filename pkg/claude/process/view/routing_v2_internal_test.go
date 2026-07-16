package view

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func TestProjectRoutingOverlayIndexesHighCardinalityJoinArrivals(t *testing.T) {
	const candidates = 2046
	routing := pathv1.NewRoutingState()
	reservation := pathv1.ActivationReservation{
		ID: "join", NodeID: "merge", ScopeID: "scope", Generation: 1,
		JoinPolicy: pathv1.JoinAll, State: pathv1.ReservationOpen,
		Candidates: make([]pathv1.CandidateRecord, 0, candidates),
	}
	edge := pathv1.EdgeKey{TemplateRef: "template", ID: "edge", FromNodeID: "branch", Outcome: "pass", ToNodeID: "merge"}
	for index := 0; index < candidates; index++ {
		candidateID := pathv1.CandidateID(fmt.Sprintf("candidate-%04d", index))
		reservation.Candidates = append(reservation.Candidates, pathv1.CandidateRecord{ID: candidateID, Kind: pathv1.CandidateInboundEdge, MemberID: string(edge.ID)})
		if index%2 == 0 {
			pathID := pathv1.PathID(fmt.Sprintf("path-%04d", index))
			routing.Paths[pathID] = pathv1.PathRecord{ID: pathID, Kind: pathv1.PathEdge, State: pathv1.PathArrived, Edge: &edge, TargetReservationID: reservation.ID, CandidateID: candidateID}
		}
	}
	routing.Reservations[reservation.ID] = reservation
	arrivals := make(map[routingArrivalKey]struct{}, candidates/2)
	for _, path := range routing.Paths {
		arrivals[routingArrivalKey{reservationID: path.TargetReservationID, candidateID: path.CandidateID}] = struct{}{}
	}
	joins, ok := projectRoutingJoins(&routing, arrivals)
	if !ok || len(joins) != 1 {
		t.Fatalf("joins = %#v, ok=%v", joins, ok)
	}
	join := joins[0]
	if join.Arrived != candidates/2 || join.Open != candidates/2 || join.Impossible != 0 || join.Failed != 0 || join.Skipped != 0 || join.Canceled != 0 {
		t.Fatalf("indexed join counts = %#v", join)
	}
}

func TestProjectRoutingDetailsPagesCompleteTypedRecordsAndFailsClosed(t *testing.T) {
	routing := pathv1.NewRoutingState()
	routing.Reservations["reservation-b"] = pathv1.ActivationReservation{
		ID: "reservation-b", NodeID: "node-b", ScopeID: "scope-b", Generation: 2,
		JoinPolicy: pathv1.JoinAll, State: pathv1.ReservationOpen,
	}
	routing.Reservations["reservation-a"] = pathv1.ActivationReservation{
		ID: "reservation-a", NodeID: "node-a", ScopeID: "scope-a", Generation: 1,
		JoinPolicy: pathv1.JoinAny, State: pathv1.ReservationOpen,
	}
	routing.Scopes["scope-b"] = pathv1.ScopeRecord{ID: "scope-b", Generation: 2, State: pathv1.ScopeOpen}
	routing.Scopes["scope-a"] = pathv1.ScopeRecord{ID: "scope-a", Generation: 1, State: pathv1.ScopeOpen}
	routing.CauseRecords["cause-b"] = pathv1.CauseRecord{ID: "cause-b", TerminalKind: pathv1.TerminalFailed, DispositionReason: "failed", EventSeq: 8}
	routing.CauseRecords["cause-a"] = pathv1.CauseRecord{ID: "cause-a", TerminalKind: pathv1.TerminalImpossible, DispositionReason: "exclusive_unselected/" + strings.Repeat("a", 64) + "/" + strings.Repeat("b", 64), EventSeq: 7}
	routing.CauseSets["digest-b"] = pathv1.CauseSetRecord{Digest: "digest-b", CauseIDs: []pathv1.CauseID{"cause-b"}}
	routing.CauseSets["digest-a"] = pathv1.CauseSetRecord{Digest: "digest-a", CauseIDs: []pathv1.CauseID{"cause-a"}}
	routing.CandidateClosures["closure-b"] = pathv1.CandidateClosure{Key: pathv1.CandidateClosureKeyRecord{ReservationID: "reservation-b", CandidateID: "candidate-b"}, TerminalKind: pathv1.TerminalFailed, CauseDigest: "digest-b"}
	routing.CandidateClosures["closure-a"] = pathv1.CandidateClosure{Key: pathv1.CandidateClosureKeyRecord{ReservationID: "reservation-a", CandidateID: "candidate-a"}, TerminalKind: pathv1.TerminalImpossible, CauseDigest: "digest-a"}
	routing.Detachments["detachment-b"] = pathv1.DetachmentRecord{ID: "detachment-b", ReservationID: "reservation-b", CandidateID: "candidate-b", WinnerPathID: "winner-b", JoinActivation: pathv1.ActivationRef{ID: "join-b", Generation: 2}, ReasonCode: "parallel_any_loser", ActivatedSeq: 10}
	routing.Detachments["detachment-a"] = pathv1.DetachmentRecord{ID: "detachment-a", ReservationID: "reservation-a", CandidateID: "candidate-a", WinnerPathID: "winner-a", JoinActivation: pathv1.ActivationRef{ID: "join-a", Generation: 1}, ReasonCode: "parallel_any_loser", ActivatedSeq: 9}
	routing.Paths["sink-b"] = pathv1.PathRecord{ID: "sink-b", State: pathv1.PathDetachedSink, SourceActivation: pathv1.ActivationRef{ID: "source-b", Generation: 2}, TargetReservationID: "reservation-b", CandidateID: "candidate-b", DetachedSink: &pathv1.DetachedSinkReceipt{DetachmentID: "detachment-b", ReasonCode: "parallel_any_loser", EventSeq: 12}}
	routing.Paths["sink-a"] = pathv1.PathRecord{ID: "sink-a", State: pathv1.PathDetachedSink, SourceActivation: pathv1.ActivationRef{ID: "source-a", Generation: 1}, TargetReservationID: "reservation-a", CandidateID: "candidate-a", DetachedSink: &pathv1.DetachedSinkReceipt{DetachmentID: "detachment-a", ReasonCode: "parallel_any_loser", EventSeq: 11}}

	details, sinkCount, ok := projectRoutingDetails(&routing, RoutingPageRequestV2{Offset: 1, Limit: 1})
	if !ok || sinkCount != 2 {
		t.Fatalf("details unavailable: ok=%v sinkCount=%d", ok, sinkCount)
	}
	if details.Generations.Page != (RoutingPageV2{Offset: 1, Limit: 1, Total: 2, HasMore: false}) ||
		len(details.Generations.Items) != 1 || details.Generations.Items[0].NodeID != "node-b" {
		t.Fatalf("generation page = %#v", details.Generations)
	}
	if len(details.Closures.Items) != 1 || details.Closures.Items[0].CauseDigest != "digest-b" ||
		len(details.CauseSets.Items) != 1 || details.CauseSets.Items[0].CauseIDs[0] != "cause-b" ||
		len(details.Causes.Items) != 1 || details.Causes.Items[0].DispositionReason != "failed" ||
		len(details.Detachments.Items) != 1 || details.Detachments.Items[0].JoinActivationGeneration != 2 ||
		len(details.DetachedSinks.Items) != 1 || details.DetachedSinks.Items[0].PathID != "sink-b" {
		t.Fatalf("typed detail pages = %#v", details)
	}

	unsafe := routing.CauseRecords["cause-a"]
	unsafe.DispositionReason = "unsafe\nreason"
	routing.CauseRecords["cause-a"] = unsafe
	if _, _, ok := projectRoutingDetails(&routing, RoutingPageRequestV2{}); ok {
		t.Fatal("unsafe checkpoint reason projected instead of failing closed")
	}
}

func TestRoutingPageRequestNormalizesToBoundedStableWindows(t *testing.T) {
	items := []string{"a", "b", "c"}
	page := routingPage(len(items), RoutingPageRequestV2{Offset: -9, Limit: MaxRoutingPageLimit + 1})
	if page.Offset != 0 || page.Limit != MaxRoutingPageLimit || page.Total != 3 || page.HasMore {
		t.Fatalf("normalized page = %#v", page)
	}
	if got := routingPageItems(items, RoutingPageRequestV2{Offset: 1, Limit: 1}); len(got) != 1 || got[0] != "b" {
		t.Fatalf("paged items = %#v", got)
	}
	if got := routingPageItems(items, RoutingPageRequestV2{Offset: 99, Limit: 1}); got == nil || len(got) != 0 {
		t.Fatalf("past-end page = %#v", got)
	}
}
