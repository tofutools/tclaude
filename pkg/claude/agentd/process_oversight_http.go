package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The oversight endpoints:
//
//	POST /v1/process/runs/{id}/concerns                     raise a concern (anyone in the run)
//	POST /v1/process/runs/{id}/concerns/{concern}/answer    continue | pause | escalate (its holder)
//	POST /v1/process/runs/{id}/overseer                     set or clear the overseer (process.runs.manage)
//
// The run view (GET /v1/process/runs/{id}) carries the overseer and the
// concerns; the run's events carry the log.

type processConcernRaiseRequest struct {
	Reason string `json:"reason"`
	NodeID string `json:"nodeId,omitempty"`
}

type processConcernAnswerRequest struct {
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

type processOverseerRequest struct {
	// Overseer is an agent selector (name, agent id or conv id), "operator",
	// or "" to clear it back to the default.
	Overseer string `json:"overseer"`
}

// Holder answers.
const (
	processConcernContinue = "continue"
	processConcernPause    = "pause"
	processConcernEscalate = "escalate"
)

// processRunOverseerView is who oversees a run and why.
type processRunOverseerView struct {
	Ref     string `json:"ref"`
	AgentID string `json:"agentId,omitempty"`
	Name    string `json:"name"`
	Via     string `json:"via"`
	// Escalation is the group's escalation agent, the chain's second level.
	EscalationAgentID string `json:"escalationAgentId,omitempty"`
	EscalationName    string `json:"escalationName,omitempty"`
}

// processRunConcernView is one concern as the run view shows it.
type processRunConcernView struct {
	ID         int64      `json:"id"`
	NodeID     string     `json:"nodeId,omitempty"`
	Source     string     `json:"source"`
	Signal     string     `json:"signal,omitempty"`
	RaisedBy   string     `json:"raisedBy"`
	RaisedName string     `json:"raisedName"`
	Reason     string     `json:"reason"`
	State      string     `json:"state"`
	Level      string     `json:"level"`
	Holder     string     `json:"holder,omitempty"`
	HolderName string     `json:"holderName"`
	AnswerBy   *time.Time `json:"answerBy,omitempty"`
	MessageID  int64      `json:"messageId,omitempty"`
	// Choices are the operator card's options while it waits on the operator.
	Choices    []string   `json:"choices,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	ResolvedBy string     `json:"resolvedBy,omitempty"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

func processRunOverseerViewOf(runID string) *processRunOverseerView {
	o, err := resolveProcessRunOverseer(runID)
	if err != nil {
		return nil
	}
	view := &processRunOverseerView{Ref: o.Ref, AgentID: o.AgentID, Name: processActorName(o.Ref), Via: o.Via}
	if id := processEscalationAgentOf(runID); id != "" && id != o.AgentID {
		view.EscalationAgentID, view.EscalationName = id, processActorName(processAgentRef(id))
	}
	return view
}

func processRunConcernViews(runID string) []processRunConcernView {
	concerns, err := db.ListProcessRunConcerns(runID)
	if err != nil {
		return nil
	}
	views := make([]processRunConcernView, 0, len(concerns))
	for _, c := range concerns {
		v := processRunConcernView{
			ID: c.ID, NodeID: c.NodeID, Source: c.Source, Signal: c.Signal,
			RaisedBy: c.RaisedBy, RaisedName: processActorName(c.RaisedBy), Reason: c.Reason,
			State: c.State, Level: c.Level, Holder: c.Holder, MessageID: c.HumanMessageID,
			Resolution: c.Resolution, ResolvedBy: c.ResolvedBy, Note: c.Note, CreatedAt: c.CreatedAt,
		}
		switch {
		case c.Open() && c.Level == db.ProcessConcernLevelOperator:
			v.HolderName = "the operator"
			if m, err := db.GetHumanMessage(c.HumanMessageID); err == nil && m != nil && m.IsOpenDecision() {
				v.Choices = m.Options
			}
		case c.Holder != "":
			v.HolderName = processActorName(c.Holder)
		}
		if !c.AnswerBy.IsZero() {
			at := c.AnswerBy
			v.AnswerBy = &at
		}
		if !c.ResolvedAt.IsZero() {
			at := c.ResolvedAt
			v.ResolvedAt = &at
		}
		views = append(views, v)
	}
	return views
}

// processCallerAgentID is the stable agent id of an agent caller, "" for the
// operator.
func processCallerAgentID(r *http.Request) string {
	if conv := confirmedAgentConv(r); conv != "" {
		return peerAgentID(conv)
	}
	return ""
}

// processRunIsOverseer reports whether agentID oversees the run.
func processRunIsOverseer(runID, agentID string) bool {
	if agentID == "" {
		return false
	}
	o, err := resolveProcessRunOverseer(runID)
	return err == nil && o.AgentID == agentID
}

// processRunParticipant reports whether agentID takes part in the run: it
// oversees it or is its escalation agent, started it, is bound to it, is a
// member of its group, or was handed one of its steps.
func processRunParticipant(runID, agentID string) bool {
	if agentID == "" {
		return false
	}
	if processRunIsOverseer(runID, agentID) || processEscalationAgentOf(runID) == agentID {
		return true
	}
	scope, err := db.GetProcessRunScope(runID)
	if err != nil {
		return false
	}
	if scope.Owner == processAgentRef(agentID) || (scope.Kind == db.ProcessRunScopeAgent && scope.AgentID == agentID) {
		return true
	}
	if scope.Kind == db.ProcessRunScopeGroup {
		if members, err := db.ListAgentGroupMembers(scope.GroupID); err == nil {
			for _, m := range members {
				if peerAgentID(m.ConvID) == agentID {
					return true
				}
			}
		}
	}
	rows, err := db.ListProcessAgentDispatchesForRun(runID)
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.AgentID == agentID {
			return true
		}
	}
	return false
}

// processRunHoldsConcern reports whether agentID holds an open concern of the
// run.
func processRunHoldsConcern(runID, agentID string) bool {
	if agentID == "" {
		return false
	}
	concerns, err := db.ListProcessRunConcerns(runID)
	if err != nil {
		return false
	}
	for _, c := range concerns {
		if c.Open() && c.Holder == processAgentRef(agentID) {
			return true
		}
	}
	return false
}

// requireProcessRunSteward gates what a run's overseer may do to the run
// (pause, resume, reassign a step): its overseer passes, anyone else needs
// process.runs.manage.
func requireProcessRunSteward(w http.ResponseWriter, r *http.Request, runID string) (string, bool) {
	if conv := confirmedAgentConv(r); conv != "" && peerActionFromRequest(r) == nil &&
		processRunIsOverseer(strings.TrimSpace(runID), peerAgentID(conv)) {
		return conv, true
	}
	return requirePermission(w, r, PermProcessRunsManage)
}

// processOversightActorOf is the evidence actor of the caller.
func processOversightActorOf(r *http.Request, caller string) (string, error) {
	actor, err := processRequestActor(r, caller)
	return string(actor), err
}

func handleProcessRunConcernRaise(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	caller := ""
	if agentID := processCallerAgentID(r); agentID != "" && peerActionFromRequest(r) == nil && processRunParticipant(runID, agentID) {
		caller = confirmedAgentConv(r)
	} else {
		var ok bool
		if caller, ok = requirePermission(w, r, PermProcessRunsManage); !ok {
			return
		}
	}
	var request processConcernRaiseRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_concern_request", err.Error())
		return
	}
	actor, err := processOversightActorOf(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	reason := strings.TrimSpace(request.Reason)
	setAuditDetail(r, "run: "+runID+", concern: "+stripControl(processConcernHeadline(reason)))
	source := db.ProcessConcernSourceAgent
	if caller == "" {
		source = db.ProcessConcernSourceOperator
	}
	// An agent whose concern nobody above it on the run would hear (it
	// oversees a run with no escalation agent, say) is asking for the
	// operator: that takes what reaching the operator always takes.
	if caller != "" && resolvePermission(caller, PermProcessRunsManage) != permAllow {
		raiser, _ := strings.CutPrefix(actor, "agent:")
		if nextProcessConcernStep(runID, "", raiser).Level == db.ProcessConcernLevelOperator {
			if _, ok := requireNotifyHumanPermission(w, r); !ok {
				return
			}
		}
	}
	c, err := raiseProcessConcern(runID, strings.TrimSpace(request.NodeID), source, "", actor, reason)
	if err != nil {
		writeProcessConcernError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusCreated, map[string]any{"concern": processConcernViewOf(runID, c.ID)})
}

func processConcernViewOf(runID string, id int64) *processRunConcernView {
	for _, v := range processRunConcernViews(runID) {
		if v.ID == id {
			return &v
		}
	}
	return nil
}

func handleProcessRunConcernAnswer(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(r.PathValue("concern"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "process_concern_id", "concern must be a positive number")
		return
	}
	c, err := db.GetProcessRunConcern(runID, id)
	if err != nil {
		writeProcessConcernError(w, err)
		return
	}
	caller := ""
	if agentID := processCallerAgentID(r); agentID != "" && peerActionFromRequest(r) == nil && c.Holder == processAgentRef(agentID) {
		caller = confirmedAgentConv(r)
	} else {
		var ok bool
		if caller, ok = requirePermission(w, r, PermProcessRunsManage); !ok {
			return
		}
	}
	var request processConcernAnswerRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_concern_request", err.Error())
		return
	}
	actor, err := processOversightActorOf(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	action := strings.TrimSpace(request.Action)
	note := strings.TrimSpace(request.Note)
	setAuditDetail(r, fmt.Sprintf("run: %s, concern: %d, answer: %s", runID, id, stripControl(action)))
	if !c.Open() {
		writeProcessConcernError(w, db.ErrProcessConcernClosed)
		return
	}
	var resolved db.ProcessRunConcern
	switch action {
	// Settling is conditional on the concern still waiting where it was read:
	// one the sweep passed up meanwhile is no longer this holder's to settle.
	case processConcernContinue:
		if resolved, err = db.ResolveProcessRunConcernAt(runID, id, c.Level, c.Holder, processConcernContinue, note, actor); err == nil {
			err = resumeProcessRunFor(runID, actor)
		}
	case processConcernPause:
		if err = pauseProcessRunFor(runID, actor); err == nil {
			resolved, err = db.ResolveProcessRunConcernAt(runID, id, c.Level, c.Holder, "paused", note, actor)
		}
	case processConcernEscalate:
		why := note
		if why == "" {
			why = processActorName(actor) + " escalated it"
		}
		resolved, err = escalateProcessConcern(c, actor, why)
	default:
		writeError(w, http.StatusBadRequest, "process_concern_action", "action must be continue, pause or escalate")
		return
	}
	if err != nil {
		writeProcessConcernError(w, err)
		return
	}
	if resolved.ID != 0 && !resolved.Open() {
		closeProcessConcernCard(resolved, actor)
		tellProcessOverseerOfAnswer(resolved, actor)
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"concern": processConcernViewOf(runID, id)})
}

