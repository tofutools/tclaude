package agentd

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEpochV8SettlementApprovalSnapshotNeverReadsSensitiveBody(t *testing.T) {
	const sentinel = "sentinel-private-settlement-reason"
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/process/runs/run/unblock", strings.NewReader(`{"baseBinding":{"revision":1,"digest":"x"},"token":"t","decision":"retry","reason":"`+sentinel+`","evidenceRef":"e"}`))
	if err != nil {
		t.Fatal(err)
	}
	preview := snapshotApprovalRequestBody(req, PermProcessAdvance)
	if strings.Contains(preview, sentinel) {
		t.Fatal("settlement secret entered approval preview")
	}
	remaining, err := io.ReadAll(req.Body)
	if err != nil || !strings.Contains(string(remaining), sentinel) {
		t.Fatalf("handler stream changed: %q, %v", remaining, err)
	}
}

func TestEpochV8SensitiveHTTPPathsAreTemplated(t *testing.T) {
	for _, path := range []string{
		"/v1/process/runs/private-run/unlock/preview",
		"/v1/process/runs/private/run/unlock/preview",
		"/v1/process/runs/private/run/unlock/preview/trailing-secret",
		"/v1/process/runs/private-run/unblock",
		"/v1/process/runs/private/run/unblock",
		"/v1/process/runs/private/run/unblock/trailing-secret",
		"/v1/process/runs/private-run/epochs/private-epoch/reason",
		"/v1/process/runs/private/run/epochs/private-epoch/reason",
		"/v1/process/runs/private/run/epochs/private/epoch/reason",
	} {
		got := safeHTTPLogPath(path)
		if strings.Contains(got, "private-run") || strings.Contains(got, "private-epoch") {
			t.Fatalf("path leaked: %q", got)
		}
	}
	for _, path := range []string{
		"/v1/process/runs/{id}/unblock",
		"/v1/process/runs/{id}/epochs/{epoch}/{artifact}",
	} {
		got, sensitive := projectSafeHTTPLogPath(path)
		if !sensitive || got != path {
			t.Fatalf("literal template classification = %q, %v", got, sensitive)
		}
	}
}

func TestEpochV8SettlementRejectsClientActorAndTimestamp(t *testing.T) {
	for _, field := range []string{`"actor":"human:forged"`, `"timestamp":"2026-01-01T00:00:00Z"`} {
		body := `{"baseBinding":{"revision":1,"digest":"` + strings.Repeat("a", 64) + `"},"token":"` + strings.Repeat("b", 64) + `","decision":"retry","reason":"r","evidenceRef":"e",` + field + `}`
		var decoded epochV8SettlementRequest
		if err := decodeOneStrictJSON(strings.NewReader(body), &decoded); err == nil {
			t.Fatalf("accepted client authority field %s", field)
		}
	}
}
