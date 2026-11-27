package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// The operator's decision card for a concern that reached the end of the
// chain. It is an ordinary notify-human decision (Messages, and the run's
// oversight panel), so the operator answers it anywhere a decision is
// answered; replyHumanMessage hands the pick here instead of mailing a
// sender. The daemon carries out what it can (resume, skip a blocked step,
// cancel) and posts the pick to the overseer, who does the rest (reassign).

// The card's choices. Which are offered depends on the run's state when the
// card is put up, and each is checked again when it is picked.
const (
	processChoiceResume     = "Resume"
	processChoiceSkip       = "Skip this step"
	processChoiceReassign   = "Reassign"
	processChoiceKeepPaused = "Keep paused"
	processChoiceCancel     = "Cancel the run"
	// processChoiceSendBackPrefix starts "Send back to <step>", offered when
	// the concern names a finished step of the run's own graph.
	processChoiceSendBackPrefix = "Send back to "
)

// processChoiceSlug is the resolution a choice records.
func processChoiceSlug(choice string) string {
	switch choice {
	case processChoiceResume:
		return "resume"
	case processChoiceSkip:
		return "skip"
	case processChoiceReassign:
		return "reassign"
	case processChoiceKeepPaused:
		return "keep_paused"
	case processChoiceCancel:
		return "cancel"
	}
	if strings.HasPrefix(choice, processChoiceSendBackPrefix) {
		return "send_back"
	}
	return ""
}

// processConcernBlocked is the parked branch a concern is about: the blocked
// entry at its step (or under it), or the run's only blocked entry when the
// concern names no step.
func processConcernBlocked(view processRunView, nodeID string) *processRunBlockedView {
	if nodeID == "" {
		if len(view.Blocked) == 1 {
			return &view.Blocked[0]
		}
		return nil
	}
	for i, b := range view.Blocked {
		if b.NodeID == nodeID || strings.HasPrefix(b.NodeID, nodeID+".") {
			return &view.Blocked[i]
		}
	}
	return nil
}

// processConcernReassignable reports whether the concern's step has an agent
// attempt someone else could take.
func processConcernReassignable(view processRunView, nodeID string) bool {
	if nodeID == "" {
		return false
	}
	for _, c := range view.Commands {
		if c.Kind != engine.CommandAgent || (c.NodeID != nodeID && !strings.HasPrefix(c.NodeID, nodeID+".")) {
			continue
		}
		switch c.State {
		case db.ProcessDispatchPending, db.ProcessDispatchDelivered, processCommandNeedsPerformer, processCommandAwaitPerformer:
			return true
		}
	}
	return false
}

// processConcernChoices are the choices a card offers for the run as it is.
func processConcernChoices(view processRunView, c db.ProcessRunConcern) []string {
	choices := []string{processChoiceResume}
	if processConcernBlocked(view, c.NodeID) != nil {
		choices = append(choices, processChoiceSkip)
	}
	if processConcernReassignable(view, c.NodeID) {
		choices = append(choices, processChoiceReassign)
	}
	// The concern's own step, when it finished and is of the run's own
	// graph, is one the run can go back to; a deeper rewind is the run view's.
	if c.NodeID != "" && !strings.ContainsAny(c.NodeID, "./") && view.Checkpoint.Nodes[c.NodeID] == engine.NodeDone {
		choices = append(choices, processChoiceSendBackPrefix+c.NodeID)
	}
	return append(choices, processChoiceKeepPaused, processChoiceCancel)
}

// postProcessConcernCard puts a concern to the operator and returns the
// message id.
func postProcessConcernCard(c db.ProcessRunConcern, why string) (int64, error) {
	view, err := loadProcessRunView(c.RunID)
	if err != nil {
		return 0, err
	}
	group := view.Scope.Group
	var b strings.Builder
	fmt.Fprintf(&b, "%s raised a concern on run %s", processActorName(c.RaisedBy), c.RunID)
	if view.TemplateRef != "" {
		fmt.Fprintf(&b, " (%s)", processRunTemplateID(view.TemplateRef))
	}
	if c.NodeID != "" {
		fmt.Fprintf(&b, " at step %s", c.NodeID)
	}
	fmt.Fprintf(&b, ":\n\n%s\n", sanitizeProcessText(c.Reason))
	if why != "" {
		fmt.Fprintf(&b, "\nWhy it reached you: %s\n", sanitizeProcessText(why))
	}
	if view.Paused != nil {
		fmt.Fprintf(&b, "\nThe run is paused (by %s).", processActorName(view.Paused.By))
	} else {
		b.WriteString("\nThe run is still going.")
	}
	overseer := processRunOverseerViewOf(c.RunID)
	if overseer != nil && overseer.AgentID != "" {
		fmt.Fprintf(&b, " Your pick is applied to the run, recorded in its log and posted to its overseer, %s.", overseer.Name)
	} else {
		b.WriteString(" Your pick is applied to the run and recorded in its log.")
	}
	m := &db.HumanMessage{
		FromTitle: "Process oversight", GroupName: group,
		Subject:      fmt.Sprintf("Concern on %s: %s", c.RunID, processConcernHeadline(c.Reason)),
		Body:         b.String(),
		ProcessRunID: c.RunID, ProcessNodeID: c.NodeID,
		Options:     processConcernChoices(view, c),
		Recommended: 1,
	}
	id, err := db.InsertHumanMessage(m)
	if err != nil {
		return 0, err
	}
	dispatchHumanFor(m.Options)("", m.FromTitle, group, m.Subject, m.Body)
	mirrorHumanMessage(id)
	return id, nil
}

