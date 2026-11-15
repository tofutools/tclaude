package agentd

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// processRunGateView is one agreement gate collecting votes: the tally so far,
// in the shape "2/3 agreed, waiting on rita".
type processRunGateView struct {
	NodeID  string                   `json:"nodeId"`
	Agree   string                   `json:"agree"`
	Agreed  int                      `json:"agreed"`
	Total   int                      `json:"total"`
	Votes   []processRunGateVoteView `json:"votes"`
	Summary string                   `json:"summary"`
}

// processRunGateVoteView is one decider's vote: who (an agent's name, or
// "human"), and the outcome chosen, empty while it is still out.
type processRunGateVoteView struct {
	NodeID  string `json:"nodeId"`
	Decider string `json:"decider"`
	Verdict string `json:"verdict,omitempty"`
}

// processRunMayHaveGates is a cheap test for whether a run's definition is
// worth preparing for its gates: some node is a gate's tally.
func processRunMayHaveGates(checkpoint engine.Checkpoint) bool {
	if checkpoint.Status != engine.RunRunning {
		return false
	}
	for id := range checkpoint.Nodes {
		if strings.HasSuffix(id, ".tally") {
			return true
		}
	}
	return false
}

// processRunGateViews lists the gates whose votes are open: the fork went out
// and the tally has not yet taken them on.
func processRunGateViews(definition *engine.Definition, checkpoint engine.Checkpoint,
	performers []processRunPerformerView) []processRunGateView {
	if checkpoint.Status != engine.RunRunning {
		return nil
	}
	var views []processRunGateView
	for _, gate := range definition.Gates(checkpoint) {
		if checkpoint.Nodes[gate.TallyID] != engine.NodePending {
			continue
		}
		open := false
		view := processRunGateView{NodeID: gate.NodeID, Agree: gate.Agree, Total: len(gate.Votes)}
		var waiting, dissents []string
		for _, vote := range gate.Votes {
			status := checkpoint.Nodes[vote.NodeID]
			if status != engine.NodePending {
				open = true
			}
			decider := processGateDecider(vote.NodeID, performers)
			view.Votes = append(view.Votes, processRunGateVoteView{NodeID: vote.NodeID, Decider: decider, Verdict: vote.Verdict})
			switch vote.Verdict {
			case "":
				waiting = append(waiting, decider)
			case gate.Agree:
				view.Agreed++
			default:
				dissents = append(dissents, fmt.Sprintf("%s chose %s", decider, vote.Verdict))
			}
		}
		if !open {
			continue
		}
		parts := []string{fmt.Sprintf("%d/%d agreed", view.Agreed, view.Total)}
		parts = append(parts, dissents...)
		if len(waiting) > 0 {
			parts = append(parts, "waiting on "+strings.Join(waiting, ", "))
		}
		view.Summary = strings.Join(parts, ", ")
		views = append(views, view)
	}
	return views
}

// processGateDecider names a vote's decider: the agent its latest attempt went
// to, or "human" for a person's vote.
func processGateDecider(voteID string, performers []processRunPerformerView) string {
	name := ""
	found := false
	for _, performer := range performers {
		if performer.NodeID != voteID {
			continue
		}
		found = true
		if performer.Name != "" {
			name = performer.Name
		} else if performer.AgentID != "" {
			name = performer.AgentID
		}
	}
	switch {
	case name != "":
		return name
	case found:
		return "an agent not yet chosen"
	default:
		return "human"
	}
}

// processAgreementFeedback is what the performer of a round an agreement gate sent back is
// told: what each decider voted, with its notes. Empty when tallyID is not a
// gate's tally.
func processAgreementFeedback(runID, tallyID string, round, rounds int) string {
	run, err := executor.LoadRun(runID)
	if err != nil {
		slog.Warn("process gates: run not loaded", "run", runID, "error", err)
		return ""
	}
	gate, ok := run.GateOfTally(tallyID)
	if !ok {
		return ""
	}
	gateID := gate.NodeID
	rows, _ := db.ListProcessAgentDispatchesForRun(runID)
	decisions, _ := db.ListLatestProcessRunNodeEvents(runID, "decision_recorded")
	var b strings.Builder
	fmt.Fprintf(&b, "## Sent back for round %d of %d\n\nThe deciders of %q did not all choose %s, so this work runs again. What each decided:\n",
		round, rounds, gateID, gate.Agree)
	for k, vote := range gate.Votes {
		decider, verdict, note := "human", "", ""
		// An agent's vote is its report; a person's is the decision row.
		for _, row := range rows {
			if row.NodeID == vote.NodeID && row.ReportVerdict != "" {
				decider, verdict, note = processAgentName(row.AgentID), row.ReportVerdict, row.ReportNote
			}
		}
		if verdict == "" {
			for _, event := range decisions {
				if event.NodeID != vote.NodeID {
					continue
				}
				// The executor's decisionEvidencePayload, field for field: the
				// payload decode is strict.
				var payload struct {
					Verdict    string             `json:"verdict"`
					Evidence   string             `json:"evidence,omitempty"`
					ChosenEdge *engine.ChosenEdge `json:"chosenEdge,omitempty"`
					Attempt    int                `json:"attempt,omitempty"`
				}
				if event.DecodePayload(&payload) == nil {
					verdict, note = payload.Verdict, payload.Evidence
				}
			}
		}
		fmt.Fprintf(&b, "\n%d. %s: %s", k+1, decider, verdict)
		if strings.TrimSpace(note) != "" {
			fmt.Fprintf(&b, "\n%s", indentProcessNote(note))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// processAgentName is an agent's current title, or its id.
func processAgentName(agentID string) string {
	if performer, _ := db.GetAgent(agentID); performer.Active() {
		if conv, _ := db.CurrentConvForAgent(agentID); conv != "" {
			if title := agent.FreshTitle(conv); title != "" {
				return title
			}
		}
	}
	return agentID
}

func indentProcessNote(note string) string {
	lines := strings.Split(strings.TrimSpace(note), "\n")
	for i, line := range lines {
		lines[i] = "   " + line
	}
	return strings.Join(lines, "\n")
}
