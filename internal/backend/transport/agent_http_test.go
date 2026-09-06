package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type fixedCaller struct{ principal model.Principal }

func (f fixedCaller) Authenticate(*http.Request) (model.Principal, error) { return f.principal, nil }

type agentSurfaceProbe struct {
	app.AgentAPI
	caller model.Principal
}

func (p *agentSurfaceProbe) WhoAmI(_ context.Context, r app.WhoAmIRequest) (app.WhoAmIResult, error) {
	p.caller = r.Principal
	return app.WhoAmIResult{Principal: r.Principal, Execution: model.Execution{ID: r.Principal.ExecutionID, Evidence: model.ProviderEvidence{Provider: "private-native", Version: 1, Payload: []byte("secret-state")}}, ContextReadiness: model.ContextReadinessPending}, nil
}
func (p *agentSurfaceProbe) ReadInbox(_ context.Context, r app.ReadInboxRequest) (app.InboxResult, error) {
	p.caller = r.Principal
	return app.InboxResult{}, nil
}

type authoritySurfaceProbe struct {
	app.AuthorityAdminAPI
	grantCalls     int
	revokeRevision model.Revision
}

func (p *authoritySurfaceProbe) PutGrant(_ context.Context, r app.PutGrantRequest) (app.GrantResult, error) {
	p.grantCalls++
	return app.GrantResult{Grant: r.Grant}, nil
}
func (p *authoritySurfaceProbe) ExecutionAccessStatus(_ context.Context, r app.ExecutionAccessStatusRequest) (app.ExecutionAccessStatusResult, error) {
	return app.ExecutionAccessStatusResult{Access: model.ExecutionAccessBinding{ExecutionID: r.ExecutionID, Generation: 123, DeliveryID: "private-resource", Revision: 8, State: model.ExecutionAccessActive}}, nil
}
func (p *authoritySurfaceProbe) RevokeExecutionAccess(_ context.Context, r app.RevokeExecutionAccessRequest) (app.ExecutionAccessStatusResult, error) {
	p.revokeRevision = r.ExpectedRevision
	return app.ExecutionAccessStatusResult{Access: model.ExecutionAccessBinding{ExecutionID: r.ExecutionID, Revision: r.ExpectedRevision + 1, State: model.ExecutionAccessRevoked}}, nil
}

func TestAgentIdentityAndInboxUseTrustedExecutionWithoutEvidenceLeak(t *testing.T) {
	principal := model.ExecutionPrincipal("execution-a", "agent-a", 7)
	agents := &agentSurfaceProbe{}
	h, err := NewHandler(&applicationProbe{}, fixedCaller{principal})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterAgentAPI(agents, &authoritySurfaceProbe{}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/v2/identity", "", "")
	if w.Code != 200 || agents.caller != principal {
		t.Fatalf("identity: %d %+v", w.Code, agents.caller)
	}
	for _, private := range []string{"private-native", "secret-state", "Generation", "Authority"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("identity exposed internal authentication/evidence: %s", w.Body)
		}
	}
	w = request(h, "GET", "/v2/inbox?agent_id=someone-else", "", "")
	if w.Code != 400 {
		t.Fatalf("claimed inbox identity accepted: %d", w.Code)
	}
	w = request(h, "GET", "/v2/inbox?unread_only=true", "", "")
	if w.Code != 200 || agents.caller != principal {
		t.Fatalf("inbox: %d", w.Code)
	}
}

func TestAuthorityRoutesRejectMetadataAndExposeUsableAccessRevision(t *testing.T) {
	admin := &authoritySurfaceProbe{}
	h := testHandler(t, &applicationProbe{})
	if err := h.RegisterAgentAPI(&agentSurfaceProbe{}, admin); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"principal":{"Kind":"operator"}}`, `{"created_at":"2026-01-01"}`, `{"revision":100}`} {
		w := request(h, "PUT", "/v2/authority/grants/grant-a", body, testCredential)
		if w.Code != 400 || admin.grantCalls != 0 {
			t.Fatalf("server-owned grant metadata accepted: %d", w.Code)
		}
	}
	w := request(h, "GET", "/v2/executions/execution-a/access", "", testCredential)
	var view accessView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != 8 || strings.Contains(w.Body.String(), "private-resource") || strings.Contains(w.Body.String(), "Generation") {
		t.Fatalf("access projection: %s", w.Body)
	}
	body, _ := json.Marshal(map[string]model.Revision{"expected_revision": view.Revision})
	r := httptest.NewRequest("POST", "/v2/executions/execution-a/access/revoke", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+testCredential)
	result := httptest.NewRecorder()
	h.ServeHTTP(result, r)
	if result.Code != 200 || admin.revokeRevision != view.Revision {
		t.Fatalf("revoke could not consume public revision: %d", result.Code)
	}
}
