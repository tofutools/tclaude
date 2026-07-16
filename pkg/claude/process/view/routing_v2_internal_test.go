package view

import (
	"fmt"
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
