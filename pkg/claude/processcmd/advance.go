package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	processplan "github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type advanceParams struct {
	RunID       string `pos:"true" help:"Process run id to advance"`
	NodeID      string `pos:"true" help:"Node id to advance"`
	StoreRoot   string `long:"store-root" help:"Filesystem process store root"`
	Verdict     string `long:"verdict" help:"Manual verdict: pass or fail, or a decision edge label"`
	EvidenceRef string `long:"evidence" optional:"true" help:"Evidence artifact/reference to attach to the event"`
	Actor       string `long:"actor" optional:"true" help:"Actor ref, e.g. human:johan, agent:agt_..., program:cmd@exit0"`
}

func advanceCmd() *cobra.Command {
	return boa.CmdT[advanceParams]{
		Use:         "advance",
		Short:       "Manually advance a process node",
		Long:        "Manually advance a process node by appending reducer events to the run evidence log.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(2),
		PreExecuteFunc: func(p *advanceParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			if strings.TrimSpace(p.Verdict) == "" {
				return fmt.Errorf("--verdict is required")
			}
			return nil
		},
		RunFunc: func(p *advanceParams, cmd *cobra.Command, _ []string) {
			exitWithError(runAdvance(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runAdvance(cmd *cobra.Command, p *advanceParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	if err := ensureRunVerifies(cmd.Context(), fs, p.RunID, out); err != nil {
		return err
	}
	snapshot, err := fs.LoadRun(cmd.Context(), p.RunID)
	if err != nil {
		return err
	}
	if snapshot.State.Status == state.RunStatusCompleted || snapshot.State.Status == state.RunStatusFailed || snapshot.State.Status == state.RunStatusCanceled {
		return fmt.Errorf("run %q is %s and cannot be advanced", p.RunID, snapshot.State.Status)
	}
	tmpl, err := fs.GetTemplate(cmd.Context(), snapshot.Run.TemplateRef)
	if err != nil {
		return err
	}
	actor := state.ActorRef(strings.TrimSpace(p.Actor))
	if actor == "" {
		actor = defaultActor()
	}
	if !state.ValidateActorRef(actor) {
		return fmt.Errorf("invalid actor %q; use human:<id>, agent:agt_<id>, or program:<cmd>@exit<code>", actor)
	}
	entries, err := planAdvance(snapshot, tmpl, p.NodeID, strings.TrimSpace(p.Verdict), actor, strings.TrimSpace(p.EvidenceRef))
	if err != nil {
		return err
	}
	result, err := fs.Append(cmd.Context(), p.RunID, snapshot.State.LastLogSeq, entries)
	if err != nil {
		if store.IsConflict(err) {
			return fmt.Errorf("%w; reload the run and retry", err)
		}
		return err
	}
	fmt.Fprintf(out, "Advanced run %s node %s to seq %d\n", p.RunID, p.NodeID, result.State.LastLogSeq)
	return nil
}

func planAdvance(snapshot store.Snapshot, tmpl *model.Template, nodeID, verdict string, actor state.ActorRef, evidenceRef string) ([]evidence.LogEntry, error) {
	node, ok := snapshot.State.Nodes[nodeID]
	if !ok {
		return nil, fmt.Errorf("node %q is not in run state", nodeID)
	}
	if node.Status == state.NodeStatusCompleted || node.Status == state.NodeStatusSkipped {
		return nil, fmt.Errorf("node %q is already %s", nodeID, node.Status)
	}
	if node.Status == state.NodeStatusFailed {
		return nil, fmt.Errorf("node %q is failed and cannot be advanced", nodeID)
	}
	templateNode, ok := tmpl.Nodes[nodeID]
	if !ok {
		return nil, fmt.Errorf("node %q is not in template", nodeID)
	}
	if node.Status != state.NodeStatusReady {
		return nil, fmt.Errorf("node %q is %s; only ready nodes can be advanced manually", nodeID, node.Status)
	}
	at := processNow().UTC()
	switch templateNode.Type {
	case model.NodeTypeTask:
		return planTaskAdvance(snapshot, tmpl, nodeID, node, templateNode, verdict, actor, evidenceRef, at)
	case model.NodeTypeDecision:
		return planDecisionAdvance(snapshot, tmpl, nodeID, templateNode, verdict, actor, evidenceRef, at)
	case model.NodeTypeWait, model.NodeTypeStart:
		if !processplan.IsPassVerdict(verdict) {
			return nil, fmt.Errorf("node %q of type %s only accepts pass verdict", nodeID, templateNode.Type)
		}
		entries := []evidence.LogEntry{
			nodeLogEntry(nodeID, evidence.EntryKindGate, state.Event{Type: state.EventNodeStatusSet, NodeStatus: state.NodeStatusCompleted}, evidenceRef, at),
		}
		return appendActivationEntries(entries, snapshot, tmpl, processplan.ResolvePassEdge(templateNode.Next, verdict), evidenceRef, at)
	case model.NodeTypeEnd:
		if !processplan.IsPassVerdict(verdict) {
			return nil, fmt.Errorf("end node %q only accepts pass verdict", nodeID)
		}
		return []evidence.LogEntry{
			nodeLogEntry(nodeID, evidence.EntryKindGate, state.Event{Type: state.EventNodeStatusSet, NodeStatus: state.NodeStatusCompleted}, evidenceRef, at),
			runLogEntry(evidence.EntryKindGate, state.Event{Type: state.EventRunStatusSet, RunStatus: processplan.TerminalRunStatus(templateNode)}, evidenceRef, at),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported node type %q", templateNode.Type)
	}
}

func planTaskAdvance(snapshot store.Snapshot, tmpl *model.Template, nodeID string, node state.NodeState, templateNode model.Node, verdict string, actor state.ActorRef, evidenceRef string, at time.Time) ([]evidence.LogEntry, error) {
	normalized := strings.ToLower(strings.TrimSpace(verdict))
	if normalized != "pass" && normalized != "fail" {
		return nil, fmt.Errorf("task node %q verdict must be pass or fail", nodeID)
	}
	attempt := node.Attempt + 1
	entries := []evidence.LogEntry{
		nodeLogEntry(nodeID, evidence.EntryKindAttempt, state.Event{
			Type:    state.EventNodeAttemptStarted,
			Actor:   actor,
			Attempt: attempt,
		}, evidenceRef, at),
	}
	if normalized == "fail" && processplan.SettleNodeStatus(normalized, attempt, templateNode.Retry) == state.NodeStatusReady {
		entries = append(entries, nodeLogEntry(nodeID, evidence.EntryKindAttempt, state.Event{
			Type:       state.EventNodeAttemptSettled,
			Outcome:    "fail",
			NodeStatus: state.NodeStatusReady,
		}, evidenceRef, at))
		return entries, nil
	}
	status := processplan.SettleNodeStatus(normalized, attempt, templateNode.Retry)
	entries = append(entries, nodeLogEntry(nodeID, evidence.EntryKindAttempt, state.Event{
		Type:       state.EventNodeAttemptSettled,
		Outcome:    normalized,
		NodeStatus: status,
	}, evidenceRef, at))
	target := processplan.ResolvePassEdge(templateNode.Next, normalized)
	if normalized == "fail" {
		target = processplan.ResolveFailEdge(templateNode.Next, templateNode.Retry)
	}
	if normalized == "fail" && target == "" {
		entries = append(entries, runLogEntry(evidence.EntryKindGate, state.Event{Type: state.EventRunStatusSet, RunStatus: state.RunStatusFailed}, evidenceRef, at))
		return entries, nil
	}
	return appendActivationEntries(entries, snapshot, tmpl, target, evidenceRef, at)
}

func planDecisionAdvance(snapshot store.Snapshot, tmpl *model.Template, nodeID string, templateNode model.Node, verdict string, actor state.ActorRef, evidenceRef string, at time.Time) ([]evidence.LogEntry, error) {
	target, ok := processplan.DecisionEdge(templateNode.Next, verdict)
	if !ok {
		return nil, fmt.Errorf("decision node %q has no edge for verdict %q; available edges: %s", nodeID, verdict, strings.Join(sortedEdgeKeys(templateNode.Next), ", "))
	}
	entries := []evidence.LogEntry{
		nodeLogEntry(nodeID, evidence.EntryKindDecision, state.Event{
			Type:       state.EventDecisionRecorded,
			Actor:      actor,
			ChosenEdge: verdict,
			Decision: &state.DecisionRecord{
				Actor:       actor,
				Verdict:     verdict,
				EvidenceRef: evidenceRef,
				Timestamp:   at,
			},
		}, evidenceRef, at),
	}
	return appendActivationEntries(entries, snapshot, tmpl, target, evidenceRef, at)
}

func appendActivationEntries(entries []evidence.LogEntry, snapshot store.Snapshot, tmpl *model.Template, targetNodeID, evidenceRef string, at time.Time) ([]evidence.LogEntry, error) {
	if strings.TrimSpace(targetNodeID) == "" {
		return entries, nil
	}
	target, ok := snapshot.State.Nodes[targetNodeID]
	if !ok {
		return nil, fmt.Errorf("target node %q is not in run state", targetNodeID)
	}
	targetTemplate, ok := tmpl.Nodes[targetNodeID]
	if !ok {
		return nil, fmt.Errorf("target node %q is not in template", targetNodeID)
	}
	if target.Status != state.NodeStatusPending {
		return entries, nil
	}
	status := state.NodeStatusReady
	if targetTemplate.Type == model.NodeTypeEnd {
		status = state.NodeStatusCompleted
	}
	entries = append(entries, nodeLogEntry(targetNodeID, evidence.EntryKindGate, state.Event{Type: state.EventNodeStatusSet, NodeStatus: status}, evidenceRef, at))
	if status == state.NodeStatusCompleted && targetTemplate.Type == model.NodeTypeEnd {
		entries = append(entries, runLogEntry(evidence.EntryKindGate, state.Event{Type: state.EventRunStatusSet, RunStatus: processplan.TerminalRunStatus(targetTemplate)}, evidenceRef, at))
	}
	return entries, nil
}
