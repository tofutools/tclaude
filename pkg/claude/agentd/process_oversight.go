package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// process_oversight.go is run oversight (tk_e2fa2bd0). A process cannot
// encode every corner case, so every run has an overseer: the agent that
// answers when the run gets stuck or someone in it raises a concern. The
// overseer is the one set on the run, else the group's lead, else whoever
// started the run (the operator, for a run started from the dashboard).
//
// A concern climbs a chain until someone settles it:
//
//	overseer → the group's escalation agent (if one is configured) → operator
//
// An agent holding a concern answers it (continue, pause, escalate) within
// processConcernPatience; one that does not answer is passed over, and the
// run pauses while the next level decides. The operator gets a decision card
// in Messages and on the run, whose pick the daemon applies and posts back to
// the overseer. Everything is recorded in the run's evidence, so the run log
// shows who raised what, where it went and what was decided.

// processConcernPatience is how long an agent holding a concern has to answer
// before it passes to the next level. A variable so flow tests can shorten it.
var processConcernPatience = 30 * time.Minute

// processOversightNow is the oversight clock; flow tests move it.
var processOversightNow = time.Now

// processEscalationRole is the group member role that makes an agent the
// group's escalation agent: the second level of the chain, between the
// overseer and the operator. It also needs process.runs.manage.
const processEscalationRole = "escalation"

// processOversightActor is the evidence actor for what the oversight sweep
// does by itself: passing over a holder that did not answer.
const processOversightActor = "tclaude:oversight"

// processOverseer names a run's overseer. Ref is "agent:<id>" or
// processOwnerOperator; Via says why it is the overseer: set (on the run),
// lead (the group's lead) or starter (whoever started the run).
type processOverseer struct {
	Ref     string
	AgentID string
	Via     string
}

const (
	processOverseerViaSet     = "set"
	processOverseerViaLead    = "lead"
	processOverseerViaStarter = "starter"
)

func (o processOverseer) operator() bool { return o.AgentID == "" }

func processAgentRef(agentID string) string { return "agent:" + agentID }

// activeProcessAgent reports whether agentID is an active agent.
func activeProcessAgent(agentID string) bool {
	if agentID == "" {
		return false
	}
	a, err := db.GetAgent(agentID)
	return err == nil && a != nil && a.Active()
}

// processOverseerOfRef turns an actor ref into an overseer: an active agent,
// or the operator for anything else that is not an agent.
func processOverseerOfRef(ref, via string) (processOverseer, bool) {
	if id, ok := strings.CutPrefix(ref, "agent:"); ok {
		if !activeProcessAgent(id) {
			return processOverseer{}, false
		}
		return processOverseer{Ref: ref, AgentID: id, Via: via}, true
	}
	if ref == processOwnerOperator {
		return processOverseer{Ref: ref, Via: via}, true
	}
	return processOverseer{}, false
}

// resolveProcessRunOverseer finds a run's overseer. An explicit overseer that
// was retired since falls back to the default, so a run never waits on an
// agent that is gone.
func resolveProcessRunOverseer(runID string) (processOverseer, error) {
	set, ok, err := db.GetProcessRunOverseer(runID)
	if err != nil {
		return processOverseer{}, err
	}
	if ok {
		if o, ok := processOverseerOfRef(set.Overseer, processOverseerViaSet); ok {
			return o, nil
		}
	}
	scope, err := db.GetProcessRunScope(runID)
	if err != nil {
		return processOverseer{}, err
	}
	if scope.Kind == db.ProcessRunScopeGroup {
		if leads := processGroupLeads(processOversightMembers(scope.GroupID)); len(leads) > 0 {
			return processOverseer{Ref: processAgentRef(leads[0].AgentID), AgentID: leads[0].AgentID, Via: processOverseerViaLead}, nil
		}
	}
	if o, ok := processOverseerOfRef(scope.Owner, processOverseerViaStarter); ok {
		return o, nil
	}
	// A run started by a linked node or an agent since retired: the operator.
	return processOverseer{Ref: processOwnerOperator, Via: processOverseerViaStarter}, nil
}

// processEscalationAgentOf is the group's escalation agent for a group-scoped
// run: the first active member with the escalation role that holds
// process.runs.manage. "" when the group has none (or the run has no group).
func processEscalationAgentOf(runID string) string {
	scope, err := db.GetProcessRunScope(runID)
	if err != nil || scope.Kind != db.ProcessRunScopeGroup {
		return ""
	}
	for _, member := range processOversightMembers(scope.GroupID) {
		if !strings.EqualFold(member.Role, processEscalationRole) {
			continue
		}
		if resolvePermission(member.ConvID, PermProcessRunsManage) == permAllow {
			return member.AgentID
		}
	}
	return ""
}

