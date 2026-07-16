package processcmd

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	processplan "github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	"github.com/tofutools/tclaude/pkg/common"
)

type showParams struct {
	RunID     string `pos:"true" help:"Process run id to show"`
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
	Mermaid   bool   `long:"mermaid" help:"Export the run graph as Mermaid"`
	Recent    int    `long:"recent" optional:"true" help:"Number of recent manifest events to show"`
}

func showCmd() *cobra.Command {
	return boa.CmdT[showParams]{
		Use:         "show",
		Short:       "Show process run state",
		Long:        "Show a process run state summary and recent manifest events.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(1),
		PreExecuteFunc: func(p *showParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *showParams, cmd *cobra.Command, _ []string) {
			exitWithError(runShow(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runShow(cmd *cobra.Command, p *showParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	schema, err := fs.RunStateSchemaVersion(cmd.Context(), p.RunID)
	if err != nil {
		report := processverify.LoadError(p.RunID, err)
		renderReport(out, report)
		return err
	}
	if schema == pathv1.CheckpointStateSchemaVersion {
		return runShowPathV1(cmd, fs, p, out)
	}
	if schema <= 0 || schema > pathv1.LegacyMaxSchemaVersion {
		return fmt.Errorf("process run %q uses unsupported state schema %d", p.RunID, schema)
	}
	snapshot, err := fs.LoadRun(cmd.Context(), p.RunID)
	if err != nil {
		report := processverify.LoadError(p.RunID, err)
		renderReport(out, report)
		return err
	}
	if p.Mermaid {
		tmpl, err := fs.GetTemplate(cmd.Context(), snapshot.Run.TemplateRef)
		if err != nil {
			return err
		}
		renderMermaid(out, snapshot, tmpl)
		return nil
	}
	report := processverify.Snapshot(snapshot)
	fmt.Fprintf(out, "Run: %s\n", snapshot.Run.ID)
	fmt.Fprintf(out, "Template: %s\n", snapshot.Run.TemplateRef)
	fmt.Fprintf(out, "Status: %s\n", report.EffectiveStatus)
	if snapshot.State.Pause != nil {
		fmt.Fprintf(out, "Waiting: %s\n", snapshot.State.Pause.Reason)
	}
	fmt.Fprintf(out, "Last seq: %d\n", snapshot.State.LastLogSeq)
	// Budgets come from the pinned template; a run whose template cannot load
	// still renders (verify reports the broken store separately).
	budgets := map[string]int{}
	if tmpl, err := fs.GetTemplate(cmd.Context(), snapshot.Run.TemplateRef); err == nil {
		budgets = gateBudgets(snapshot.State.Nodes, tmpl)
	}
	fmt.Fprintln(out, "\nNodes:")
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tATTEMPT\tCHOSEN\tDETAIL")
	rendered := map[string]bool{}
	for _, nodeID := range sortedNodeIDs(snapshot.State.Nodes) {
		node := snapshot.State.Nodes[nodeID]
		if node.Parent != "" {
			continue
		}
		renderNodeRow(tw, nodeID, node, false, budgets[nodeID])
		rendered[nodeID] = true
		for _, childID := range node.Children {
			child, ok := snapshot.State.Nodes[childID]
			if !ok {
				continue
			}
			renderNodeRow(tw, childID, child, true, budgets[childID])
			rendered[childID] = true
		}
	}
	// Stage children not listed by their parent (corrupt linkage) still render;
	// verify flags the inconsistency.
	for _, nodeID := range sortedNodeIDs(snapshot.State.Nodes) {
		if rendered[nodeID] {
			continue
		}
		renderNodeRow(tw, nodeID, snapshot.State.Nodes[nodeID], true, budgets[nodeID])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(snapshot.State.Obligations) > 0 {
		fmt.Fprintln(out, "\nObligations:")
		ow := newTable(out)
		fmt.Fprintln(ow, "ID\tNODE\tKIND\tASSIGNEE\tSTATUS\tDUE\tACTIONS\tSUMMARY")
		for _, id := range sortedObligationIDs(snapshot.State.Obligations) {
			obligation := snapshot.State.Obligations[id]
			fmt.Fprintf(ow, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				id, obligation.NodeID, obligation.Kind, obligation.Assignee, obligation.Status,
				formatTime(obligation.DueAt), strings.Join(obligation.AvailableActions, ","), obligation.Summary)
		}
		if err := ow.Flush(); err != nil {
			return err
		}
	}
	if len(snapshot.State.Contacts) > 0 {
		fmt.Fprintln(out, "\nNudges:")
		cw := newTable(out)
		fmt.Fprintln(cw, "COMMAND\tASSIGNEE\tLAST\tNEXT\tBUDGET\tESCALATION\tSTATE")
		for _, id := range sortedContactIDs(snapshot.State.Contacts) {
			contact := snapshot.State.Contacts[id]
			contactState := "active"
			if contact.Paused {
				contactState = "paused: " + contact.PauseReason
			} else if !contact.EscalatedAt.IsZero() {
				contactState = "escalated"
			}
			fmt.Fprintf(cw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n", id, contact.Assignee,
				formatTime(contact.LastContactedAt), formatTime(contact.NextContactAt), contact.Used, contact.Budget,
				contact.EscalationTarget, contactState)
		}
		if err := cw.Flush(); err != nil {
			return err
		}
	}
	recent := p.Recent
	if recent <= 0 {
		recent = 8
	}
	fmt.Fprintln(out, "\nRecent manifest:")
	start := len(snapshot.Manifest) - recent
	if start < 0 {
		start = 0
	}
	for _, entry := range snapshot.Manifest[start:] {
		fmt.Fprintf(out, "  #%d %s %s %s\n", entry.Seq, entry.Scope.Kind, orDash(entry.Scope.ID), entry.EventRef)
	}
	return nil
}

func runShowPathV1(cmd *cobra.Command, fs *store.FS, p *showParams, out io.Writer) error {
	snapshot, err := fs.LoadPathV1RunView(cmd.Context(), p.RunID)
	if err != nil {
		return err
	}
	if _, err := pathv1.VerifyExecutionInput(cmd.Context(), snapshot.CheckpointJSON, snapshot.TemplateSource); err != nil {
		return err
	}
	parsed, err := model.ParseExactSource(snapshot.TemplateSource)
	if err != nil || parsed.Diagnostics.HasErrors() {
		return fmt.Errorf("schema-7 process template is invalid")
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	if err != nil {
		return err
	}
	if p.Mermaid {
		renderPathV1Mermaid(out, parsed.Template)
		return nil
	}
	fmt.Fprintf(out, "Run: %s\n", snapshot.Run.ID)
	fmt.Fprintf(out, "Template: %s\n", snapshot.Run.TemplateRef)
	fmt.Fprintf(out, "Status: %s\n", pathv1.CurrentRunStatus(snapshot.Checkpoint))
	fmt.Fprintf(out, "State schema: %d\n", pathv1.CheckpointStateSchemaVersion)
	fmt.Fprintf(out, "Checkpoint revision: %d\n", pathv1.CheckpointRevision(snapshot.Checkpoint))
	fmt.Fprintln(out, "\nNodes:")
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tTYPE\tROUTING STATE")
	states := pathV1RoutingStates(aggregate)
	ids := make([]string, 0, len(parsed.Template.Nodes))
	for id := range parsed.Template.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", id, parsed.Template.Nodes[id].Type, orDash(states[id]))
	}
	return tw.Flush()
}

func pathV1RoutingStates(aggregate pathv1.AggregateCheckpoint) map[string]string {
	states := make(map[string]string)
	generations := make(map[string]uint64)
	for _, reservation := range aggregate.Routing.Reservations {
		value := string(reservation.State)
		if previous := states[reservation.NodeID]; previous == "" || reservation.Generation >= generations[reservation.NodeID] {
			states[reservation.NodeID] = value
			generations[reservation.NodeID] = reservation.Generation
		}
	}
	return states
}

func renderPathV1Mermaid(out io.Writer, tmpl *model.Template) {
	fmt.Fprintln(out, "graph TD")
	for _, edge := range model.NormalizeEdges(tmpl) {
		from := edge.From
		if from == "" {
			from = "__start"
		}
		fmt.Fprintf(out, "  %s -->|%s| %s\n", mermaidID(from), mermaidLabel(edge.Outcome), mermaidID(edge.To))
	}
	ids := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(out, "  %s[\"%s<br/>%s\"]\n", mermaidID(id), mermaidLabel(id), mermaidLabel(string(tmpl.Nodes[id].Type)))
	}
}

func sortedObligationIDs(values map[string]state.ObligationRecord) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedContactIDs(values map[string]state.ContactState) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func renderMermaid(out io.Writer, snapshot store.Snapshot, tmpl *model.Template) {
	fmt.Fprintln(out, "graph TD")
	for _, edge := range model.NormalizeEdges(tmpl) {
		from := edge.From
		if from == "" {
			from = "__start"
		}
		fmt.Fprintf(out, "  %s -->|%s| %s\n", mermaidID(from), mermaidLabel(edge.Outcome), mermaidID(edge.To))
	}
	for _, nodeID := range sortedNodeIDs(snapshot.State.Nodes) {
		node := snapshot.State.Nodes[nodeID]
		fmt.Fprintf(out, "  %s[\"%s<br/>%s\"]\n", mermaidID(nodeID), mermaidLabel(nodeID), mermaidLabel(string(node.Status)))
	}
}

func renderNodeRow(tw io.Writer, nodeID string, node state.NodeState, indent bool, gateBudget int) {
	id := nodeID
	if indent {
		id = "  " + nodeID
	}
	nodeType := string(node.Type)
	if node.Stage != "" {
		nodeType = "stage:" + string(node.Stage)
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", id, orDash(nodeType), node.Status, node.Attempt, orDash(node.ChosenEdge), orDash(nodeDetail(node, gateBudget)))
}

func nodeDetail(node state.NodeState, gateBudget int) string {
	var parts []string
	if node.Status == state.NodeStatusBlocked {
		blocked := fmt.Sprintf("blocked: %s (owner %s)", node.BlockedReason, node.BlockedOwner)
		if !node.BlockedAt.IsZero() {
			blocked += " since " + formatTime(node.BlockedAt)
		}
		parts = append(parts, blocked)
	}
	if node.Parent != "" && node.Stage.IsGateStage() {
		if gateBudget > 0 {
			parts = append(parts, fmt.Sprintf("fails %d/%d", node.FailCount, gateBudget))
		} else if node.FailCount > 0 {
			parts = append(parts, fmt.Sprintf("fails %d", node.FailCount))
		}
		if len(node.Decisions) > 0 {
			parts = append(parts, fmt.Sprintf("verdicts %d", len(node.Decisions)))
		}
	}
	if node.PendingFeedback != nil {
		parts = append(parts, "feedback pending from "+node.PendingFeedback.FromNodeID)
	}
	if node.Status != state.NodeStatusBlocked && node.ActiveAttempt != nil && node.ActiveAttempt.EvidenceRef != "" {
		parts = append(parts, "evidence: "+node.ActiveAttempt.EvidenceRef)
	}
	return strings.Join(parts, "; ")
}

// gateBudgets derives each gate stage child's failed-verdict budget from the
// pinned template, keyed by child node id.
func gateBudgets(nodes map[string]state.NodeState, tmpl *model.Template) map[string]int {
	budgets := map[string]int{}
	for _, nodeID := range sortedNodeIDs(nodes) {
		node := nodes[nodeID]
		if node.Parent != "" || len(node.Children) == 0 {
			continue
		}
		templateNode, ok := tmpl.Nodes[nodeID]
		if !ok {
			continue
		}
		for _, spec := range model.ExpandNode(nodeID, templateNode) {
			if spec.Stage.IsGateStage() {
				budgets[spec.ChildID] = processplan.GateBudget(spec)
			}
		}
	}
	return budgets
}

func sortedNodeIDs(nodes map[string]state.NodeState) []string {
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mermaidID(id string) string {
	return "n_" + hex.EncodeToString([]byte(id))
}

func mermaidLabel(label string) string {
	replacer := strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"|", " ",
		"[", "(",
		"]", ")",
		"<", "",
		">", "",
		"\"", "'",
	)
	return replacer.Replace(label)
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
