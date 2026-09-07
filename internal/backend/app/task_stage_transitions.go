package app

import (
	"context"
	"math"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func inheritTaskActivation(graph model.WorkGraph, current model.WorkNodeAttempt, next *model.WorkNodeAttempt, windows []model.DecisionWindow) {
	group, ok := taskGroup(graph, current.Ref.NodeID)
	nextGroup, nextOK := taskGroup(graph, next.Ref.NodeID)
	if !ok || !nextOK || group.ID != nextGroup.ID {
		return
	}
	next.Ref.ActivationID, next.Ref.Attempt = current.Ref.ActivationID, current.Ref.Attempt
	for i := range windows {
		windows[i].Attempt = next.Ref
	}
}

func taskStageAttempts(run model.WorkRun, activation model.WorkActivationID, node model.WorkNodeID) (model.WorkNodeAttempt, uint32) {
	var latest model.WorkNodeAttempt
	var count uint32
	for _, attempt := range run.NodeAttempts {
		if attempt.Ref.ActivationID != activation || attempt.Ref.NodeID != node {
			continue
		}
		count++
		if attempt.Ref.Attempt > latest.Ref.Attempt {
			latest = attempt
		}
	}
	return latest, count
}

func (s *Service) retryTaskStage(run model.WorkRun, current model.WorkNodeAttempt, target model.WorkNodeID, budget uint32, now time.Time) (model.WorkNodeAttempt, []model.DecisionWindow) {
	node := graphNode(*run.Graph, target)
	anchor := current
	anchor.Ref.NodeID = target
	anchor.Performer = node.Performer
	anchor.Deadline = run.Deadline
	next, windows := s.retryActivation(run, anchor, node, now, budget)
	next.Detail = "Rework after stage " + string(current.Ref.NodeID) + ": " + current.Detail
	return next, windows
}

// A check/review rejection spends the shared work budget, not a second budget
// that would retry the check without correcting its input.
func (s *Service) taskGateFailure(record WorkRunRecord, current model.WorkNodeAttempt, detail string, transition GraphTransition) (GraphTransition, bool) {
	group, ok := taskGroup(*record.Run.Graph, current.Ref.NodeID)
	if !ok || current.Ref.NodeID != group.Review && !slices.Contains(group.Checks, current.Ref.NodeID) {
		return GraphTransition{}, false
	}
	work, count := taskStageAttempts(record.Run, current.Ref.ActivationID, group.Work)
	if work.Ref.NodeID == "" {
		return GraphTransition{}, false
	}
	if count < work.RetryBudget && current.Ref.Attempt < math.MaxUint32 {
		current.Detail = detail
		next, windows := s.retryTaskStage(record.Run, current, group.Work, work.RetryBudget, transition.At)
		taskStageFeedback(record, current, &next, detail)
		for i := range windows {
			if next.Performer.Human != nil {
				windows[i].Question = next.Performer.Human.Prompt
			}
		}
		transition.Activations, transition.DecisionWindows = []model.WorkNodeAttempt{next}, windows
		return transition, true
	}
	return s.parkTaskGate(record, current, detail, transition), true
}

func (s *Service) parkTaskGate(record WorkRunRecord, current model.WorkNodeAttempt, detail string, transition GraphTransition) GraphTransition {
	decisionID := model.DecisionID(s.newID("decision_"))
	transition.Updates[0] = GraphAttemptUpdate{Ref: current.Ref, DecisionID: decisionID, State: model.NodeAttemptBlocked, Outcome: model.WorkOutcomeRejected, Detail: detail}
	transition.DecisionWindows = append(transition.DecisionWindows, model.DecisionWindow{
		ID: decisionID, Kind: model.DecisionBlocked, SourceRevision: record.Run.Revision, Attempt: current.Ref,
		Audience: []model.DecisionAudience{{Subject: record.Run.Authority}}, Question: "Task checks failed and the work budget is exhausted. Retry the work or cancel?",
		PermittedAnswers: []string{string(model.BlockedRetry), string(model.BlockedRework), string(model.BlockedCancel)},
		ExpiresAt:        record.Run.Deadline, State: model.DecisionOpen, Revision: 1, CreatedAt: transition.At, UpdatedAt: transition.At,
	})
	virtual := slices.Clone(record.Run.NodeAttempts)
	for i := range virtual {
		if virtual[i].Ref == current.Ref {
			virtual[i].State = model.NodeAttemptBlocked
		}
	}
	if onlyWaiting(virtual) {
		transition.RunState, transition.ControlState = model.WorkRunWaiting, model.WorkControlWaiting
	}
	return transition
}

func (s *Service) reworkTaskPlan(ctx context.Context, run WorkRunRecord, current model.WorkNodeAttempt, submission model.DecisionSubmission, group model.CompiledTaskGroup) (WorkRunRecord, error) {
	if current.Ref.Attempt == math.MaxUint32 {
		return run, fail(ErrConflict, "task cycle limit reached")
	}
	current.Detail = submission.Reason
	plan, _ := taskStageAttempts(run.Run, current.Ref.ActivationID, group.Plan)
	next, windows := s.retryTaskStage(run.Run, current, group.Plan, plan.RetryBudget, s.now().UTC())
	taskStageFeedback(run, current, &next, submission.Reason)
	for i := range windows {
		if next.Performer.Human != nil {
			windows[i].Question = next.Performer.Human.Prompt
		}
	}
	transition := GraphTransition{WorkRunID: run.Run.ID, ExpectedRevision: run.Run.Revision,
		Updates:     []GraphAttemptUpdate{{Ref: current.Ref, State: model.NodeAttemptFailed, Outcome: model.WorkOutcomeRejected, Detail: submission.Reason}},
		Activations: []model.WorkNodeAttempt{next}, DecisionWindows: windows, RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: s.now().UTC()}
	return s.store.ApplyGraphTransition(ctx, transition)
}

func (s *Service) resolveTaskGate(ctx context.Context, run WorkRunRecord, current model.WorkNodeAttempt, submission model.DecisionSubmission, group model.CompiledTaskGroup) (WorkRunRecord, error) {
	work, _ := taskStageAttempts(run.Run, current.Ref.ActivationID, group.Work)
	window := normalizedAttempts(graphNode(*run.Run.Graph, group.Work).Retry.MaxAttempts)
	if work.Ref.NodeID == "" || work.RetryBudget >= maxWorkAttempts || window > maxWorkAttempts-work.RetryBudget || current.Ref.Attempt == math.MaxUint32 {
		return run, fail(ErrConflict, "task cannot extend work retry budget")
	}
	current.Detail = submission.Reason
	next, windows := s.retryTaskStage(run.Run, current, group.Work, work.RetryBudget+window, s.now().UTC())
	taskStageFeedback(run, current, &next, submission.Reason)
	for i := range windows {
		if next.Performer.Human != nil {
			windows[i].Question = next.Performer.Human.Prompt
		}
	}
	transition := GraphTransition{WorkRunID: run.Run.ID, ExpectedRevision: run.Run.Revision,
		Updates:     []GraphAttemptUpdate{{Ref: current.Ref, State: model.NodeAttemptFailed, Outcome: model.WorkOutcomeRejected, Detail: submission.Reason}},
		Activations: []model.WorkNodeAttempt{next}, DecisionWindows: windows, RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: s.now().UTC()}
	return s.store.ApplyGraphTransition(ctx, transition)
}

// Carry bounded stage feedback into agent/human work without rewriting program
// input schemas. Full evidence remains available through the exact run reader.
func taskStageFeedback(record WorkRunRecord, current model.WorkNodeAttempt, next *model.WorkNodeAttempt, detail string) {
	group, ok := taskGroup(*record.Run.Graph, current.Ref.NodeID)
	nextGroup, nextOK := taskGroup(*record.Run.Graph, next.Ref.NodeID)
	if !ok || !nextOK || group.ID != nextGroup.ID || next.Performer == nil {
		return
	}
	text := "\n\nTask stage context (recorded results; full evidence is available on work run " + string(record.Run.ID) + "):\n"
	for _, attempt := range record.Run.NodeAttempts {
		priorGroup, grouped := taskGroup(*record.Run.Graph, attempt.Ref.NodeID)
		if !grouped || priorGroup.ID != group.ID || attempt.Ref.ActivationID != current.Ref.ActivationID || !graphAttemptTerminal(attempt.State) || attempt.Detail == "" {
			continue
		}
		text += string(attempt.Ref.NodeID) + ": " + boundedStageFeedback(attempt.Detail) + "\n"
		if len(text) > 24<<10 {
			break
		}
	}
	if detail != "" {
		text += string(current.Ref.NodeID) + ": " + boundedStageFeedback(detail) + "\n"
	}
	performer := *next.Performer
	if performer.Agent != nil {
		agent := *performer.Agent
		agent.Brief += text
		performer.Agent = &agent
	}
	if performer.Human != nil {
		human := *performer.Human
		human.Prompt += text
		performer.Human = &human
	}
	next.Performer = &performer
}

func boundedStageFeedback(text string) string {
	if len(text) <= 4096 {
		return text
	}
	end := 4096
	for !utf8.RuneStart(text[end]) {
		end--
	}
	text = text[:end]
	return text + " [truncated; inspect the stage evidence for the full result]"
}

func taskCompletionWaived(graph model.WorkGraph, attempts []model.WorkNodeAttempt, current model.WorkNodeAttempt) bool {
	group, ok := taskGroup(graph, current.Ref.NodeID)
	if !ok {
		return false
	}
	latest := map[model.WorkNodeID]model.WorkNodeAttempt{}
	for _, a := range attempts {
		g, grouped := taskGroup(graph, a.Ref.NodeID)
		if grouped && g.ID == group.ID && a.Ref.ActivationID == current.Ref.ActivationID && a.Ref.Attempt > latest[a.Ref.NodeID].Ref.Attempt {
			latest[a.Ref.NodeID] = a
		}
	}
	for _, a := range latest {
		if a.State == model.NodeAttemptWaived {
			return true
		}
	}
	return false
}
