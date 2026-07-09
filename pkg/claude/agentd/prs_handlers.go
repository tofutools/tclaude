package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Explicit PR presentation endpoints:
//
//	GET/POST /v1/whoami/prs          → read/present/handle caller PRs (self.pr)
//	GET/POST /v1/agent/{conv}/prs    → read/present/handle another agent's PRs (agent.pr / owner)
func handleWhoamiPRs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfPR)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own PRs; use /v1/agent/{conv}/prs to act on another agent")
		return
	}
	if r.Method == http.MethodGet {
		writePRsResponse(w, convID, convID)
		return
	}
	runPRUpdate(w, r, convID, convID)
}

func handleAgentPRs(w http.ResponseWriter, r *http.Request, targetConv string) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentPR, targetConv)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		writePRsResponse(w, targetConv, caller)
		return
	}
	runPRUpdate(w, r, targetConv, caller)
}

func runPRUpdate(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		URL     string `json:"url"`
		Summary string `json:"summary"`
		State   string `json:"state"`
		Handled bool   `json:"handled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Summary = strings.TrimSpace(body.Summary)
	if err := validateAgentPRURL(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pr_url", err.Error())
		return
	}
	if err := validateAgentPRSummary(body.Summary); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pr_summary", err.Error())
		return
	}
	state, err := normalizeAgentPRState(body.State)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pr_state", err.Error())
		return
	}

	agentID, err := db.AgentIDForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(target))
		return
	}
	if body.Handled || state == "handled" {
		if _, err := db.MarkAgentPRHandled(agentID, body.URL); err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		writePRUpdateResponse(w, target, caller, presentedPRView{URL: body.URL, Number: deriveGitHubPRNumber(body.URL), State: "handled"}, true)
		return
	}
	row, err := db.UpsertAgentPR(agentID, body.URL, body.Summary, state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	views := presentedPRViews([]db.AgentPR{row})
	var view presentedPRView
	if len(views) > 0 {
		view = views[0]
	}
	writePRUpdateResponse(w, target, caller, view, false)
}

func writePRsResponse(w http.ResponseWriter, convID, caller string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(convID))
		return
	}
	all, err := db.ListUnhandledAgentPRs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id": convID,
		"prs":     presentedPRViews(all[agentID]),
	})
}

func writePRUpdateResponse(w http.ResponseWriter, target, caller string, view presentedPRView, handled bool) {
	resp := map[string]any{
		"conv_id": target,
		"pr":      view,
		"handled": handled,
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}
