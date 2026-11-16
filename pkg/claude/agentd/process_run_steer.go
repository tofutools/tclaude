package agentd

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// Steering a run beyond pause and resume: giving the whole run up, and
// parking a step that is out on a person.

type processRunCancelRequest struct {
	Note string `json:"note,omitempty"`
}

type processRunMarkBlockedRequest struct {
	NodeID  string `json:"nodeId"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
}

// handleProcessRunCancel gives a run up from any running state. It needs
// process.runs.manage: an overseer steers a run but does not end it (it
// raises a concern, and the operator's card offers the cancel).
func handleProcessRunCancel(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunCancelRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processRequestActor(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	setAuditDetail(r, "run: "+runID+", cancel")
	submitProcessRunSteer(w, runID, string(actor), processRunInput{cancel: &processRunCancelInput{note: request.Note}})
}

// handleProcessRunMarkBlocked parks an agent step that is out, with a reason,
// on the run's overseer (or anyone with process.runs.manage). Its performer is
// released, and the step waits on retry, skip or cancel.
func handleProcessRunMarkBlocked(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	caller, ok := requireProcessRunSteward(w, r, runID)
	if !ok {
		return
	}
	var request processRunMarkBlockedRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processRequestActor(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	setAuditDetail(r, "run: "+runID+", block node: "+stripControl(request.NodeID)+", attempt: "+strconv.Itoa(request.Attempt))
	submitProcessRunSteer(w, runID, string(actor), processRunInput{block: &executor.MarkBlockInput{
		NodeID: request.NodeID, Attempt: request.Attempt, Reason: strings.TrimSpace(request.Reason),
	}})
}

func submitProcessRunSteer(w http.ResponseWriter, runID, actor string, input processRunInput) {
	started, err := processRuns.submit(runID, actor, input)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	// A parked step's command left the outbox: release its performer now
	// rather than on the next sweep.
	kickProcessAgentDispatch(runID)
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}
