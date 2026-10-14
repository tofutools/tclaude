package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// processResolutionReassigned marks a dispatch a person or manager handed to a
// specific performer at runtime.
const processResolutionReassigned = "reassigned"

// processAssignRequest is the body of
// POST /v1/process/runs/{id}/nodes/{node}/assign.
type processAssignRequest struct {
	// Agent names the new performer: a stable agent id, a conv id, or a name.
	Agent string `json:"agent"`
}

// processAssignError is a refusal the handler maps to an HTTP answer.
type processAssignError struct {
	status  int
	code    string
	message string
}

func (e *processAssignError) Error() string { return e.message }

// handleProcessRunAssign hands a node's outstanding agent attempt to a named
// performer. It is gated like decide, and it goes through the same dispatch
// path as resolution: the new performer gets the node prompt in its inbox, the
// previous one (if any) is told to stop, and only the new one may report.
//
// It applies only to an agent attempt that is outstanding and not yet
// reported: a node that is not ready yet, already settled, or not agentic has
// nothing to hand over.
func handleProcessRunAssign(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processAssignRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_assign_request", err.Error())
		return
	}
	request.Agent = strings.TrimSpace(request.Agent)
	if request.Agent == "" {
		writeError(w, http.StatusUnprocessableEntity, "process_assign_request", "agent is required")
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID, nodeID := r.PathValue("id"), r.PathValue("node")
	setAuditDetail(r, "run: "+stripControl(runID)+", node: "+stripControl(nodeID)+
		", agent: "+stripControl(request.Agent))

	row, err := assignProcessNodePerformer(runID, nodeID, request.Agent, string(actor))
	if err != nil {
		var refused *processAssignError
		if errors.As(err, &refused) {
			writeError(w, refused.status, refused.code, refused.message)
			return
		}
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{
		"runId": runID, "nodeId": nodeID, "attempt": row.Attempt, "agentId": row.AgentID, "run": view,
	})
}

// assignProcessNodePerformer moves one node's outstanding agent attempt to the
// named agent. It holds processDispatchMu throughout, so no resolution or
// sweep pass can deliver the attempt between the release and the new
// delivery.
func assignProcessNodePerformer(runID, nodeID, selector, actor string) (*db.ProcessAgentDispatch, error) {
	processDispatchMu.Lock()
	defer processDispatchMu.Unlock()

	record, err := db.GetProcessRun(runID)
	if err != nil {
		return nil, err
	}
	var checkpoint engine.Checkpoint
	if err := json.Unmarshal(record.CheckpointJSON, &checkpoint); err != nil {
		return nil, fmt.Errorf("decode checkpoint: %w", err)
	}
	var command *engine.Command
	for i := range checkpoint.Commands {
		if c := checkpoint.Commands[i]; c.NodeID == nodeID && c.Kind == engine.CommandAgent && c.Agent != nil {
			command = &checkpoint.Commands[i]
		}
	}
	if command == nil || checkpoint.Status != engine.RunRunning || engine.Draining(checkpoint) {
		return nil, &processAssignError{http.StatusConflict, "process_assign_not_outstanding", fmt.Sprintf(
			"node %q has no outstanding agent attempt to assign: it is not an agent node, not ready yet, or already settled", nodeID)}
	}
	row, err := db.EnsureProcessAgentDispatch(runID, command.ID, command.NodeID, command.Attempt)
	if err != nil {
		return nil, err
	}
	if row.State == db.ProcessDispatchReported || row.State == db.ProcessDispatchClosed {
		return nil, &processAssignError{http.StatusConflict, "process_assign_not_outstanding",
			"this attempt was already reported; it can no longer be reassigned"}
	}

	scope, err := processRunScopeOf(runID)
	if err != nil {
		return nil, err
	}
	candidate, reason := processAssignCandidate(scope, actor, selector)
	if candidate == nil {
		return nil, &processAssignError{http.StatusUnprocessableEntity, "process_assign_performer", reason}
	}
	if row.State == db.ProcessDispatchDelivered && row.AgentID == candidate.AgentID {
		return row, nil
	}
	if row.State == db.ProcessDispatchDelivered {
		// A spawned performer that has not launched yet cannot be told to stop:
		// it would start with the node prompt and do the work for nothing.
		if pending, err := db.GetPendingSpawnByAgentID(row.AgentID); err != nil {
			return nil, err
		} else if pending != nil {
			return nil, &processAssignError{http.StatusConflict, "process_assign_performer_starting",
				"the current performer is still being spawned; reassign once it has started"}
		}
	}
	conflict := &processAssignError{http.StatusConflict, "process_assign_not_outstanding",
		"the attempt changed while reassigning (its performer may have just reported); check the run and retry"}

	fromState := row.State
	if row.State == db.ProcessDispatchDelivered {
		if err := db.ReleaseProcessAgentDispatch(runID, row.CommandID, row.AgentID,
			"reassigned to "+candidate.AgentID+" by "+actor); err != nil {
			if errors.Is(err, db.ErrProcessDispatchConflict) {
				return nil, conflict
			}
			return nil, err
		}
		notifyProcessPerformerReleased(*row, "This process node attempt was reassigned to another performer. Stop working on it; your report is no longer needed.")
		fromState = db.ProcessDispatchPending
	}
	messageID, err := queueAgentMessage(&db.AgentMessage{
		GroupID: scope.GroupID, ToConv: candidate.ConvID,
		Subject: processDispatchSubject(runID, *command),
		Body:    processDispatchMessage(runID, *command, processAssignFeedback(*row, *command)),
	})
	if err != nil {
		// The released attempt is pending again, so the next pass resolves it
		// the ordinary way rather than leaving it with nobody.
		return nil, err
	}
	if err := db.MarkProcessAgentDispatchDelivered(runID, row.CommandID, fromState,
		candidate.AgentID, processResolutionReassigned, messageID); err != nil {
		if errors.Is(err, db.ErrProcessDispatchConflict) {
			return nil, conflict
		}
		return nil, err
	}
	return db.GetProcessAgentDispatch(runID, row.CommandID)
}