// closeProcessConcernCard withdraws the operator card of a concern someone
// settled elsewhere, so the operator is not left a stale decision.
func closeProcessConcernCard(c db.ProcessRunConcern, actor string) {
	if c.HumanMessageID == 0 {
		return
	}
	by := db.HumanDismissedByAgent
	if actor == processOwnerOperator {
		by = db.HumanDismissedByOperator
	}
	if _, err := db.DismissHumanMessage(c.HumanMessageID, by, "settled by "+processActorName(actor)); err == nil {
		humanMessagesEnded([]int64{c.HumanMessageID}, "settled", actor)
	}
}

// tellProcessOverseerOfAnswer tells the run's overseer how a concern it did
// not settle itself was settled.
func tellProcessOverseerOfAnswer(c db.ProcessRunConcern, actor string) {
	o, err := resolveProcessRunOverseer(c.RunID)
	if err != nil || o.operator() || o.Ref == actor {
		return
	}
	conv, _ := db.CurrentConvForAgent(o.AgentID)
	if conv == "" {
		return
	}
	body := fmt.Sprintf("Concern #%d on run %s was settled by %s: %s.\n\nConcern: %s\n",
		c.ID, c.RunID, processActorName(actor), c.Resolution, sanitizeProcessText(c.Reason))
	if c.Note != "" {
		body += "\nNote: " + sanitizeProcessText(c.Note) + "\n"
	}
	body += processConcernFollowUp(c)
	if _, err := queueAgentMessage(&db.AgentMessage{
		ToConv: conv, ToRecipients: []string{conv},
		Subject: fmt.Sprintf("[process] concern #%d on %s: %s", c.ID, c.RunID, c.Resolution),
		Body:    body,
	}); err != nil {
		slog.Warn("process oversight: answer mail failed", "run", c.RunID, "concern", c.ID, "error", err)
	}
}

