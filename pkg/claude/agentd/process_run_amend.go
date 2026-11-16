package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// Amending a run (tk_e2fa2bd0 3c). A run gets its own copy of its template,
// changed only where the run has not been yet (engine.Amend). Someone with
// process.runs.manage or process.templates.manage applies an amendment at
// once; the run's overseer proposes one, and the operator applies or
// rejects it from a card that carries the diff. An amendment formed against
// a run that has moved on since is refused: it was checked against a
// frontier the run has left.

const (
	processChoiceApplyAmendment  = "Apply amendment"
	processChoiceRejectAmendment = "Reject"
)

type processRunAmendRequest struct {
	// Template is the amended template as JSON, or YAML the run's copy as
	// printed (one of the two).
	Template json.RawMessage `json:"template,omitempty"`
	YAML     string          `json:"yaml,omitempty"`
	Note     string          `json:"note,omitempty"`
	// DryRun checks the amendment and says what it changes, storing nothing.
	DryRun bool `json:"dryRun,omitempty"`
}

// MaxProcessAmendYAMLBytes bounds an amendment sent as YAML.
const MaxProcessAmendYAMLBytes = 1 << 20

func (request processRunAmendRequest) template() (*model.Template, error) {
	if (len(request.Template) == 0) == (request.YAML == "") {
		return nil, errors.New("send the amended template as template (JSON) or yaml, one of the two")
	}
	if request.YAML != "" {
		if len(request.YAML) > MaxProcessAmendYAMLBytes {
			return nil, errors.New("yaml is too large")
		}
		parsed, err := model.ParseAuthoring([]byte(request.YAML))
		if err != nil {
			return nil, err
		}
		if errs := parsed.Diagnostics.Errors(); len(errs) > 0 {
			return nil, fmt.Errorf("the amended template does not validate: %s", errs[0].Message)
		}
		return parsed.Template, nil
	}
	var amended model.Template
	if err := strictjson.Decode(request.Template, &amended); err != nil {
		return nil, err
	}
	return &amended, nil
}

// processRunAmendmentView is one amendment as the run view and API show it.
type processRunAmendmentView struct {
	ID               int64               `json:"id"`
	State            string              `json:"state"`
	ProposedBy       string              `json:"proposedBy"`
	ProposedName     string              `json:"proposedName,omitempty"`
	Note             string              `json:"note,omitempty"`
	Summary          engine.AmendSummary `json:"summary"`
	MessageID        int64               `json:"messageId,omitempty"`
	DecidedBy        string              `json:"decidedBy,omitempty"`
	DecidedName      string              `json:"decidedName,omitempty"`
	DecisionNote     string              `json:"decisionNote,omitempty"`
	SavedTemplateRef string              `json:"savedTemplateRef,omitempty"`
	CreatedAt        string              `json:"createdAt"`
	DecidedAt        string              `json:"decidedAt,omitempty"`
	// Outdated marks a proposal the run has moved on from since: applying it
	// would be refused, so it has to be proposed again.
	Outdated bool `json:"outdated,omitempty"`
}

