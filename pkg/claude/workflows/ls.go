package workflows

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
	"github.com/tofutools/tclaude/pkg/common"
)

// LsParams configures `workflows ls`.
type LsParams struct {
	Runs  bool `long:"runs" help:"Show only runs (omit saved templates)."`
	Saved bool `long:"saved" help:"Show only saved templates (omit runs)."`
	JSON  bool `long:"json" help:"Emit JSON instead of text."`
}

// LsCmd returns the `workflows ls` subcommand.
func LsCmd() *cobra.Command {
	return boa.CmdT[LsParams]{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List saved workflow templates and runs",
		Long: "List saved workflow templates (~/.claude/workflows/saved and the project-local\n" +
			"mirror) and workflow runs across all Claude Code sessions on this machine.\n" +
			"By default both are shown; restrict with --saved or --runs. --json for machine use.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *LsParams, _ *cobra.Command, _ []string) {
			if code := RunLs(params, os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// lsJSON is the --json shape for `workflows ls`. A section is present (as a
// possibly-empty array) exactly when it was requested, and absent otherwise, so
// a consumer can tell "filtered out" (key absent) from "requested but empty"
// (key = []). Pointers give that three-way distinction with omitempty.
type lsJSON struct {
	Saved *[]ccworkflows.SavedScript `json:"saved,omitempty"`
	Runs  *[]ccworkflows.RunRef      `json:"runs,omitempty"`
}

// RunLs is the testable core of `workflows ls`.
func RunLs(params *LsParams, stdout, stderr io.Writer) int {
	// Default (neither flag) shows both; either flag narrows to that section.
	showSaved := params.Saved || !params.Runs
	showRuns := params.Runs || !params.Saved

	var saved []ccworkflows.SavedScript
	var runs []ccworkflows.RunRef
	if showSaved {
		projectDir, _ := os.Getwd() // "" is fine: just skips the project mirror
		s, err := ccworkflows.DefaultSavedScripts(projectDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error listing saved workflows: %v\n", err)
			return 1
		}
		saved = s
	}
	if showRuns {
		r, err := ccworkflows.ListAllRuns()
		if err != nil {
			fmt.Fprintf(stderr, "Error listing workflow runs: %v\n", err)
			return 1
		}
		runs = r
	}

	if params.JSON {
		out := lsJSON{}
		if showSaved {
			if saved == nil {
				saved = []ccworkflows.SavedScript{} // requested → [] not null
			}
			out.Saved = &saved
		}
		if showRuns {
			if runs == nil {
				runs = []ccworkflows.RunRef{}
			}
			out.Runs = &runs
		}
		return writeJSON(stdout, stderr, out)
	}

	if showSaved {
		writeSavedTable(stdout, saved)
	}
	if showSaved && showRuns {
		fmt.Fprintln(stdout)
	}
	if showRuns {
		writeRunsTable(stdout, runs)
	}
	return 0
}

func writeSavedTable(w io.Writer, saved []ccworkflows.SavedScript) {
	fmt.Fprintf(w, "SAVED TEMPLATES (%d)\n", len(saved))
	if len(saved) == 0 {
		fmt.Fprintln(w, "  (none — ~/.claude/workflows/saved is empty or absent)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tSCOPE\tPHASES\tDESCRIPTION")
	for _, s := range saved {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\n", s.Name, s.Scope, len(s.Meta.Phases), firstLine(s.Meta.Description, 60))
	}
	_ = tw.Flush()
}

func writeRunsTable(w io.Writer, runs []ccworkflows.RunRef) {
	fmt.Fprintf(w, "RUNS (%d)\n", len(runs))
	if len(runs) == 0 {
		fmt.Fprintln(w, "  (none found in any session)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  RUN ID\tSTATUS\tWORKFLOW\tAGENTS\tSTARTED\tSESSION")
	for _, r := range runs {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			r.RunID, r.Status, dashIfEmpty(r.WorkflowName), fmtCount(r.AgentCount),
			fmtTimeMs(r.StartTimeMs), shortID(r.SessionID))
	}
	_ = tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
