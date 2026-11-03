package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Several daemon-side launches (AWB pickups, the spawn queue, federation jobs
// and spawns, agent-bundle landings) go through handleGroupSpawn in-process,
// so they get exactly the checks an HTTP spawn gets. These helpers own the
// mechanics of that: building the synthetic POST and reading its recorded
// response. Each caller still chooses the identity it spawns as and decides
// what a refusal means for it.

// internalSpawnRequest describes one in-process group-spawn POST.
type internalSpawnRequest struct {
	// Ctx is the base context; nil means context.Background().
	Ctx context.Context
	// Path is the request path. It is visible to the handler (approval
	// prompts show it), so callers keep the one they always used.
	Path string
	// Group, when set, is bound as the {name} path value.
	Group string
	Body  []byte
	// Actor is the identity the spawn runs as; nil leaves it to the caller.
	Actor *peer
	// ReservedAgentID, when set, is the agent id the launch must use.
	ReservedAgentID string
}

func newInternalSpawnRequest(in internalSpawnRequest) *http.Request {
	req := httptest.NewRequest(http.MethodPost, in.Path, bytes.NewReader(in.Body))
	if in.Ctx != nil {
		req = req.WithContext(in.Ctx)
	}
	if in.Group != "" {
		req.SetPathValue("name", in.Group)
	}
	if in.ReservedAgentID != "" {
		req = req.WithContext(context.WithValue(req.Context(), reservedAgentIDContextKey{}, in.ReservedAgentID))
	}
	if in.Actor != nil {
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, in.Actor))
	}
	return req
}

// internalSpawnResult is handleGroupSpawn's recorded response.
type internalSpawnResult struct {
	Status int
	Header http.Header
	Body   []byte
}

// serveInternalGroupSpawn runs handleGroupSpawn in-process and records its
// response.
func serveInternalGroupSpawn(r *http.Request, g *db.AgentGroup) internalSpawnResult {
	rec := httptest.NewRecorder()
	handleGroupSpawn(rec, r, g)
	return recordedSpawnResult(rec)
}

func recordedSpawnResult(rec *httptest.ResponseRecorder) internalSpawnResult {
	return internalSpawnResult{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

// OK reports a 2xx response.
func (r internalSpawnResult) OK() bool { return r.Status >= 200 && r.Status <= 299 }

// Refusal decodes writeError's {error, code} body; a spawnFailure's Kind
// travels as code. A body of another shape yields empty strings.
func (r internalSpawnResult) Refusal() (code, msg string) {
	var refusal struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &refusal)
	return refusal.Code, refusal.Error
}

// Response decodes a successful spawn's agent.SpawnResponse.
func (r internalSpawnResult) Response() (agent.SpawnResponse, error) {
	var out agent.SpawnResponse
	err := json.Unmarshal(r.Body, &out)
	return out, err
}

// WriteTo relays the recorded response unchanged to the outer caller.
func (r internalSpawnResult) WriteTo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}
