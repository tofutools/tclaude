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
	"github.com/tofutools/tclaude/pkg/claude/process/state"
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
	fmt.Fprintf(out, "Last seq: %d\n", snapshot.State.LastLogSeq)
	fmt.Fprintln(out, "\nNodes:")
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tATTEMPT\tCHOSEN")
	for _, nodeID := range sortedNodeIDs(snapshot.State.Nodes) {
		node := snapshot.State.Nodes[nodeID]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", nodeID, node.Type, node.Status, node.Attempt, orDash(node.ChosenEdge))
	}
	if err := tw.Flush(); err != nil {
		return err
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
