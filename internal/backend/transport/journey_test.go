package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type journeyProbe struct {
	app.JourneyAPI
	refresh  *app.RefreshHistoryRequest
	start    *app.StartWorkRequest
	evidence *app.RecordWorkEvidenceRequest
	decision *app.DecideWorkRequest
	result   app.WorkRunResult
}

func (p *journeyProbe) RefreshHistory(_ context.Context, r app.RefreshHistoryRequest) (app.HistorySearchResult, error) {
	p.refresh = &r
	return app.HistorySearchResult{}, nil
}
func (p *journeyProbe) StartWork(_ context.Context, r app.StartWorkRequest) (app.WorkRunResult, error) {
	p.start = &r
	return p.result, nil
}
func (p *journeyProbe) InspectWork(context.Context, app.InspectWorkRequest) (app.WorkRunResult, error) {
	return p.result, nil
}
func (p *journeyProbe) RecordWorkEvidence(_ context.Context, r app.RecordWorkEvidenceRequest) (app.WorkRunResult, error) {
	p.evidence = &r
	return p.result, nil
}
func (p *journeyProbe) DecideWork(_ context.Context, r app.DecideWorkRequest) (app.WorkRunResult, error) {
	p.decision = &r
	return p.result, nil
}

func TestJourneyHistoryRefreshAcceptsSourceNamesWithoutNativeScope(t *testing.T) {
	p := &journeyProbe{}
	h := testHandler(t, &applicationProbe{})
	if err := h.RegisterJourneyAPI(p); err != nil {
		t.Fatal(err)
	}
	w := request(h, "POST", "/v2/history/refresh", `{"harness":"claude","source":"configured"}`, "")
	if w.Code != 401 || p.refresh != nil {
		t.Fatalf("unauthenticated refresh: %d", w.Code)
	}
	w = request(h, "POST", "/v2/history/refresh", `{"harness":"claude","source":"configured","scope":{"Source":"/caller-selected"}}`, testCredential)
	if w.Code != 400 || p.refresh != nil {
		t.Fatalf("caller-selected scope accepted: %d", w.Code)
	}
	w = request(h, "POST", "/v2/history/refresh", `{"harness":"claude","source":"configured"}`, testCredential)
	if w.Code != 200 || p.refresh == nil || p.refresh.Harness != "claude" || p.refresh.SourceName != "configured" || p.refresh.Principal.Kind != model.PrincipalOperator {
		t.Fatalf("source name/principal: %d %+v", w.Code, p.refresh)
	}
}

func TestJourneyMutationsDeriveAttributionFromAuthenticatedCaller(t *testing.T) {
	caller := model.ExecutionPrincipal("execution-manager", "agent-manager", 19)
	p := &journeyProbe{}
	h, err := NewHandler(&applicationProbe{}, fixedCaller{caller})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterJourneyAPI(p); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"id":"work-a","request_id":"request-a","authority":{"Kind":"operator"}}`,
		`{"id":"work-a","request_id":"request-a","delegation":{}}`,
		`{"id":"work-a","request_id":"request-a","spec":{"Requester":{"Kind":"operator"}}}`,
	} {
		w := request(h, "POST", "/v2/work", body, "")
		if w.Code != 400 || p.start != nil {
			t.Fatalf("claimed authority accepted: %d", w.Code)
		}
	}
	w := request(h, "POST", "/v2/work", `{"id":"work-a","request_id":"request-a","spec":{"SourceMode":"fresh_handoff","Brief":"bounded work"}}`, "")
	if w.Code != 200 || p.start == nil || p.start.Context.Principal != caller || p.start.Context.RequestID != "request-a" {
		t.Fatalf("start attribution: %d %+v", w.Code, p.start)
	}
	w = request(h, "POST", "/v2/work/evidence", `{"request_id":"evidence-request","work_run_id":"work-a","expected_revision":7,"step":"await_evidence","attempt":2,"kind":"worker_report","detail":"done"}`, "")
	if w.Code != 200 || p.evidence == nil || p.evidence.Context.Principal != caller || p.evidence.Context.RequestID != "evidence-request" || p.evidence.ExpectedRunRevision != 7 {
		t.Fatalf("evidence attribution: %d %+v", w.Code, p.evidence)
	}
	w = request(h, "POST", "/v2/work/decision", `{"request_id":"decision-request","work_run_id":"work-a","expected_revision":8,"step":"evaluate","attempt":2,"decision":"accept","reason":"checked"}`, "")
	if w.Code != 200 || p.decision == nil || p.decision.Context.Principal != caller || p.decision.Context.RequestID != "decision-request" || p.decision.ExpectedRunRevision != 8 {
		t.Fatalf("decision attribution: %d %+v", w.Code, p.decision)
	}
}

func TestJourneyWorkProjectionRetainsUsableRevisionsWithoutPrivateAuthority(t *testing.T) {
	caller := model.ExecutionPrincipal("execution-a", "agent-a", 23)
	caller.Delegation = &model.AutomationDelegation{}
	p := &journeyProbe{result: app.WorkRunResult{
		Run:      model.WorkRun{ID: "work-a", Requester: caller, Delegation: caller.Delegation, HistoryUseID: "private-history-claim", Spec: model.WorkRunSpec{SourceMode: model.WorkSourceFork, History: model.HistorySelection{ConversationID: "conversation-source", ExpectedConversationRevision: 4, PointID: "point-a", ExpectedPointRevision: 3}, Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision}}, WorkerExecutionID: "execution-worker", Revision: 9, Attempts: []model.WorkStepAttempt{{Step: model.WorkStepLaunchWorker, Attempt: 1, OperationID: "operation-launch", State: model.WorkAttemptSucceeded, Detail: "private-native-diagnostic"}}},
		Evidence: []model.WorkEvidence{{ID: "evidence-a", Reporter: caller, Revision: 2}},
		Decision: &model.WorkDecision{Decider: caller, Revision: 1},
	}}
	h := testHandler(t, &applicationProbe{})
	if err := h.RegisterJourneyAPI(p); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/v2/work/work-a", "", testCredential)
	if w.Code != 200 {
		t.Fatalf("work read: %d", w.Code)
	}
	for _, private := range []string{"Generation", "Authority", "Delegation", "private-history-claim", "private-native-diagnostic"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("private field exposed: %s", w.Body)
		}
	}
	var view workResultView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Run.Revision != 9 || view.Run.Spec.History.ExpectedPointRevision != 3 || view.Run.WorkerExecutionID != "execution-worker" || view.Run.Attempts[0].OperationID != "operation-launch" || view.Evidence[0].Revision != 2 || view.Decision.Revision != 1 {
		t.Fatalf("usable read state missing: %s", w.Body)
	}
}