// answerProcessConcernCard carries out the operator's pick on a concern card.
func answerProcessConcernCard(orig *db.HumanMessage, c db.ProcessRunConcern, decision string, body humanMessageReply) (map[string]any, *humanReplyFailure) {
	if decision == "" {
		return nil, replyFail(http.StatusBadRequest, "invalid_arg", "pick one of the card's choices; a note goes with the pick")
	}
	if body.AttachmentToken != "" {
		return nil, replyFail(http.StatusBadRequest, "invalid_arg", "attachments are not supported for process decisions")
	}
	if !c.Open() {
		return nil, replyFail(http.StatusConflict, "decided", "this concern was already settled")
	}
	claimed, err := db.DecideHumanMessage(orig.ID, decision)
	if err != nil {
		return nil, replyFail(http.StatusInternalServerError, "io", "record decision: "+err.Error())
	}
	if !claimed {
		return nil, replyFail(http.StatusConflict, "decided", "this decision has already been answered or dismissed")
	}
	if err := applyProcessConcernChoice(c, decision, body.Body); err != nil {
		if uerr := db.UndecideHumanMessage(orig.ID); uerr != nil {
			slog.Warn("process oversight: release a failed pick", "id", orig.ID, "error", uerr)
		}
		return nil, replyFail(http.StatusConflict, "process_concern", err.Error())
	}
	resolved, err := db.ResolveProcessRunConcern(c.RunID, c.ID, processChoiceSlug(decision), body.Body, processOwnerOperator)
	if err != nil {
		// The pick was applied; a concurrent settlement only lost the record.
		slog.Warn("process oversight: record the operator's pick", "run", c.RunID, "concern", c.ID, "error", err)
		resolved = c
		resolved.Resolution, resolved.Note = processChoiceSlug(decision), body.Body
	}
	if _, err := db.MarkHumanMessageRead(orig.ID); err != nil {
		slog.Warn("process oversight: mark card read", "id", orig.ID, "error", err)
	}
	humanMessageAnswered(orig, body.Choice, body.Body)
	tellProcessOverseerOfAnswer(resolved, processOwnerOperator)
	return map[string]any{"resolved": true, "run_id": c.RunID, "concern": c.ID, "decision": decision}, nil
}

// applyProcessConcernChoice carries out a pick on the run.
func applyProcessConcernChoice(c db.ProcessRunConcern, choice, note string) error {
	actor := processOwnerOperator
	switch choice {
	case processChoiceResume:
		return resumeProcessRunFor(c.RunID, actor)
	case processChoiceKeepPaused:
		return pauseProcessRunFor(c.RunID, actor)
	case processChoiceReassign:
		// The overseer picks who; the pick is posted to it.
		return nil
	case processChoiceSendBackPrefix + c.NodeID:
		_, err := processRuns.submit(c.RunID, actor, processRunInput{back: &executor.SendBackInput{
			NodeID: c.NodeID,
			Note:   truncateBytes(strings.TrimSpace("concern #"+fmt.Sprint(c.ID)+": "+note), executor.MaxResolutionNoteBytes),
		}})
		if err != nil {
			return err
		}
		kickProcessAgentDispatch(c.RunID)
		// The step runs again: what waited on the concern can go on.
		return resumeProcessRunFor(c.RunID, actor)
	case processChoiceCancel:
		_, err := processRuns.submit(c.RunID, actor, processRunInput{cancel: &processRunCancelInput{
			note: truncateBytes(strings.TrimSpace("concern #"+fmt.Sprint(c.ID)+": "+note), executor.MaxResolutionNoteBytes),
		}})
		return err
	case processChoiceSkip:
		view, err := loadProcessRunView(c.RunID)
		if err != nil {
			return err
		}
		blocked := processConcernBlocked(view, c.NodeID)
		if blocked == nil {
			return fmt.Errorf("the step is no longer blocked")
		}
		_, err = processRuns.submit(c.RunID, actor, processRunInput{resolution: &executor.ResolutionInput{
			NodeID: blocked.NodeID, Attempt: blocked.Attempt, Action: engine.ResolveSkip,
			Note: truncateBytes(strings.TrimSpace("concern #"+fmt.Sprint(c.ID)+": "+note), executor.MaxResolutionNoteBytes),
		}})
		if err != nil {
			return err
		}
		// What waited on the skipped step can go on.
		return resumeProcessRunFor(c.RunID, actor)
	}
	return fmt.Errorf("unknown choice %q", choice)
}