func processRunAmendmentViewOf(a db.ProcessRunAmendment) processRunAmendmentView {
	v := processRunAmendmentView{
		ID: a.ID, State: a.State, ProposedBy: a.ProposedBy, ProposedName: processActorName(a.ProposedBy),
		Note: a.Note, MessageID: a.HumanMessageID, DecidedBy: a.DecidedBy, DecisionNote: a.DecisionNote,
		SavedTemplateRef: a.SavedTemplateRef, CreatedAt: a.CreatedAt.UTC().Format(timeRFC3339),
	}
	_ = json.Unmarshal(a.SummaryJSON, &v.Summary)
	if a.DecidedBy != "" {
		v.DecidedName = processActorName(a.DecidedBy)
	}
	if !a.DecidedAt.IsZero() {
		v.DecidedAt = a.DecidedAt.UTC().Format(timeRFC3339)
	}
	return v
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// processRunAmendmentViews are a run's amendments for its view, newest first;
// stateVersion is the run's, to tell which proposals it has moved on from.
func processRunAmendmentViews(runID string, stateVersion int64) []processRunAmendmentView {
	rows, err := db.ListProcessRunAmendments(runID)
	if err != nil {
		return nil
	}
	out := make([]processRunAmendmentView, 0, len(rows))
	for _, a := range rows {
		v := processRunAmendmentViewOf(a)
		v.Outdated = a.State == db.ProcessAmendmentProposed && a.BaseStateVersion != stateVersion
		out = append(out, v)
	}
	return out
}

// processAmendSummaryText says an amendment's change in one line.
func processAmendSummaryText(s engine.AmendSummary) string {
	var parts []string
	add := func(label string, ids []string) {
		if len(ids) > 0 {
			parts = append(parts, label+" "+strings.Join(ids, ", "))
		}
	}
	add("adds", s.Added)
	add("removes", s.Removed)
	add("changes", s.Changed)
	add("reroutes", s.Rerouted)
	if s.Settings {
		parts = append(parts, "changes template settings")
	}
	if len(parts) == 0 {
		return "changes nothing"
	}
	return strings.Join(parts, "; ")
}

// processAmendDirect reports whether the caller applies an amendment at once:
// the operator, or an agent holding process.runs.manage or
// process.templates.manage. An overseer without them proposes.
func processAmendDirect(r *http.Request) bool {
	conv := confirmedAgentConv(r)
	if conv == "" {
		return false
	}
	return standingPermissionAllows(conv, PermProcessRunsManage, ActionContext{}) ||
		standingPermissionAllows(conv, PermProcessTemplatesManage, ActionContext{})
}

func handleProcessRunAmend(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	var caller string
	direct := processAmendDirect(r)
	if conv := confirmedAgentConv(r); direct && conv != "" && peerActionFromRequest(r) == nil {
		// process.runs.manage or process.templates.manage: applies at once.
		caller = conv
	} else if !direct && conv != "" && peerActionFromRequest(r) == nil && processRunIsOverseer(runID, peerAgentID(conv)) {
		caller = conv
	} else {
		var ok bool
		if caller, ok = requirePermission(w, r, PermProcessRunsManage); !ok {
			return
		}
		direct = true
	}
	var request processRunAmendRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	if err := executor.ValidateAgentReportNote(request.Note); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	amended, err := request.template()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template: "+err.Error())
		return
	}
	actor, err := processOversightActorOf(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	setAuditDetail(r, "run: "+runID+", amend")
	run, err := executor.LoadRun(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	_, _, summary, err := executor.PlanAmendment(run, amended)
	if err != nil {
		writeProcessAmendError(w, err)
		return
	}
	if request.DryRun {
		writeProcessJSON(w, http.StatusOK, map[string]any{"summary": summary, "direct": direct})
		return
	}
	snapshot, _ := json.Marshal(amended)
	summaryJSON, _ := json.Marshal(summary)
	id, err := db.InsertProcessRunAmendment(db.ProcessRunAmendment{
		RunID: runID, ProposedBy: actor, Note: strings.TrimSpace(request.Note),
		BaseStateVersion: run.StateVersion(), TemplateJSON: snapshot, SummaryJSON: summaryJSON,
	})
	if errors.Is(err, db.ErrProcessAmendmentLimit) {
		writeError(w, http.StatusConflict, "process_amendment_limit", err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if direct {
		if err := applyProcessRunAmendment(runID, id, actor); err != nil {
			// Nothing will decide it later: it has no card.
			_ = db.DecideProcessRunAmendment(runID, id, db.ProcessAmendmentStale, actor, err.Error())
			writeProcessAmendError(w, err)
			return
		}
	} else if err := postProcessAmendmentCard(runID, id); err != nil {
		slog.Warn("process amend: card failed", "run", runID, "amendment", id, "error", err)
	}
	a, _ := db.GetProcessRunAmendment(runID, id)
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	status := http.StatusAccepted
	if !direct {
		status = http.StatusCreated
	}
	writeProcessJSON(w, status, map[string]any{"amendment": processRunAmendmentViewOf(a), "run": view})
}

// applyProcessRunAmendment applies a stored proposal through the run's owner.
// A run that moved on since leaves the proposal stale.
func applyProcessRunAmendment(runID string, id int64, actor string) error {
	a, err := db.GetProcessRunAmendment(runID, id)
	if err != nil {
		return err
	}
	if a.State != db.ProcessAmendmentProposed {
		return fmt.Errorf("%w: it is %s", db.ErrProcessAmendmentDecided, a.State)
	}
	var amended model.Template
	if err := strictjson.Decode(a.TemplateJSON, &amended); err != nil {
		return err
	}
	_, err = processRuns.submit(runID, actor, processRunInput{amend: &executor.AmendInput{
		AmendmentID: id, BaseStateVersion: a.BaseStateVersion, Template: &amended, Note: a.Note,
	}})
	if err != nil {
		if errors.Is(err, executor.ErrAmendmentStale) || errors.Is(err, engine.ErrAmendmentRefused) {
			if derr := db.DecideProcessRunAmendment(runID, id, db.ProcessAmendmentStale, actor, err.Error()); derr != nil {
				slog.Warn("process amend: mark stale", "run", runID, "amendment", id, "error", derr)
			}
		}
		return err
	}
	processTemplateOversightCache.Delete(runID)
	kickProcessAgentDispatch(runID)
	return nil
}

func writeProcessAmendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, executor.ErrAmendmentStale):
		writeError(w, http.StatusConflict, "process_amendment_stale", err.Error())
	case errors.Is(err, engine.ErrAmendmentRefused):
		writeError(w, http.StatusUnprocessableEntity, "process_amendment_refused", err.Error())
	case errors.Is(err, db.ErrProcessAmendmentDecided):
		writeError(w, http.StatusConflict, "process_amendment_decided", err.Error())
	default:
		writeProcessRuntimeError(w, err)
	}
}

// postProcessAmendmentCard puts a proposal to the operator, with what it
// changes, so it can be decided without opening the editor.
func postProcessAmendmentCard(runID string, id int64) error {
	a, err := db.GetProcessRunAmendment(runID, id)
	if err != nil {
		return err
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		return err
	}
	var summary engine.AmendSummary
	_ = json.Unmarshal(a.SummaryJSON, &summary)
	var b strings.Builder
	fmt.Fprintf(&b, "%s proposes amending run %s", processActorName(a.ProposedBy), runID)
	if view.TemplateRef != "" {
		fmt.Fprintf(&b, " (%s)", processRunTemplateID(view.TemplateRef))
	}
	b.WriteString(". Only this run changes; the template stays as it is.\n\n")
	line := func(label string, ids []string) {
		if len(ids) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(ids, ", "))
		}
	}
	line("Steps added", summary.Added)
	line("Steps removed", summary.Removed)
	line("Steps changed", summary.Changed)
	line("Steps rerouted", summary.Rerouted)
	if summary.Settings {
		b.WriteString("Template settings changed\n")
	}
	if a.Note != "" {
		fmt.Fprintf(&b, "\nWhy: %s\n", sanitizeProcessText(a.Note))
	}
	b.WriteString("\nIf the run moves on before you decide, applying it is refused and it has to be proposed again.")
	m := &db.HumanMessage{
		FromTitle: "Process oversight", GroupName: view.Scope.Group,
		Subject:      fmt.Sprintf("Amendment to %s: %s", runID, processAmendSummaryText(summary)),
		Body:         b.String(),
		ProcessRunID: runID,
		Options:      []string{processChoiceApplyAmendment, processChoiceRejectAmendment},
		Recommended:  1,
	}
	msgID, err := db.InsertHumanMessage(m)
	if err != nil {
		return err
	}
	if err := db.SetProcessRunAmendmentMessage(id, msgID); err != nil {
		return err
	}
	dispatchHumanMessageNotification("", m.FromTitle, view.Scope.Group, m.Subject, m.Body)
	mirrorHumanMessage(msgID)
	return nil
}