// processOversightMembers are a group's active members with their roles and
// ownership, from the database alone: the run view and the context read
// resolve the overseer on every poll, so this must not ask tmux who is
// online the way performer resolution does.
func processOversightMembers(groupID int64) []processPerformerCandidate {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return nil
	}
	var out []processPerformerCandidate
	for _, member := range members {
		agentID := peerAgentID(member.ConvID)
		if !activeProcessAgent(agentID) {
			continue
		}
		owner, _ := db.IsAgentGroupOwner(groupID, member.ConvID)
		out = append(out, processPerformerCandidate{AgentID: agentID, ConvID: member.ConvID, Role: member.Role, Owner: owner})
	}
	return out
}

// processConcernLevels is the escalation chain, in order.
var processConcernLevels = []string{db.ProcessConcernLevelOverseer, db.ProcessConcernLevelEscalation, db.ProcessConcernLevelOperator}

// processConcernStep is where a concern goes next: a level and its holder.
type processConcernStep struct {
	Level   string
	Holder  string // "agent:<id>", "" at the operator
	AgentID string
}

// nextProcessConcernStep walks the chain from the level after `after` ("" for
// the start) to the first level that has someone to hold the concern, other
// than the agents in skip (whoever raised or passed it on). The operator ends
// the chain and always holds.
func nextProcessConcernStep(runID, after string, skip ...string) processConcernStep {
	start := 0
	for i, level := range processConcernLevels {
		if level == after {
			start = i + 1
		}
	}
	skipped := func(agentID string) bool {
		for _, s := range skip {
			if s != "" && s == agentID {
				return true
			}
		}
		return false
	}
	for _, level := range processConcernLevels[start:] {
		switch level {
		case db.ProcessConcernLevelOverseer:
			o, err := resolveProcessRunOverseer(runID)
			if err == nil && !o.operator() && !skipped(o.AgentID) {
				return processConcernStep{Level: level, Holder: o.Ref, AgentID: o.AgentID}
			}
		case db.ProcessConcernLevelEscalation:
			if id := processEscalationAgentOf(runID); id != "" && !skipped(id) {
				return processConcernStep{Level: level, Holder: processAgentRef(id), AgentID: id}
			}
		}
	}
	return processConcernStep{Level: db.ProcessConcernLevelOperator}
}

// processConcernMu serializes concern placement, so a raise, an answer and the
// sweep never deliver one concern twice.
var processConcernMu sync.Mutex

// raiseProcessConcern records a concern and hands it to the first level that
// can take it. An agent's own concern skips itself: an overseer raising one is
// escalating, and so on up.
func raiseProcessConcern(runID, nodeID, source, signal, raisedBy, reason string) (db.ProcessRunConcern, error) {
	processConcernMu.Lock()
	defer processConcernMu.Unlock()
	raiser, _ := strings.CutPrefix(raisedBy, "agent:")
	step := nextProcessConcernStep(runID, "", raiser)
	c := db.ProcessRunConcern{
		RunID: runID, NodeID: nodeID, Source: source, Signal: signal, RaisedBy: raisedBy,
		Reason: reason, Level: step.Level, Holder: step.Holder,
	}
	if step.AgentID != "" {
		c.AnswerBy = processOversightNow().Add(processConcernPatience)
	}
	c, err := db.RaiseProcessRunConcern(c)
	if err != nil {
		return db.ProcessRunConcern{}, err
	}
	return deliverProcessConcern(c, ""), nil
}

// escalateProcessConcern moves a concern from its current level to the next
// one that can take it. by is the actor moving it (the holder, or the
// oversight sweep), why the note shown to the next holder.
func escalateProcessConcern(c db.ProcessRunConcern, by, why string) (db.ProcessRunConcern, error) {
	processConcernMu.Lock()
	defer processConcernMu.Unlock()
	if c.Level == db.ProcessConcernLevelOperator {
		return db.ProcessRunConcern{}, fmt.Errorf("%w: the concern is with the operator already", db.ErrProcessConcernClosed)
	}
	holder, _ := strings.CutPrefix(c.Holder, "agent:")
	raiser, _ := strings.CutPrefix(c.RaisedBy, "agent:")
	step := nextProcessConcernStep(c.RunID, c.Level, holder, raiser)
	var answerBy time.Time
	if step.AgentID != "" {
		answerBy = processOversightNow().Add(processConcernPatience)
	}
	moved, err := db.EscalateProcessRunConcern(c.RunID, c.ID, db.ProcessConcernEscalation{
		Level: step.Level, Holder: step.Holder, AnswerBy: answerBy, Why: why, Actor: by, FromLevel: c.Level,
	})
	if err != nil {
		return db.ProcessRunConcern{}, err
	}
	return deliverProcessConcern(moved, why), nil
}

