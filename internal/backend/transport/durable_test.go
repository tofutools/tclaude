package transport

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTransportOfflineAgentSurvivesBackendRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := testHandler(t, app.New(store, providers.NewRegistry()))
	body := `{"id":"agent-a","name":"offline worker","desired":{"Harness":"claude","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"workspace_write"}}`
	w := request(h, "POST", "/v2/agents", body, testCredential)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h = testHandler(t, app.New(reopened, providers.NewRegistry()))
	w = request(h, "GET", "/v2/snapshot", "", testCredential)
	var snapshot struct {
		Agents     []model.Agent
		Executions []executionView
	}
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].ID != "agent-a" || len(snapshot.Executions) != 0 {
		t.Fatalf("offline catalog was not preserved: %+v", snapshot)
	}
	w = request(h, "PUT", "/v2/agents/agent-a", `{"expected_revision":99,"name":"stale edit","desired":{"Harness":"claude","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"workspace_write"}}`, testCredential)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale edit: %d %s", w.Code, w.Body)
	}
}
