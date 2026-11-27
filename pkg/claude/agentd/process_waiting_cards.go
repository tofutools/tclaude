package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// What a process run waits on a person for, a decision or a step a person
// performs, reaches them as a Messages card (tk_b9c30e99): one per step and
// attempt, put up when the run starts waiting and closed when the step is
// settled (here or anywhere else) or withdrawn. The card's choices are the
// step's; a pick goes through the same decide path the run view uses.

// processWaitingCardsMu serialises reconciles: the executor's hook calls for
// one run may overlap, and each compares the run with its cards.
var processWaitingCardsMu sync.Mutex

func init() {
	executor.SetWaitingHook(reconcileProcessWaitingCards)
}

// processWaitingKey names one card: a step, its attempt, and its kind.
type processWaitingKey struct {
	nodeID  string
	attempt int
	kind    string
}

// processPersonChoiceLabel is a step's choice as its card's button says it,
// as the dashboard says it: the default pair as plain words, a template's own
// choices as written. A decision's verdicts are said as written.
func processPersonChoiceLabel(kind, choice string) string {
	if kind == processAwaitingTask {
		switch choice {
		case engine.HumanChoiceDone:
			return "Done"
		case engine.HumanChoiceFailed:
			return "Couldn't do it"
		}
	}
	return choice
}

// reconcileProcessWaitingCards puts up a card for each thing the run waits on
// a person for that has none, and closes the cards of those it no longer
// waits on.
func reconcileProcessWaitingCards(runID string) {
	processWaitingCardsMu.Lock()
	defer processWaitingCardsMu.Unlock()
	view, err := loadProcessRunView(runID)
	if err != nil {
		slog.Warn("process waiting cards: load run", "run", runID, "error", err)
		return
	}
	waiting := map[processWaitingKey]processRunDecisionView{}
	if view.Status == engine.RunRunning {
		for _, d := range view.AwaitingDecisions {
			waiting[processWaitingKey{d.NodeID, d.Attempt, d.Kind}] = d
		}
	}
	open, err := db.ListOpenProcessWaitingCards(runID)
	if err != nil {
		slog.Warn("process waiting cards: list", "run", runID, "error", err)
		return
	}
	for _, card := range open {
		if _, ok := waiting[processWaitingKey{card.NodeID, card.Attempt, card.Kind}]; !ok {
			closeProcessWaitingCard(view, card)
		}
	}
	carded := map[processWaitingKey]bool{}
	for _, card := range open {
		carded[processWaitingKey{card.NodeID, card.Attempt, card.Kind}] = true
	}
	for key, d := range waiting {
		if !carded[key] {
			postProcessWaitingCard(view, d)
		}
	}
}

// postProcessWaitingCard puts up the card for one thing the run waits on a
// person for, unless it already has one.
func postProcessWaitingCard(view processRunView, d processRunDecisionView) {
	// The card names the step and its process as their authors did; the
	// dashboard links the process to the run, so no ids are spelled out.
	name, step := processRunNames(view.ID, d.NodeID)
	step = strings.TrimSuffix(step, " ("+d.NodeID+")")
	subject := fmt.Sprintf("Your decision: %s · %s", step, name)
	if d.Kind == processAwaitingTask {
		subject = fmt.Sprintf("Waiting on you: %s · %s", step, name)
	}
	var b strings.Builder
	ask, prompt := sanitizeProcessText(d.Ask), sanitizeProcessText(d.Prompt)
	if ask != "" {
		fmt.Fprintf(&b, "%s\n\n", ask)
	}
	if prompt != "" {
		fmt.Fprintf(&b, "%s\n\n", prompt)
	}
	if ask == "" && prompt == "" {
		fmt.Fprintf(&b, "%s waits on you.\n\n", step)
	}
	b.WriteString("Answer here or in the run; this updates when it's settled.")
	options := make([]string, 0, len(d.Verdicts))
	for _, choice := range d.Verdicts {
		options = append(options, processPersonChoiceLabel(d.Kind, choice))
	}
	m := &db.HumanMessage{
		FromTitle: name, GroupName: view.Scope.Group,
		Subject: subject, Body: b.String(),
		ProcessRunID: view.ID, ProcessNodeID: d.NodeID,
		Options: options,
	}
	card := db.ProcessWaitingCard{RunID: view.ID, NodeID: d.NodeID, Attempt: d.Attempt, Kind: d.Kind}
	if d.Kind == processAwaitingTask && len(d.Outputs) > 0 {
		card.Outputs = slices.Clone(d.Outputs)
		for _, choice := range d.Passing {
			card.Passing = append(card.Passing, processPersonChoiceLabel(d.Kind, choice))
		}
	}
	id, inserted, err := db.InsertProcessWaitingCardWithMessage(card, m)
	if err != nil {
		slog.Warn("process waiting cards: put up", "run", view.ID, "node", d.NodeID, "error", err)
		return
	}
	if inserted {
		dispatchHumanQuestionNotification("", m.FromTitle, m.GroupName, m.Subject, m.Body)
		slog.Debug("process waiting cards: put up", "run", view.ID, "node", d.NodeID, "message", id)
	}
}

