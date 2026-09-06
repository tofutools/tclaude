package transport

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type observationProbe struct{ app.API }

func (observationProbe) Observe(_ context.Context, req app.ObserveRequest) (app.ObservationResult, error) {
	if req.Principal.Kind != model.PrincipalOperator || req.ExecutionID != "execution-a" {
		return app.ObservationResult{}, app.ErrUnauthorized
	}
	private := model.ProviderEvidence{Provider: "vendor", Version: 1, Payload: []byte("private-secret")}
	return app.ObservationResult{Execution: model.Execution{ID: req.ExecutionID, Evidence: private}, Observation: ports.Observation{Evidence: private, NativeConversation: &model.NativeConversationEvidence{Namespace: "private-store", Reference: "private-native-id"}}}, nil
}
func TestTransportObservationKeepsNativeEvidencePrivate(t *testing.T) {
	w := request(testHandler(t, observationProbe{}), "POST", "/v2/observe", `{"execution_id":"execution-a"}`, testCredential)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	for _, secret := range []string{"private", "Evidence", "NativeConversation"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("private observation leaked: %s", w.Body)
		}
	}
}
func TestTransportPreservesApplicationErrorMeaning(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{app.ErrNotFound, 404, "not_found"}, {app.ErrUnauthorized, 403, "forbidden"},
		{app.ErrConflict, 409, "conflict"}, {app.ErrUncertain, 409, "uncertain"},
		{app.ErrUnsupported, 422, "unsupported"}, {app.ErrUnavailable, 503, "unavailable"},
	} {
		w := request(testHandler(t, &applicationProbe{err: test.err}), "GET", "/v2/snapshot", "", testCredential)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) {
			t.Fatalf("%v: %d %s", test.err, w.Code, w.Body)
		}
	}
}

func TestTransportOperationDiagnosticsStayPrivate(t *testing.T) {
	p := &applicationProbe{snapshot: app.Snapshot{Operations: []model.Operation{{ID: "op-a", ResultCode: "release_uncertain", Detail: "private credential in native error"}}}}
	w := request(testHandler(t, p), "GET", "/v2/snapshot", "", testCredential)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private credential") || !strings.Contains(w.Body.String(), "release_uncertain") {
		t.Fatalf("public operation: %d %s", w.Code, w.Body)
	}
}
