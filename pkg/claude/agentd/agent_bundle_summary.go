package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// agent_bundle_summary.go asks a live agent for a summary of its earlier
// work, for an export or snapshot to carry as context/handoff.md. A later
// landing on a smaller context window keeps the pinned start, the summary
// and the newest turns. It is opt-in (--with-summary): asking costs the
// agent a turn, so it never happens behind the operator's back.
//
// The agent answers with `tclaude agent morph-handoff`, the command a
// morph's source writes its summary with. A request lives in memory only,
// for exportSummaryTTL; a restart forgets it and the export is retried.

const exportSummaryTTL = 15 * time.Minute

type exportSummary struct {
	ID        string
	AgentID   string
	Conv      string
	Requester string
	ExpiresAt time.Time
	Body      string
}

var exportSummaries = struct {
	sync.Mutex
	byID map[string]*exportSummary
}{byID: map[string]*exportSummary{}}

func pruneExportSummariesLocked(now time.Time) {
	for id, s := range exportSummaries.byID {
		if now.After(s.ExpiresAt) {
			delete(exportSummaries.byID, id)
		}
	}
}

// handleExportSummaryRequest asks an agent for a summary of its earlier
// work. It needs what an export of that agent needs.
//
//	POST /v1/agent-bundle/summary {agent} → {id, agent, expires_at}
func handleExportSummaryRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, 400, "body", err.Error())
		return
	}
	res, _, err := agent.ResolveSelector(req.Agent)
	if err != nil {
		writeError(w, 404, "agent", err.Error())
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentBundleExport, res.ConvID)
	if !ok {
		return
	}
	if caller != "" && sameActor(caller, res.ConvID) {
		writeError(w, 400, "summary", "an agent cannot wait on its own summary: write it first, or have the operator export with --with-summary")
		return
	}
	agentID, err := db.AgentIDForConv(res.ConvID)
	if err != nil || agentID == "" {
		writeError(w, 409, "summary", "only an agent can be asked for a summary")
		return
	}
	now := time.Now().UTC()
	s := &exportSummary{ID: "sum_" + proto.NewEnvelopeID(), AgentID: agentID, Conv: res.ConvID, Requester: caller, ExpiresAt: now.Add(exportSummaryTTL)}
	exportSummaries.Lock()
	pruneExportSummariesLocked(now)
	for id, old := range exportSummaries.byID {
		if old.AgentID == agentID {
			delete(exportSummaries.byID, id)
		}
	}
	exportSummaries.byID[s.ID] = s
	exportSummaries.Unlock()
	body := fmt.Sprintf("An export of you is being prepared that should carry a summary of your earlier work, so that "+
		"if you are later landed on a model with a smaller context window, what is cut is not simply lost. Write a "+
		"concise summary for yourself: the goal, what is done, what is in progress, the decisions made and why, open "+
		"questions, and the next concrete steps, with the file paths involved. Do not include secrets.\n\n"+
		"Submit it with `tclaude agent morph-handoff -f <file>` (or pass the text as the argument), then end your "+
		"turn. Deadline: %s. Without it the export is not made.", s.ExpiresAt.Format(time.RFC3339))
	var groupID int64
	if groups, err := db.ListGroupsForConv(res.ConvID); err == nil && len(groups) > 0 {
		groupID = groups[0].ID
	}
	if _, err := queueAgentMessage(&db.AgentMessage{GroupID: groupID, ToConv: res.ConvID, Subject: "Summary requested for an export", Body: body}); err != nil {
		slog.Warn("export summary: request", "conv", short8(res.ConvID), "error", err)
		writeError(w, 500, "summary", "could not ask the agent: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": s.ID, "agent": agentID, "expires_at": s.ExpiresAt})
}

// handleExportSummaryStatus reports whether a requested summary arrived.
//
//	GET /v1/agent-bundle/summary/{id} → {id, state: waiting|ready}
func handleExportSummaryStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller, _, ok := authedCaller(w, r)
	if !ok {
		return
	}
	exportSummaries.Lock()
	pruneExportSummariesLocked(time.Now())
	s := exportSummaries.byID[id]
	state := ""
	if s != nil && s.Requester == caller {
		state = "waiting"
		if s.Body != "" {
			state = "ready"
		}
	}
	exportSummaries.Unlock()
	if state == "" {
		writeError(w, 404, "summary", "no such summary request, or it expired")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "state": state})
}

// submitExportSummary records an agent's summary for its pending export
// request, if it has one.
func submitExportSummary(agentID, body string) bool {
	exportSummaries.Lock()
	defer exportSummaries.Unlock()
	pruneExportSummariesLocked(time.Now())
	for _, s := range exportSummaries.byID {
		if s.AgentID == agentID {
			s.Body = body
			return true
		}
	}
	return false
}

// takeExportSummary hands an export the summary it asked for. The request
// must be the caller's own and for the agent being exported. It stays until
// the export succeeds and calls dropExportSummary, so a refused export (for
// example over suspected credentials) can be retried without asking again.
func takeExportSummary(id, caller, conv string) (string, error) {
	exportSummaries.Lock()
	defer exportSummaries.Unlock()
	pruneExportSummariesLocked(time.Now())
	s := exportSummaries.byID[id]
	if s == nil || s.Requester != caller || !sameActor(s.Conv, conv) {
		return "", fmt.Errorf("summary %s is not a pending summary of this agent; ask again with --with-summary", id)
	}
	if strings.TrimSpace(s.Body) == "" {
		return "", fmt.Errorf("the agent has not written its summary yet")
	}
	return s.Body, nil
}

// dropExportSummary forgets a summary its export has carried.
func dropExportSummary(id string) {
	exportSummaries.Lock()
	delete(exportSummaries.byID, id)
	exportSummaries.Unlock()
}