// closeProcessWaitingCard closes a card the run no longer waits on: as
// decided, with the choice, when the step was settled; as withdrawn when not.
func closeProcessWaitingCard(view processRunView, card db.ProcessWaitingCard) {
	if closed, err := db.CloseProcessWaitingCard(card); err != nil || !closed {
		if err != nil {
			slog.Warn("process waiting cards: close", "run", card.RunID, "node", card.NodeID, "error", err)
		}
		return
	}
	m, err := db.GetHumanMessage(card.HumanMessageID)
	if err != nil || m == nil || !m.IsOpenDecision() {
		return
	}
	if choice, ok := processWaitingCardSettledWith(card); ok {
		if _, err := db.DecideHumanMessage(m.ID, processPersonChoiceLabel(card.Kind, choice)); err != nil {
			slog.Warn("process waiting cards: record settled", "message", m.ID, "error", err)
		}
		return
	}
	reason := "it no longer waits on you"
	switch view.Status {
	case engine.RunCanceled:
		reason = "the run was canceled"
	case engine.RunFailed:
		reason = "the run failed"
	case engine.RunCompleted:
		reason = "the run finished"
	}
	if _, err := db.DismissHumanMessage(m.ID, processWaitingDismissedBy, reason); err != nil {
		slog.Warn("process waiting cards: withdraw", "message", m.ID, "error", err)
	}
}

// processWaitingDismissedBy marks a card the run withdrew, for the dashboard.
const processWaitingDismissedBy = "process"

// processWaitingCardSettledWith is the choice a card's step was settled with,
// read from the step's latest report or verdict at the card's attempt.
func processWaitingCardSettledWith(card db.ProcessWaitingCard) (string, bool) {
	kind := "decision_recorded"
	if card.Kind == processAwaitingTask {
		kind = "human_reported"
	}
	events, err := db.ListLatestProcessRunNodeEvents(card.RunID, kind)
	if err != nil {
		return "", false
	}
	for _, event := range events {
		if event.NodeID != card.NodeID {
			continue
		}
		var payload struct {
			Choice  string `json:"choice"`
			Verdict string `json:"verdict"`
			Attempt int    `json:"attempt"`
		}
		if json.Unmarshal(event.PayloadJSON, &payload) != nil {
			return "", false
		}
		// A decision's first window is 0 (left out of the payload), so a card
		// at a later window or attempt only matches its own.
		if payload.Attempt != card.Attempt {
			return "", false
		}
		if card.Kind == processAwaitingTask {
			return payload.Choice, payload.Choice != ""
		}
		return payload.Verdict, payload.Verdict != ""
	}
	return "", false
}

