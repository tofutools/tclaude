package agentd

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// A step that fails with nothing in its process to handle it ends the run
// there (tk_82f97468). The run then says which step and why: its overseer is
// always told, and escalates as with a concern; the operator gets a Messages
// card only for a run they started or one with no agent overseer, so a fleet
// of agent-run processes does not flood them.

// processRunFailure is the step whose unhandled failure ended a run, or nil.
func processRunFailure(runID string) *engine.StepFailure {
	view, err := loadProcessRunView(runID)
	if err != nil {
		return nil
	}
	return view.Checkpoint.Failure
}

// notifyProcessRunFailed tells who should know that a run failed, naming the
// step and its reason.
func notifyProcessRunFailed(runID string) {
	view, err := loadProcessRunView(runID)
	if err != nil || view.Checkpoint.Failure == nil {
		return
	}
	failure := view.Checkpoint.Failure
	overseer, err := resolveProcessRunOverseer(runID)
	if err != nil {
		slog.Warn("process run failed: find its overseer", "run", runID, "error", err)
		return
	}
	name, step := processRunNames(runID, failure.NodeID)
	reason := sanitizeProcessText(failure.Reason)
	if reason == "" {
		reason = "no reason was given"
	}
	headline := fmt.Sprintf("run %s (%s) failed at step %s", runID, name, failure.NodeID)
	body := fmt.Sprintf("%s (run %s) failed at its step %s (attempt %d):\n\n%s\n\n"+
		"Nothing in the process handles this failure, so the run stopped there. "+
		"`tclaude process show %s` has the run; `tclaude process events %s` has its history.",
		name, runID, step, failure.Attempt, reason, runID, runID)
	if !overseer.operator() {
		conv, _ := db.CurrentConvForAgent(overseer.AgentID)
		if conv != "" {
			if _, err := queueAgentMessage(&db.AgentMessage{
				ToConv: conv, ToRecipients: []string{conv},
				Subject: fmt.Sprintf("[process] %s: %s", headline, processConcernHeadline(reason)),
				Body: body + "\n\nYou oversee this run. If the operator should decide what happens next, tell them " +
					"(`tclaude agent notify-human`).",
			}); err != nil {
				slog.Warn("process run failed: overseer mail", "run", runID, "error", err)
			}
		}
	}
	// A card the operator already holds on this step (a refused worker
	// spawn, say) tells them what stopped it; a second would only repeat it.
	if told, err := db.HasHumanMessageForProcessNode(runID, failure.NodeID); err == nil && told {
		return
	}
	if overseer.operator() || view.Scope.Owner == processOwnerOperator {
		m := &db.HumanMessage{
			FromTitle: "Process oversight", GroupName: view.Scope.Group,
			Subject:      fmt.Sprintf("Run %s failed at %s: %s", runID, failure.NodeID, processConcernHeadline(reason)),
			Body:         body,
			ProcessRunID: runID, ProcessNodeID: failure.NodeID,
		}
		id, err := db.InsertHumanMessage(m)
		if err != nil {
			slog.Warn("process run failed: operator card", "run", runID, "error", err)
			return
		}
		dispatchHumanMessageNotification("", m.FromTitle, m.GroupName, m.Subject, m.Body)
		mirrorHumanMessage(id)
	}
}

// processRunNames names a run's process and one of its steps for a person:
// the names their author gave, else the ids.
func processRunNames(runID, nodeID string) (process, step string) {
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
		if !strings.EqualFold(step, nodeID) {
			step = fmt.Sprintf("%s (%s)", step, nodeID)
		}
	}
	return process, step
}
