// Package workflows is the `tclaude workflows` CLI surface over the
// ccworkflows data layer: a read-only, greppable view of Claude Code's builtin
// workflow runs and saved templates (ls / show / cat). It mirrors the web
// "Workflows" tab and does no path logic of its own — everything is read
// through the ccworkflows package.
//
// Note the deliberate plural noun: `tclaude workflows` observes Claude Code's
// builtin *workflows* feature, distinct from tclaude's own (paused) custom
// node-graph `workgraph` engine.
package workflows

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params is the (empty) parameter set for the parent group.
type Params struct{}

// Cmd returns the `workflows` command group.
func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:     "workflows",
		Aliases: []string{"wf"},
		Short:   "Inspect Claude Code's builtin workflow runs and saved templates",
		Long: "Inspect Claude Code's builtin \"workflows\" feature: list saved workflow\n" +
			"templates and runs, show a run's phase/agent fan-out tree, and print the\n" +
			"script behind a run or template.\n\n" +
			"The ls/show/cat subcommands are read-only; output is plain and greppable by\n" +
			"default, pass --json for machine use. `run` is an EXPERIMENTAL trigger that\n" +
			"injects a saved workflow's launch into another agent's pane (best-effort).\n" +
			"(Distinct from tclaude's own `workgraph` engine.)",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			LsCmd(),
			ShowCmd(),
			CatCmd(),
			RunCmd(),
		},
		// No RunFunc: invoking the bare group prints help (cobra default).
	}.ToCobra()
}

// --- shared formatting helpers ---------------------------------------------

// writeJSON marshals v as indented JSON to w. It returns an exit code (0 ok,
// 1 on error, with the error written to stderr).
func writeJSON(w io.Writer, stderr io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "Error encoding JSON: %v\n", err)
		return 1
	}
	return 0
}

// fmtTimeMs renders an epoch-ms timestamp as a local "2006-01-02 15:04", or "-"
// when zero/unknown.
func fmtTimeMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// fmtDurationMs renders a millisecond duration compactly (e.g. "3.0s",
// "1m04s"), or "-" when zero/unknown.
func fmtDurationMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// fmtCount renders a count, or "-" when zero (used for the agent count of an
// in-flight run whose final count is not yet known).
func fmtCount(n int) string {
	if n <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

// shortID truncates a long id (session UUID, agent id) for column display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// firstLine returns s up to the first newline, truncated to max runes with an
// ellipsis — for one-line descriptions in a table.
func firstLine(s string, max int) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			s = s[:i]
			break
		}
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
