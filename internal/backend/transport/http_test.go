package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const testCredential = "0123456789abcdef0123456789abcdef"

type applicationProbe struct {
	app.API
	createCalls int
	caller      model.Principal
	snapshot    app.Snapshot
	err         error
}

func (p *applicationProbe) CreateAgent(_ context.Context, r app.CreateAgentRequest) (app.AgentResult, error) {
	p.createCalls++
	p.caller = r.Context
	return app.AgentResult{Agent: model.Agent{ID: r.ID, Name: r.Name}}, p.err
}

func (p *applicationProbe) Snapshot(_ context.Context, r app.SnapshotRequest) (app.Snapshot, error) {
	p.caller = r.Principal
	return p.snapshot, p.err
}

func testHandler(t *testing.T, api app.API) *Handler {
	t.Helper()
	auth, err := NewOperatorToken(testCredential)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(api, auth)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, path, body, credential string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTransportAuthenticatesBeforeApplicationAdmission(t *testing.T) {
	p := &applicationProbe{}
	h := testHandler(t, p)
	for _, secret := range []string{"", "incorrect"} {
		w := request(h, "POST", "/v2/agents", `{"name":"reviewer"}`, secret)
		if w.Code != http.StatusUnauthorized || p.createCalls != 0 {
			t.Fatalf("status=%d application calls=%d", w.Code, p.createCalls)
		}
	}
	w := request(h, "POST", "/v2/agents", `{"name":"reviewer"}`, testCredential)
	if w.Code != http.StatusCreated || p.createCalls != 1 || p.caller.Kind != model.PrincipalOperator {
		t.Fatalf("status=%d calls=%d principal=%+v", w.Code, p.createCalls, p.caller)
	}
}

func TestTransportRejectsClaimedPrincipalAndAmbiguousBodies(t *testing.T) {
	p := &applicationProbe{}
	h := testHandler(t, p)
	for _, body := range []string{
		`{"name":"reviewer","Context":{"Kind":"operator"}}`,
		`{"name":"reviewer","principal":{"Kind":"operator"}}`,
		`{"name":"reviewer"} {"name":"other"}`,
		`{"name":"` + strings.Repeat("x", maxRequestBytes) + `"}`,
	} {
		w := request(h, "POST", "/v2/agents", body, testCredential)
		if w.Code != http.StatusBadRequest || p.createCalls != 0 {
			t.Fatalf("status=%d application calls=%d", w.Code, p.createCalls)
		}
	}
}

func TestTransportDoesNotSerializeProviderRecoveryEvidence(t *testing.T) {
	p := &applicationProbe{snapshot: app.Snapshot{Executions: []model.Execution{{
		ID: "execution-a", Evidence: model.ProviderEvidence{
			Provider: "private-provider", Version: 1, Payload: []byte("private endpoint and credential reference"),
		},
	}}}}
	w := request(testHandler(t, p), "GET", "/v2/snapshot", "", testCredential)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var result struct {
		Executions []map[string]json.RawMessage `json:"executions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Executions) != 1 || string(result.Executions[0]["id"]) != `"execution-a"` {
		t.Fatalf("unexpected execution projection: %s", w.Body.String())
	}
	for key := range result.Executions[0] {
		if strings.EqualFold(key, "evidence") {
			t.Fatal("provider recovery evidence exposed through query response")
		}
	}
	if strings.Contains(w.Body.String(), "private-provider") {
		t.Fatal("provider evidence exposed")
	}
}

func TestTransportDoesNotExposeInternalErrorText(t *testing.T) {
	p := &applicationProbe{err: errors.New("native endpoint with secret")}
	w := request(testHandler(t, p), "POST", "/v2/agents", `{"name":"reviewer"}`, testCredential)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unsafe error response: %d %s", w.Code, w.Body.String())
	}
}

func TestOperatorCredentialRejectsMultipleAuthorizationHeaders(t *testing.T) {
	a, err := NewOperatorToken(testCredential)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v2/snapshot", nil)
	r.Header.Add("Authorization", "Bearer "+testCredential)
	r.Header.Add("Authorization", "Bearer other")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("ambiguous authentication accepted: %v", err)
	}
}