// processConcernFollowUp is what the overseer is asked to do after a pick the
// daemon cannot carry out by itself.
func processConcernFollowUp(c db.ProcessRunConcern) string {
	switch c.Resolution {
	case processChoiceSlug(processChoiceReassign):
		step := c.NodeID
		if step == "" {
			step = "<step>"
		}
		return fmt.Sprintf("\nThe operator wants the step handed to someone else. As its overseer, do it with\n"+
			"  tclaude process assign %s --node %s --agent <agent>\nthen resume the run if it is paused.\n", c.RunID, step)
	case "paused", processChoiceSlug(processChoiceKeepPaused):
		return fmt.Sprintf("\nThe run stays paused. Resume it with `tclaude process resume %s` when it can go on.\n", c.RunID)
	}
	return ""
}

func handleProcessRunOverseer(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processOverseerRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_overseer_request", err.Error())
		return
	}
	actor, err := processOversightActorOf(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	ref, err := processOverseerRefOf(request.Overseer)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_overseer", err.Error())
		return
	}
	setAuditDetail(r, "run: "+runID+", overseer: "+stripControl(ref))
	changed, err := db.SetProcessRunOverseer(runID, ref, actor)
	if err != nil {
		writeProcessConcernError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"changed": changed, "overseer": processRunOverseerViewOf(runID)})
}

// processOverseerRefOf resolves an overseer selector to the actor ref stored
// for it: "" clears, "operator" is the operator, anything else must name an
// active agent.
func processOverseerRefOf(selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	switch strings.ToLower(selector) {
	case "", "default":
		return "", nil
	case "operator", "human", processOwnerOperator:
		return processOwnerOperator, nil
	}
	resolved, _, err := agent.ResolveSelector(selector)
	if err != nil || resolved == nil || resolved.AgentID == "" || !activeProcessAgent(resolved.AgentID) {
		return "", fmt.Errorf("overseer %q is not an active agent", selector)
	}
	return processAgentRef(resolved.AgentID), nil
}

func writeProcessConcernError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrProcessConcernNotFound):
		writeError(w, http.StatusNotFound, "process_concern_not_found", err.Error())
	case errors.Is(err, db.ErrProcessConcernClosed):
		writeError(w, http.StatusConflict, "process_concern_closed", err.Error())
	case errors.Is(err, db.ErrProcessConcernLimit):
		writeError(w, http.StatusTooManyRequests, "process_concern_limit", err.Error())
	default:
		writeProcessRuntimeError(w, err)
	}
}