// answerProcessAmendmentCard carries out the operator's pick on a proposal.
func answerProcessAmendmentCard(orig *db.HumanMessage, a db.ProcessRunAmendment, decision string, body humanMessageReply) (map[string]any, *humanReplyFailure) {
	if decision != processChoiceApplyAmendment && decision != processChoiceRejectAmendment {
		return nil, replyFail(http.StatusBadRequest, "invalid_arg", "pick Apply amendment or Reject")
	}
	if a.State != db.ProcessAmendmentProposed {
		return nil, replyFail(http.StatusConflict, "decided", "this amendment is already "+a.State)
	}
	claimed, err := db.DecideHumanMessage(orig.ID, decision)
	if err != nil {
		return nil, replyFail(http.StatusInternalServerError, "io", "record decision: "+err.Error())
	}
	if !claimed {
		return nil, replyFail(http.StatusConflict, "decided", "this decision has already been answered or dismissed")
	}
	settled := func() {
		if _, err := db.MarkHumanMessageRead(orig.ID); err != nil {
			slog.Warn("process amend: mark card read", "id", orig.ID, "error", err)
		}
		humanMessageAnswered(orig, body.Choice, body.Body)
	}
	outcome := "rejected"
	if decision == processChoiceRejectAmendment {
		if err := db.DecideProcessRunAmendment(a.RunID, a.ID, db.ProcessAmendmentRejected, processOwnerOperator, body.Body); err != nil {
			return nil, replyFail(http.StatusConflict, "decided", err.Error())
		}
	} else {
		if err := applyProcessRunAmendment(a.RunID, a.ID, processOwnerOperator); err != nil {
			if !errors.Is(err, executor.ErrAmendmentStale) && !errors.Is(err, engine.ErrAmendmentRefused) {
				// A passing failure: the proposal stays, so give its card back.
				if uerr := db.UndecideHumanMessage(orig.ID); uerr != nil {
					slog.Warn("process amend: release a failed pick", "id", orig.ID, "error", uerr)
				}
				return nil, replyFail(http.StatusConflict, "process_amendment", err.Error())
			}
			settled()
			tellProcessAmendmentProposer(a, "could not be applied: "+err.Error())
			if errors.Is(err, executor.ErrAmendmentStale) {
				return nil, replyFail(http.StatusConflict, "process_amendment_stale",
					"the run moved on since this amendment was proposed; it was not applied and has to be proposed again")
			}
			return nil, replyFail(http.StatusConflict, "process_amendment", err.Error())
		}
		outcome = "applied"
	}
	settled()
	tellProcessAmendmentProposer(a, outcome+" by the operator")
	return map[string]any{"resolved": true, "run_id": a.RunID, "amendment": a.ID, "decision": decision}, nil
}