// deliverProcessConcern tells the concern's holder: mail to an agent, a
// decision card to the operator. Delivery is best-effort; the concern is
// recorded either way and shows on the run.
func deliverProcessConcern(c db.ProcessRunConcern, why string) db.ProcessRunConcern {
	if c.Level == db.ProcessConcernLevelOperator {
		id, err := postProcessConcernCard(c, why)
		if err != nil {
			slog.Warn("process oversight: operator card failed", "run", c.RunID, "concern", c.ID, "error", err)
			return c
		}
		if err := db.SetProcessRunConcernMessage(c.RunID, c.ID, id); err != nil {
			slog.Warn("process oversight: record operator card", "run", c.RunID, "concern", c.ID, "error", err)
		}
		c.HumanMessageID = id
		return c
	}
	agentID, _ := strings.CutPrefix(c.Holder, "agent:")
	conv, _ := db.CurrentConvForAgent(agentID)
	if conv == "" {
		return c
	}
	if _, err := queueAgentMessage(&db.AgentMessage{
		ToConv: conv, ToRecipients: []string{conv},
		Subject: fmt.Sprintf("[process] concern #%d on %s: %s", c.ID, c.RunID, processConcernHeadline(c.Reason)),
		Body:    processConcernMail(c, why),
	}); err != nil {
		slog.Warn("process oversight: concern mail failed", "run", c.RunID, "concern", c.ID, "error", err)
	}
	return c
}

// processConcernHeadline is a reason's first line, clipped for a subject.
func processConcernHeadline(reason string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	return clipProjectionText(line, 80)
}

// processActorName names an actor ref for a human: an agent's title, the
// operator, or the engine.
func processActorName(ref string) string {
	switch {
	case ref == processOwnerOperator:
		return "the operator"
	case ref == processOversightActor || strings.HasPrefix(ref, "tclaude:"):
		return "the engine"
	}
	if id, ok := strings.CutPrefix(ref, "agent:"); ok {
		if conv, _ := db.CurrentConvForAgent(id); conv != "" {
			if title := agent.FreshTitle(conv); title != "" {
				return title
			}
		}
		return id
	}
	return ref
}

func processConcernMail(c db.ProcessRunConcern, why string) string {
	var b strings.Builder
	role := "You oversee this run"
	if c.Level == db.ProcessConcernLevelEscalation {
		role = "You are this group's escalation agent"
	}
	fmt.Fprintf(&b, "%s, and a concern needs your answer.\n\n", role)
	fmt.Fprintf(&b, "Run:     %s\nConcern: #%d\n", c.RunID, c.ID)
	if c.NodeID != "" {
		fmt.Fprintf(&b, "Step:    %s\n", c.NodeID)
	}
	fmt.Fprintf(&b, "From:    %s\n\n%s\n", processActorName(c.RaisedBy), sanitizeProcessText(c.Reason))
	if why != "" {
		fmt.Fprintf(&b, "\nPassed on: %s\n", sanitizeProcessText(why))
	}
	fmt.Fprintf(&b, "\nAnswer it with one of:\n"+
		"  tclaude process answer-concern %[1]s %[2]d continue --note \"…\"   the run goes on (and resumes, if paused)\n"+
		"  tclaude process answer-concern %[1]s %[2]d pause --note \"…\"      hold the run\n"+
		"  tclaude process answer-concern %[1]s %[2]d escalate --note \"…\"   pass it up the chain\n"+
		"See the run with `tclaude process show %[1]s` and its log with `tclaude process events %[1]s`.\n",
		c.RunID, c.ID)
	if !c.AnswerBy.IsZero() {
		fmt.Fprintf(&b, "\nIf nobody answers by %s, the run pauses and the concern passes up the chain.\n",
			c.AnswerBy.Local().Format("15:04"))
	}
	return b.String()
}

// sweepProcessConcerns passes over holders that did not answer in time: the
// run pauses (if it is not paused already) and the concern goes up a level.
func sweepProcessConcerns() {
	due, err := db.ListDueProcessRunConcerns(processOversightNow(), 32)
	if err != nil {
		slog.Warn("process oversight: due concerns unreadable", "error", err)
		return
	}
	for _, c := range due {
		why := fmt.Sprintf("%s did not answer within %s", processActorName(c.Holder), processConcernPatience.Round(time.Minute))
		if err := pauseProcessRunFor(c.RunID, processOversightActor); err != nil {
			slog.Warn("process oversight: pause an unanswered run", "run", c.RunID, "error", err)
		}
		if _, err := escalateProcessConcern(c, processOversightActor, why); err != nil && !errors.Is(err, db.ErrProcessConcernClosed) {
			slog.Warn("process oversight: pass over an unanswered holder", "run", c.RunID, "concern", c.ID, "error", err)
		}
	}
}

// pauseProcessRunFor pauses a run as actor, ordered against planning like an
// operator's pause.
func pauseProcessRunFor(runID, actor string) error {
	processRunPauseGate.Lock()
	_, changed, err := db.SetProcessRunPaused(runID, true, actor)
	processRunPauseGate.Unlock()
	if err != nil {
		return err
	}
	if changed {
		kickProcessAgentDispatch(runID)
	}
	return nil
}

// resumeProcessRunFor releases a run's pause as actor and starts it again.
func resumeProcessRunFor(runID, actor string) error {
	unpaused, err := releaseProcessRunPause(runID, actor)
	if err != nil {
		return err
	}
	_, err = resumeProcessRun(runID, unpaused)
	return err
}