// processAssignCandidate resolves and authorizes a named performer within the
// run's scope, using the same reach rules as resolution: an agent-scoped run
// keeps its bound agent, a group run takes only active members, and an
// unscoped run takes an agent its owner — and an agent caller — could task.
func processAssignCandidate(scope processRunScope, actor, selector string) (*processPerformerCandidate, string) {
	if scope.Orphaned {
		return nil, "the run is orphaned: its scope target is gone"
	}
	switch scope.Kind {
	case processScopeAgent:
		candidate, reason := processBoundCandidate(scope.AgentID)
		if candidate == nil {
			return nil, reason
		}
		if !processCandidateMatches(*candidate, selector) {
			return nil, "an agent-scoped run is performed only by the agent it is bound to"
		}
		return candidate, ""
	case processScopeGroup:
		group, err := db.GetAgentGroupByID(scope.GroupID)
		if err != nil || group == nil || group.IsArchived() {
			return nil, "the run's group is unavailable"
		}
		for _, member := range processGroupCandidates(scope.GroupID) {
			if processCandidateMatches(member, selector) {
				return &member, ""
			}
		}
		return nil, fmt.Sprintf("%q is not an active member of the run's group", selector)
	default:
		candidate := processAgentBySelector(selector)
		if candidate == nil {
			return nil, fmt.Sprintf("%q is not an active agent", selector)
		}
		if !processOwnerMayTask(scope.Owner, *candidate) {
			return nil, fmt.Sprintf("%q shares no group with the run owner", selector)
		}
		if strings.HasPrefix(actor, "agent:") && !processOwnerMayTask(actor, *candidate) {
			return nil, fmt.Sprintf("%q shares no group with you", selector)
		}
		return candidate, ""
	}
}

// processAssignFeedback carries a feedback-same-session retry's previous
// failure to whoever takes the attempt, as ordinary resolution would.
func processAssignFeedback(row db.ProcessAgentDispatch, command engine.Command) string {
	if command.Agent.RetryMode != model.RetryModeFeedbackSameSession {
		return ""
	}
	previous := previousProcessDispatch(row)
	if previous == nil {
		return ""
	}
	if previous.ReportNote == "" {
		return "the previous attempt reported failure without a note"
	}
	return previous.ReportNote
}
