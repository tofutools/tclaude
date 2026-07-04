package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskref_handlers.go serves the per-agent task-reference link endpoints:
//
//	GET/POST /v1/whoami/task          → read/set the CALLER's own link (self.task)
//	GET/POST /v1/agent/{conv}/task    → read/set ANOTHER agent's link (agent.task / owner)
//
// A POST with a non-empty url sets the link (validated http(s)); a POST
// with clear=true or an empty url clears it. Unlike rename/compact this
// is a pure DB write — no tmux send-keys — so there is no injection sink
// and no charset gate beyond the URL-scheme check.
//
// Task links are dashboard-only (like branchlinks.go's repo links, they
// never ride the agent-facing /v1/peers surface), so even the cross-agent
// READ stays behind the same manager gate as the write — an agent can't
// enumerate peers' task links, only a manager (slug or group owner) can.

// handleWhoamiTask reads (GET) or sets/clears (POST) the calling agent's
// own task-reference link. Permission-gated on self.task.
func handleWhoamiTask(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfTask)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own task link; use /v1/agent/{conv}/task to act on another agent")
		return
	}
	if r.Method == http.MethodGet {
		writeTaskRefResponse(w, convID, convID)
		return
	}
	runTaskRefUpdate(w, r, convID, convID)
}

// handleAgentTask reads (GET) or sets/clears (POST) ANOTHER agent's
// task-reference link. Routed via handleAgentByConv. Auth: agent.task
// slug OR caller owns a group containing the target.
func handleAgentTask(w http.ResponseWriter, r *http.Request, targetConv string) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentTask, targetConv)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeTaskRefResponse(w, targetConv, caller)
		return
	}
	runTaskRefUpdate(w, r, targetConv, caller)
}

// runTaskRefUpdate decodes the request body, validates the URL, writes
// the task ref onto the target agent, and responds. A clear=true or an
// empty url clears the link (and any explicit label with it).
func runTaskRefUpdate(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		URL   string `json:"url"`
		Label string `json:"label"`
		Clear bool   `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Trim up front so validation, the stored value (db.SetAgentTaskRef
	// trims too), and the echoed response all agree — a raw API caller
	// that pads the URL used to get a padded value back that then
	// disagreed with the next snapshot.
	body.URL = strings.TrimSpace(body.URL)
	body.Label = strings.TrimSpace(body.Label)

	if body.Clear || body.URL == "" {
		if err := setTaskRefForConv(w, target, "", ""); err != nil {
			return
		}
		writeTaskRefUpdateResponse(w, target, caller, taskRefView{}, true)
		return
	}
	if err := validateTaskRefURL(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_url", err.Error())
		return
	}
	if err := validateTaskRefLabel(body.Label); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_label", err.Error())
		return
	}
	if err := setTaskRefForConv(w, target, body.URL, body.Label); err != nil {
		return
	}
	view := taskRefViewFor(db.AgentTaskRef{URL: body.URL, Label: body.Label})
	writeTaskRefUpdateResponse(w, target, caller, view, false)
}

// setTaskRefForConv resolves a conv to its stable agent_id and writes the
// task ref. On any failure it writes the HTTP error itself and returns a
// non-nil error so the caller just returns.
func setTaskRefForConv(w http.ResponseWriter, convID, url, label string) error {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return err
	}
	if agentID == "" {
		err := fmt.Errorf("no agent enrolled for conv %s", short8(convID))
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return err
	}
	if _, err := db.SetAgentTaskRef(agentID, url, label); err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return err
	}
	return nil
}

// writeTaskRefResponse loads and returns a conv's current task ref.
func writeTaskRefResponse(w http.ResponseWriter, convID, caller string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(convID))
		return
	}
	ref, err := db.GetAgentTaskRef(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeTaskRefUpdateResponse(w, convID, caller, taskRefViewFor(ref), false)
}

// writeTaskRefUpdateResponse writes the JSON body shared by the read and
// write paths: the target conv, the effective url/label, and (when a
// manager acted on someone else) the caller's identity for the audit
// trail. cleared marks a clear operation so the CLI can word its output.
func writeTaskRefUpdateResponse(w http.ResponseWriter, target, caller string, view taskRefView, cleared bool) {
	resp := map[string]any{
		"conv_id":        target,
		"task_ref_url":   view.TaskURL,
		"task_ref_label": view.TaskLabel,
		"cleared":        cleared,
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}
