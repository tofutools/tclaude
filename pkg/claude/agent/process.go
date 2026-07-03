package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent process` — the advisory process runtime (JOH-242).
//
// A task force deployed from a template with a process runs through an ORDERED
// list of phases (its quest plan). The runtime is ADVISORY: it records and
// surfaces the current phase and nudges the entering roles on advance, but
// enforces nothing. Verbs:
//
//	show    [--group <name>]              → the current phase, phase map, log
//	advance [--group <name>] [--to <phase>] → move to the next / a named phase
//
// --group is optional: with the caller in exactly one group it is inferred.
// `show` is open; `advance` is gated daemon-side (the human always, group
// owners of the group, otherwise the process.advance slug).

func processCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "process",
		Short: "Inspect or advance a group's advisory process (phases)",
		Long: "A task force deployed from a template with a process runs through an ordered list of phases. " +
			"`show` prints the current phase, the full phase map and the transition log; `advance` moves the " +
			"group to the next phase (or, with --to, a named phase) and nudges the roles active in the phase it " +
			"enters. The process is advisory — tracked and surfaced, never enforced.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			processShowCmd(),
			processAdvanceCmd(),
		},
	}.ToCobra()
}

// processPhaseView / processTransitionView / processStateView mirror the
// daemon's wire shape (agentd/process.go).
type processPhaseView struct {
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Criteria string   `json:"criteria,omitempty"`
	Current  bool     `json:"current,omitempty"`
}

type processTransitionView struct {
	From  string `json:"from"`
	To    string `json:"to"`
	At    string `json:"at,omitempty"`
	Actor string `json:"actor,omitempty"`
}

type processStateView struct {
	CurrentPhase   string                  `json:"current_phase"`
	PhaseIndex     int                     `json:"phase_index"`
	PhaseCount     int                     `json:"phase_count"`
	PhaseStartedAt string                  `json:"phase_started_at,omitempty"`
	Phases         []processPhaseView      `json:"phases"`
	Transitions    []processTransitionView `json:"transitions"`
}

// ---- show ----

type processShowParams struct {
	Group string `long:"group" optional:"true" help:"Group whose process to show. Inferred when you are in exactly one group."`
}

func processShowCmd() *cobra.Command {
	return boa.CmdT[processShowParams]{
		Use:         "show",
		Short:       "Show a group's current phase, phase map and transition log",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *processShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessShow(p *processShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	group, rc := resolveCallerGroup(p.Group, stderr)
	if rc != rcOK {
		return rc
	}
	var st processStateView
	if err := DaemonRequest(http.MethodGet, "/v1/groups/"+url.PathEscape(group)+"/process", nil, &st, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printProcessState(stdout, group, &st)
	return rcOK
}

// printProcessState renders the phase chip, the ordered phase map (the current
// phase marked), and the transition log.
func printProcessState(stdout io.Writer, group string, st *processStateView) {
	chip := st.CurrentPhase
	if st.PhaseIndex >= 0 {
		chip = fmt.Sprintf("phase %d/%d: %s", st.PhaseIndex+1, st.PhaseCount, st.CurrentPhase)
	}
	fmt.Fprintf(stdout, "Group %q — %s\n", group, chip)
	for i, ph := range st.Phases {
		marker := "  "
		if ph.Current {
			marker = "▸ "
		}
		roles := "(any)"
		if len(ph.Roles) > 0 {
			roles = strings.Join(ph.Roles, ", ")
		}
		fmt.Fprintf(stdout, "  %s%d. %s  [roles: %s]\n", marker, i+1, ph.Name, roles)
		if crit := strings.TrimSpace(ph.Criteria); crit != "" {
			for _, line := range strings.Split(ph.Criteria, "\n") {
				fmt.Fprintf(stdout, "       │ %s\n", line)
			}
		}
	}
	if len(st.Transitions) > 0 {
		fmt.Fprintln(stdout, "  transitions:")
		for _, tr := range st.Transitions {
			from := tr.From
			if from == "" {
				from = "(start)"
			}
			line := fmt.Sprintf("    %s → %s", from, tr.To)
			if tr.Actor != "" {
				line += "  by " + tr.Actor
			}
			if tr.At != "" {
				line += "  " + tr.At
			}
			fmt.Fprintln(stdout, line)
		}
	}
}

// ---- advance ----

type processAdvanceParams struct {
	Group string `long:"group" optional:"true" help:"Group whose process to advance. Inferred when you are in exactly one group."`
	To    string `long:"to" optional:"true" help:"Advance to this named phase instead of the next one (for correction). Still advisory."`
}

func processAdvanceCmd() *cobra.Command {
	return boa.CmdT[processAdvanceParams]{
		Use:   "advance",
		Short: "Advance a group's process to the next (or a named) phase",
		Long: "Moves the group to the next phase — or, with --to, to a named phase — records the transition with " +
			"your identity, and nudges the roles active in the phase it enters. Gated: the human always, group " +
			"owners of the group, otherwise the process.advance permission.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processAdvanceParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *processAdvanceParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessAdvance(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessAdvance(p *processAdvanceParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	group, rc := resolveCallerGroup(p.Group, stderr)
	if rc != rcOK {
		return rc
	}
	body := map[string]any{}
	if to := strings.TrimSpace(p.To); to != "" {
		body["to"] = to
	}
	var resp struct {
		Group    string           `json:"group"`
		From     string           `json:"from"`
		To       string           `json:"to"`
		Notified int              `json:"notified"`
		State    processStateView `json:"state"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/groups/"+url.PathEscape(group)+"/process/advance",
		body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Advanced group %q: %s → %s (%d agent%s nudged)\n",
		resp.Group, orStart(resp.From), resp.To, resp.Notified, plural(resp.Notified))
	printProcessState(stdout, resp.Group, &resp.State)
	return rcOK
}

// orStart renders the phase moved from, or "(start)" for the initial "".
func orStart(phase string) string {
	if phase == "" {
		return "(start)"
	}
	return phase
}

// resolveCallerGroup returns the group to act on: the explicit --group when
// given, else the caller's own group when it is in exactly one. Zero groups or
// several (with no --group) is a clear error rather than a guess.
func resolveCallerGroup(explicit string, stderr io.Writer) (string, int) {
	if g := strings.TrimSpace(explicit); g != "" {
		return g, rcOK
	}
	var resp struct {
		Groups []string `json:"groups"`
	}
	if err := DaemonGet("/v1/whoami", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: could not determine your group: %v\n", err)
		return "", MapDaemonErrorToRC(err)
	}
	switch len(resp.Groups) {
	case 0:
		fmt.Fprintln(stderr, "Error: you are not in a group — pass --group <name>")
		return "", rcInvalidArg
	case 1:
		return resp.Groups[0], rcOK
	default:
		fmt.Fprintf(stderr, "Error: you are in several groups (%s) — pass --group <name>\n",
			strings.Join(resp.Groups, ", "))
		return "", rcInvalidArg
	}
}