// sweepProcessWaitingCards reconciles every run that waits on a person or
// holds an open card: what a restart between a commit and its card left.
func sweepProcessWaitingCards() {
	runIDs, err := db.ListProcessRunIDsWaitingOnPerson()
	if err != nil {
		slog.Warn("process waiting cards: sweep", "error", err)
		return
	}
	for _, runID := range runIDs {
		reconcileProcessWaitingCards(runID)
	}
}

// answerProcessWaitingCard carries out the operator's pick on a card: the
// step's choice, through the decide path, as the operator.
func answerProcessWaitingCard(orig *db.HumanMessage, card db.ProcessWaitingCard, decision string, body humanMessageReply) (map[string]any, *humanReplyFailure) {
	if decision == "" {
		return nil, replyFail(http.StatusBadRequest, "invalid_arg", "pick one of the card's choices; a note goes with the pick")
	}
	if body.AttachmentToken != "" {
		return nil, replyFail(http.StatusBadRequest, "invalid_arg", "attachments are not supported for process decisions")
	}
	// Under the reconcile lock, so a settle elsewhere cannot close the card
	// between this pick's claim and its release.
	processWaitingCardsMu.Lock()
	defer processWaitingCardsMu.Unlock()
	if current, ok, err := db.GetProcessWaitingCardForHumanMessage(orig.ID); err == nil && ok {
		card = current
	}
	if card.Closed {
		return nil, replyFail(http.StatusConflict, "decided", "this step no longer waits on you")
	}
	view, err := loadProcessRunView(card.RunID)
	if err != nil {
		return nil, replyFail(http.StatusInternalServerError, "io", "load run: "+err.Error())
	}
	var entry *processRunDecisionView
	for i, d := range view.AwaitingDecisions {
		if d.NodeID == card.NodeID && d.Attempt == card.Attempt && d.Kind == card.Kind {
			entry = &view.AwaitingDecisions[i]
			break
		}
	}
	index := body.Choice - 1
	if entry == nil || view.Status != engine.RunRunning {
		return nil, replyFail(http.StatusConflict, "decided", "this step no longer waits on you")
	}
	if index < 0 || index >= len(entry.Verdicts) || processPersonChoiceLabel(card.Kind, entry.Verdicts[index]) != decision {
		return nil, replyFail(http.StatusConflict, "stale", "this step's choices changed; open the run to finish it")
	}
	verdict := entry.Verdicts[index]
	if card.Kind == processAwaitingTask && len(entry.Outputs) > 0 && slices.Contains(entry.Passing, verdict) &&
		(len(entry.Outputs) > 1 || strings.TrimSpace(body.Body) == "") {
		return nil, replyFail(http.StatusConflict, "needs_outputs",
			fmt.Sprintf("finishing this step as %s fills in %s: do that in the run view", decision, strings.Join(entry.Outputs, ", ")))
	}
	claimed, err := db.DecideHumanMessage(orig.ID, decision)
	if err != nil {
		return nil, replyFail(http.StatusInternalServerError, "io", "record decision: "+err.Error())
	}
	if !claimed {
		return nil, replyFail(http.StatusConflict, "decided", "this decision has already been answered or dismissed")
	}
	if _, err := processRuns.submit(card.RunID, processOwnerOperator, processRunInput{
		decision: &executor.DecisionInput{
			NodeID: card.NodeID, Verdict: verdict, Evidence: body.Body,
			Attempt: entry.Attempt, CurrentWindow: entry.Attempt == 0,
		}}); err != nil {
		if uerr := db.UndecideHumanMessage(orig.ID); uerr != nil {
			slog.Warn("process waiting cards: release a failed pick", "id", orig.ID, "error", uerr)
		}
		return nil, replyFail(http.StatusConflict, "process_decide", err.Error())
	}
	if _, err := db.CloseProcessWaitingCard(card); err != nil {
		slog.Warn("process waiting cards: close answered", "id", orig.ID, "error", err)
	}
	return map[string]any{"resolved": true, "run_id": card.RunID, "node_id": card.NodeID, "decision": decision}, nil
}
