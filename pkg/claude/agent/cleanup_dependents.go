package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/common"
)

type cleanupDependentsParams struct {
	Orphaned bool `long:"orphaned" help:"Clean up what still refers to retired or deleted agents (required: the only mode)"`
	DryRun   bool `long:"dry-run" help:"Only list what would be removed, cancelled or cleared"`
	JSON     bool `long:"json" help:"Emit the daemon's JSON object instead of text"`
}

// orphanedDependentsReply is the daemon's /v1/orphaned-dependents answer.
type orphanedDependentsReply struct {
	Agents []struct {
		AgentID    string `json:"agent_id"`
		State      string `json:"state"`
		ConvID     string `json:"conv_id"`
		Title      string `json:"title"`
		Dependents []struct {
			Kind string `json:"kind"`
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Via  string `json:"via"`
		} `json:"dependents"`
		Runs []struct {
			ID          string `json:"id"`
			TemplateRef string `json:"template_ref"`
		} `json:"runs"`
		PinnedSteps []struct {
			Template string `json:"template"`
			Node     string `json:"node"`
			Slot     string `json:"slot"`
		} `json:"pinned_steps"`
		Result *dependentsReply `json:"result"`
		Error  string           `json:"error"`
	} `json:"agents"`
	Unavailable []string `json:"unavailable"`
}

func cleanupDependentsCmd() *cobra.Command {
	return boa.CmdT[cleanupDependentsParams]{
		Use:   "cleanup-dependents",
		Short: "Remove what still refers to retired or deleted agents",
		Long: "With --orphaned, finds agents that are retired or deleted but still have cron jobs, standing orders " +
			"or automations (their own, or cron jobs and standing orders addressed to them) or unfinished runs " +
			"scoped to them, as retirements before the cascade, or with --keep-dependents, left behind. It removes " +
			"those rows, cancels the runs and clears template steps pinned to the agent by id, as retiring it " +
			"today would. --dry-run lists what that would do. An agent away on a federation peer is not an orphan. " +
			"It exits non-zero when an agent was skipped or left a warning. Operator only: run it from your own shell.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cleanupDependentsParams, _ *cobra.Command, _ []string) {
			os.Exit(runCleanupDependents(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runCleanupDependents(p *cleanupDependentsParams, stdout, stderr io.Writer) int {
	if !p.Orphaned {
		fmt.Fprintln(stderr, "Error: say what to clean up: --orphaned (what still refers to retired or deleted agents)")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	method := http.MethodPost
	if p.DryRun {
		method = http.MethodGet
	}
	var out orphanedDependentsReply
	if err := DaemonRequest(method, "/v1/orphaned-dependents", nil, &out, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, out)
	}
	if len(out.Agents) == 0 {
		fmt.Fprintln(stdout, "(nothing refers to a retired or deleted agent)")
	}
	incomplete := 0
	for _, a := range out.Agents {
		name := a.Title
		if name == "" {
			name = a.AgentID
		}
		fmt.Fprintf(stdout, "%s (%s, %s)\n", name, a.AgentID, a.State)
		if a.Error != "" {
			fmt.Fprintf(stdout, "  skipped: %s\n", a.Error)
			incomplete++
			continue
		}
		if !p.DryRun {
			a.Result.print(stdout)
			if a.Result != nil && len(a.Result.Warnings) > 0 {
				incomplete++
			}
			continue
		}
		for _, dep := range a.Dependents {
			label := dep.Name
			if label == "" {
				label = fmt.Sprintf("#%d", dep.ID)
			}
			via := "its own"
			if dep.Via == "target" {
				via = "addressed to it"
			}
			fmt.Fprintf(stdout, "  would remove %s %s (%s)\n", dependentKindWords[dep.Kind], label, via)
		}
		for _, run := range a.Runs {
			fmt.Fprintf(stdout, "  would cancel run %s (%s)\n", run.ID, run.TemplateRef)
		}
		for _, step := range a.PinnedSteps {
			fmt.Fprintf(stdout, "  would clear template %s step %s (%s)\n", step.Template, step.Node, step.Slot)
		}
	}
	for _, note := range out.Unavailable {
		fmt.Fprintln(stdout, "note: "+note)
	}
	if incomplete > 0 {
		fmt.Fprintf(stderr, "Error: %d agent(s) were not fully cleaned up; see skipped and warning lines above\n", incomplete)
		return rcIOFailure
	}
	return rcOK
}
