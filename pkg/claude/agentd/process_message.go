package agentd

// A message step sends a message and goes on (model.PerformerMessage): to
// the operator (a Messages card, with the desktop and away notifications),
// or to agents of the run's group (an inbox message each). The executor
// performs it like a program, through the sender installed here; it passes
// once every recipient has the message, and fails naming who it could not
// reach otherwise.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

func init() {
	executor.SetMessageSender(sendProcessMessage)
}

// processMessageKeyOperator is the delivery key of the operator.
const processMessageKeyOperator = "operator"

// processMessageTarget is one recipient a message goes to: the operator, or
// an agent (Key its id) of the run's group.
type processMessageTarget struct {
	Key  string
	Name string
	Conv string
}

// processMessageReport is the stored shape of a message_sent event this
// sender reads back: its outcome and what it delivered.
type processMessageReport struct {
	Observation engine.ProgramObservation `json:"observation"`
	Message     *executor.MessageReport   `json:"message"`
}

func sendProcessMessage(_ context.Context, runID string, command engine.Command) (executor.Result, error) {
	result := executor.Result{
		StartedAt: time.Now().UTC(),
		Observation: engine.ProgramObservation{
			CommandID: command.ID, NodeID: command.NodeID, Outcome: engine.ProgramFailed, ExitCode: -1,
		},
	}
	fail := func(reason string) (executor.Result, error) {
		result.FinishedAt = time.Now().UTC()
		result.Observation.Error = reason
		return result, nil
	}
	message := command.Message
	if message == nil {
		return fail("the message step has no message")
	}
	scope, err := processRunScopeOf(runID)
	if err != nil {
		return fail("read the run's scope: " + err.Error())
	}
	process, step := processMessageNames(runID, command.NodeID)
	values := processMessageOutputs(runID, command.NodeID)
	subject := strings.TrimSpace(model.InterpolateCaptures(message.Subject, values))
	if subject == "" {
		subject = fmt.Sprintf("%s: %s", process, step)
	}
	// Outputs are filled in now and may be long: what is sent stays within
	// the bounds the step's own text has, so its record always fits.
	subject = clipProcessMessage(strings.Join(strings.Fields(subject), " "), model.MaxMessageSubjectBytes)
	text := clipProcessMessage(model.InterpolateCaptures(message.Text, values), processMessageSentMax)
	report := &executor.MessageReport{Subject: subject, Text: text, Round: command.Round}
	result.Message = report
	done := processMessageDelivered(runID, command)
	var group *db.AgentGroup
	if scope.GroupID != 0 {
		group, _ = db.GetAgentGroupByID(scope.GroupID)
	}
	groupName := ""
	if group != nil {
		groupName = group.Name
	}
	from := fmt.Sprintf("process %s", process)
	var problems []string
	for _, to := range message.To {
		targets, err := processMessageTargets(scope, group, to)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", to, err))
			continue
		}
		for _, target := range targets {
			delivery := executor.MessageDelivery{To: to, Key: target.Key, Name: target.Name}
			// Recorded as each delivery is made, so a resume (after a crash, or
			// a result that could not be committed) skips who already has it.
			if !done[target.Key] {
				if had, err := db.ProcessMessageDelivered(command.ID, target.Key); err == nil && had {
					done[target.Key] = true
				}
			}
			if done[target.Key] || slices.ContainsFunc(report.Delivered, func(d executor.MessageDelivery) bool { return d.Key == target.Key }) {
				// Already has it: from an earlier attempt, or through another
				// recipient of this one (the lead is also in the group).
				if !slices.ContainsFunc(report.Delivered, func(d executor.MessageDelivery) bool { return d.Key == target.Key }) {
					report.Delivered = append(report.Delivered, delivery)
				}
				continue
			}
			if err := deliverProcessMessage(runID, command, groupName, from, subject, text, target); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", to, err))
				continue
			}
			if err := db.RecordProcessMessageDelivery(runID, command.ID, target.Key); err != nil {
				slog.Warn("process message: record a delivery", "run", runID, "node", command.NodeID, "to", target.Key, "error", err)
			}
			report.Delivered = append(report.Delivered, delivery)
		}
	}
	if len(problems) > 0 {
		return fail("could not send the message to " + strings.Join(problems, "; "))
	}
	result.FinishedAt = time.Now().UTC()
	result.Dispatched = true
	result.Observation.Outcome = engine.ProgramSucceeded
	result.Observation.ExitCode = 0
	return result, nil
}

