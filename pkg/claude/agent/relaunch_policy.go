package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

const relaunchPolicyHarnessConfigHelp = "harness_config (the --harness-config posture) weighs an explicit posture against " +
	"the one the agent's sandbox profiles give on each resume, reincarnate and clone:\n" +
	"  explicit-wins (default)  the explicit posture always stays, like any explicit flag.\n" +
	"  tighten-only             the same, except for one case: an agent spawned with --harness-config write whose " +
	"sandbox profiles keep the read-only floor gets the floor back. An explicit read pin is already the strictest posture, so it always stays.\n" +
	"  chain-wins               the sandbox profiles' posture always applies, so a read pin over a write chain is lost.\n" +
	"Switching back to explicit-wins restores the explicit posture on the next relaunch, for agents with a recorded spawn " +
	"request; clones and older agents lose it for good under chain-wins."

func relaunchPolicyCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "relaunch-policy",
		Short: "Show or set the global relaunch policy",
		Long: "The relaunch policy decides, for every agent, how a resume, reincarnate or clone resolves the configurable " +
			"launch fields. Nothing configured means every field takes its default. Reading is open; setting takes " +
			"relaunch-policy.manage, because an answer can widen what a relaunch gives every agent.\n\n" + relaunchPolicyHarnessConfigHelp,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{relaunchPolicyShowCmd(), relaunchPolicySetCmd()},
	}.ToCobra()
}

type relaunchPolicyRowJSON struct {
	Field    string   `json:"field"`
	Label    string   `json:"label"`
	Mode     string   `json:"mode"`
	Default  string   `json:"default"`
	Modes    []string `json:"modes"`
	Set      bool     `json:"set"`
	Settable bool     `json:"settable"`
	Why      string   `json:"why"`
	Gap      string   `json:"gap,omitempty"`
}

type relaunchPolicyJSON struct {
	Scope string                  `json:"scope"`
	Rows  []relaunchPolicyRowJSON `json:"rows"`
}

type relaunchPolicyShowParams struct {
	JSON bool `long:"json" help:"Emit the stable JSON object instead of the table"`
}

func relaunchPolicyShowCmd() *cobra.Command {
	return boa.CmdT[relaunchPolicyShowParams]{
		Use:         "show",
		Short:       "Show each configurable field's relaunch mode, its default, and why",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *relaunchPolicyShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runRelaunchPolicyShow(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runRelaunchPolicyShow(p *relaunchPolicyShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var out relaunchPolicyJSON
	if err := DaemonRequest(http.MethodGet, "/v1/relaunch-policy", nil, &out, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, out)
	}
	printRelaunchPolicy(stdout, out)
	return rcOK
}

func printRelaunchPolicy(w io.Writer, out relaunchPolicyJSON) {
	fmt.Fprintf(w, "%-26s  %-14s  %-14s  %s\n", "FIELD", "MODE", "DEFAULT", "MODES")
	for _, r := range out.Rows {
		mode := r.Mode
		switch {
		case !r.Settable:
			mode += " -"
		case !r.Set:
			mode += " *"
		}
		fmt.Fprintf(w, "%-26s  %-14s  %-14s  %s\n", r.Field, mode, r.Default, strings.Join(r.Modes, ", "))
	}
	fmt.Fprintln(w, "(* = not configured, the default applies; - = not settable yet, relaunches do not read this row)")
	for _, r := range out.Rows {
		fmt.Fprintf(w, "\n%s (%s)\n  Why: %s\n", r.Label, r.Field, r.Why)
		if r.Gap != "" {
			fmt.Fprintf(w, "  Not yet: %s\n", r.Gap)
		}
	}
}

type relaunchPolicySetParams struct {
	Field string `pos:"true" help:"Configurable field key, from 'tclaude agent relaunch-policy show' (e.g. harness_config)"`
	Mode  string `pos:"true" help:"One of the field's modes; its default clears the stored answer"`
}

func relaunchPolicySetCmd() *cobra.Command {
	return boa.CmdT[relaunchPolicySetParams]{
		Use:         "set",
		Short:       "Set one field's relaunch mode for every agent (requires relaunch-policy.manage)",
		Long:        "Set the relaunch mode of one configurable field, for every agent. It takes effect on each agent's next resume, reincarnate or clone. Setting a field to its default clears the stored answer.\n\n" + relaunchPolicyHarnessConfigHelp,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *relaunchPolicySetParams, _ *cobra.Command, _ []string) {
			os.Exit(runRelaunchPolicySet(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runRelaunchPolicySet(p *relaunchPolicySetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var out relaunchPolicyJSON
	body := map[string]string{"field": strings.TrimSpace(p.Field), "mode": strings.TrimSpace(p.Mode)}
	if err := DaemonRequest(http.MethodPut, "/v1/relaunch-policy", body, &out, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	for _, r := range out.Rows {
		if r.Field == body["field"] {
			fmt.Fprintf(stdout, "%s: %s (takes effect on each agent's next relaunch)\n", r.Field, r.Mode)
		}
	}
	return rcOK
}
