package transport

import (
	"context"
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type commandProbe struct {
	app.API
	launchCalls int
	launch      app.LaunchRequest
	resumeCalls int
	resume      app.ResumeRequest
}

func (p *commandProbe) Launch(_ context.Context, r app.LaunchRequest) (app.OperationResult, error) {
	p.launchCalls++
	p.launch = r
	return app.OperationResult{Operation: model.Operation{ID: "operation-a"}, Execution: &model.Execution{
		ID: "execution-a", Evidence: model.ProviderEvidence{Provider: "hidden-provider", Version: 1, Payload: []byte("private")},
	}}, nil
}

func (p *commandProbe) Resume(_ context.Context, r app.ResumeRequest) (app.OperationResult, error) {
	p.resumeCalls++
	p.resume = r
	return app.OperationResult{Operation: model.Operation{ID: "operation-resume"}}, nil
}

func TestTransportStandaloneLaunchDoesNotInventAgent(t *testing.T) {
	p := &commandProbe{}
	w := request(testHandler(t, p), "POST", "/v2/launch",
		`{"request_id":"request-a","target":{"standalone":{"desired":{"Harness":"claude","WorkingDirectory":"/work"}}}}`, testCredential)
	if w.Code != http.StatusAccepted || p.launchCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, p.launchCalls, w.Body.String())
	}
	if p.launch.Target.Agent != nil || p.launch.Target.Standalone == nil || p.launch.Target.Standalone.Desired.Harness != "claude" {
		t.Fatalf("standalone target not preserved: %+v", p.launch.Target)
	}
	if p.launch.Principal.Kind != model.PrincipalOperator || p.launch.RequestID != "request-a" {
		t.Fatalf("caller/request mismatch: %+v", p.launch.RequestContext)
	}
}

func TestTransportResumeAcceptsSelectionNotProviderEvidence(t *testing.T) {
	p := &commandProbe{}
	h := testHandler(t, p)
	w := request(h, "POST", "/v2/resume", `{
		"request_id":"request-b","target":{"agent":{"agent_id":"agent-a","expected_revision":2}},
		"conversation_id":"conversation-a","expected_association_revision":3
	}`, testCredential)
	if w.Code != http.StatusAccepted || p.resumeCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, p.resumeCalls, w.Body.String())
	}
	if p.resume.ConversationID != "conversation-a" || p.resume.ExpectedAssociationRevision != 3 || p.resume.Target.Agent.ExpectedRevision != 2 {
		t.Fatalf("selection not preserved: %+v", p.resume)
	}
	for _, field := range []string{"prior_evidence", "native", "principal"} {
		w = request(h, "POST", "/v2/resume", `{"request_id":"request-c","`+field+`":{}}`, testCredential)
		if w.Code != http.StatusBadRequest || p.resumeCalls != 1 {
			t.Fatalf("untrusted %s accepted: status=%d calls=%d", field, w.Code, p.resumeCalls)
		}
	}
}