// processMessageSentMax bounds a message's text as sent, its outputs filled
// in: twice what its author may write, which leaves room for an output or two
// and keeps the message_sent record well inside an event's bound.
const processMessageSentMax = 2 * model.MaxMessageTextBytes

// clipProcessMessage cuts text to at most limit bytes, on a rune boundary,
// saying it was cut.
func clipProcessMessage(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const cut = " …(cut)"
	end := limit - len(cut)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + cut
}

// processMessageTargets resolves one authored recipient in the run's scope:
// the operator always; the lead, the whole group or a role among the run
// group's members; an agent by name or id when it is one of them (or, in a
// run scoped to an agent, that agent).
func processMessageTargets(scope processRunScope, group *db.AgentGroup, to string) ([]processMessageTarget, error) {
	if to == model.MessageToOperator {
		return []processMessageTarget{{Key: processMessageKeyOperator, Name: "you"}}, nil
	}
	var members []processPerformerCandidate
	if group != nil {
		members = processOversightMembers(group.ID)
	}
	agentTarget := func(candidate processPerformerCandidate) processMessageTarget {
		return processMessageTarget{Key: candidate.AgentID, Name: processAgentName(candidate.AgentID), Conv: candidate.ConvID}
	}
	pick := func(chosen []processPerformerCandidate, none string) ([]processMessageTarget, error) {
		if len(chosen) == 0 {
			return nil, fmt.Errorf("%s", none)
		}
		targets := make([]processMessageTarget, 0, len(chosen))
		for _, candidate := range chosen {
			targets = append(targets, agentTarget(candidate))
		}
		return targets, nil
	}
	if want, ok := strings.CutPrefix(to, model.MessageToAgentPrefix); ok {
		for _, member := range members {
			if member.AgentID == want || strings.EqualFold(processAgentName(member.AgentID), want) {
				return []processMessageTarget{agentTarget(member)}, nil
			}
		}
		if group == nil && scope.AgentID != "" && (scope.AgentID == want || strings.EqualFold(processAgentName(scope.AgentID), want)) {
			if conv, _ := db.CurrentConvForAgent(scope.AgentID); conv != "" {
				return []processMessageTarget{{Key: scope.AgentID, Name: processAgentName(scope.AgentID), Conv: conv}}, nil
			}
		}
		return nil, fmt.Errorf("no agent %s in the run's group", want)
	}
	if group == nil {
		return nil, fmt.Errorf("the run has no group to send to")
	}
	switch to {
	case model.MessageToLead:
		return pick(processGroupLeads(members), fmt.Sprintf("%s has no lead (a member with role lead, or an owner)", group.Name))
	case model.MessageToGroup:
		return pick(members, fmt.Sprintf("%s has no active members", group.Name))
	}
	if role, ok := strings.CutPrefix(to, model.MessageToRolePrefix); ok {
		var matching []processPerformerCandidate
		for _, member := range members {
			if strings.EqualFold(member.Role, role) {
				matching = append(matching, member)
			}
		}
		return pick(matching, fmt.Sprintf("no member of %s has role %s", group.Name, role))
	}
	return nil, fmt.Errorf("unknown recipient")
}