// tellProcessAmendmentProposer mails an agent that proposed an amendment
// what became of it.
func tellProcessAmendmentProposer(a db.ProcessRunAmendment, what string) {
	agentID, ok := strings.CutPrefix(a.ProposedBy, "agent:")
	if !ok {
		return
	}
	conv, _ := db.CurrentConvForAgent(agentID)
	if conv == "" {
		return
	}
	if _, err := queueAgentMessage(&db.AgentMessage{
		ToConv: conv, ToRecipients: []string{conv},
		Subject: fmt.Sprintf("[process] amendment #%d to %s %s", a.ID, a.RunID, strings.SplitN(what, ":", 2)[0]),
		Body:    fmt.Sprintf("Your amendment #%d to run %s was %s.\n", a.ID, a.RunID, sanitizeProcessText(what)),
	}); err != nil {
		slog.Warn("process amend: proposer mail failed", "run", a.RunID, "amendment", a.ID, "error", err)
	}
}

// handleProcessRunAmendments lists a run's amendments, newest first.
func handleProcessRunAmendments(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	if _, ok := requireProcessRunRead(w, r, runID); !ok {
		return
	}
	record, err := db.GetProcessRun(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"amendments": processRunAmendmentViews(runID, record.StateVersion)})
}

// handleProcessRunAmendmentTemplate returns the template an amendment
// proposed (or applied), for review and for saving it back.
func handleProcessRunAmendmentTemplate(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	if _, ok := requireProcessRunRead(w, r, runID); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("amendment"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "amendment id must be a number")
		return
	}
	a, err := db.GetProcessRunAmendment(runID, id)
	if errors.Is(err, db.ErrProcessAmendmentNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"amendment": processRunAmendmentViewOf(a), "template": a.TemplateJSON})
}

// handleProcessRunTemplate returns the run's own template as it stands: the
// starting point for an amendment.
func handleProcessRunTemplate(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	if _, ok := requireProcessRunRead(w, r, runID); !ok {
		return
	}
	record, err := db.GetProcessRun(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		writeProcessRuntimeError(w, fmt.Errorf("%w: decode template snapshot: %v", executor.ErrInvalidRun, err))
		return
	}
	source, err := yaml.Marshal(&tmpl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{
		"template": record.TemplateSnapshotJSON, "yaml": string(source), "stateVersion": record.StateVersion,
		"canAmendDirectly": processAmendDirect(r) || confirmedAgentConv(r) == "",
	})
}