// deliverProcessMessage writes the message where its recipient reads it: a
// Messages card for the operator, an inbox message for an agent. The row is
// the delivery; notifications and the pane nudge follow on their own.
func deliverProcessMessage(runID string, command engine.Command, groupName, from, subject, text string, target processMessageTarget) error {
	footer := fmt.Sprintf("\n\n— sent by %s, step %s (run %s)", strings.TrimPrefix(from, "process "), command.NodeID, runID)
	if target.Key == processMessageKeyOperator {
		m := &db.HumanMessage{
			FromTitle: from, GroupName: groupName, Subject: subject, Body: text + footer,
			ProcessRunID: runID, ProcessNodeID: command.NodeID,
		}
		id, err := db.InsertHumanMessage(m)
		if err != nil {
			return err
		}
		dispatchHumanMessageNotification("", m.FromTitle, m.GroupName, m.Subject, m.Body)
		mirrorHumanMessage(id)
		return nil
	}
	if target.Conv == "" {
		return fmt.Errorf("%s has no conversation to deliver to", target.Name)
	}
	_, err := queueAgentMessage(&db.AgentMessage{
		ToConv: target.Conv, ToRecipients: []string{target.Conv},
		Subject: "[" + from + "] " + subject, Body: text + footer,
	})
	return err
}

// processMessageDelivered is who the step's previous attempt already
// reached in this round, for a retry to skip. A round of a loop, or a
// send-back over the step, sends again.
func processMessageDelivered(runID string, command engine.Command) map[string]bool {
	events, err := db.ListLatestProcessRunNodeEvents(runID, "message_sent")
	if err != nil {
		return nil
	}
	rewound := processRunRewoundAt(runID)
	done := map[string]bool{}
	for _, event := range events {
		if event.NodeID != command.NodeID {
			continue
		}
		if at, ok := rewound[event.NodeID]; ok && !event.OccurredAt.After(at) {
			continue
		}
		var report processMessageReport
		if json.Unmarshal(event.PayloadJSON, &report) != nil || report.Message == nil || report.Message.Round != command.Round ||
			report.Observation.CommandID == command.ID {
			continue
		}
		for _, delivery := range report.Message.Delivered {
			done[delivery.Key] = true
		}
	}
	return done
}

// processMessageOutputs reads the step outputs a message names, as
// reported: a message carries a value as is, to a person or an agent alike.
func processMessageOutputs(runID, nodeID string) func(name string) (string, bool) {
	rows, err := db.ListProcessAgentDispatchesForRun(runID)
	if err != nil {
		rows = nil
	}
	scope := processNodeScope(nodeID)
	rewound := processRunRewoundAt(runID)
	return func(name string) (string, bool) {
		var latest *db.ProcessAgentDispatch
		for i := range rows {
			row := &rows[i]
			if _, ok := row.ReportOutputs[name]; !ok || row.ReportOutcome != string(engine.ProgramSucceeded) ||
				processNodeScope(row.NodeID) != scope {
				continue
			}
			if at, ok := rewound[row.NodeID]; ok && !row.ReportedAt.After(at) {
				continue
			}
			if latest == nil || row.ReportedAt.After(latest.ReportedAt) {
				latest = row
			}
		}
		if latest == nil {
			return fmt.Sprintf("(%s: none reported yet)", name), true
		}
		return latest.ReportOutputs[name], true
	}
}

// processMessageNames names the run's process and the step for a message's
// default subject: the names their author gave, else the ids.
func processMessageNames(runID, nodeID string) (process, step string) {
	process, step = processRunTemplateName(runID), nodeID
	record, err := db.GetProcessRun(runID)
	if err != nil || record == nil {
		return process, step
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return process, step
	}
	if name := strings.TrimSpace(tmpl.Name); name != "" {
		process = sanitizeProcessText(name)
	}
	if node, ok := tmpl.Nodes[nodeID]; ok && strings.TrimSpace(node.Name) != "" {
		step = sanitizeProcessText(strings.TrimSpace(node.Name))
	}
	return process, step
}
