package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// serviceLaunchView mirrors agentd.NodeServiceLaunch.
type serviceLaunchView struct {
	Profile   *config.LaunchProfile `json:"profile"`
	Version   string                `json:"version"`
	Suggested struct {
		Flags []struct {
			Arg    string `json:"arg"`
			Name   string `json:"name"`
			Secret bool   `json:"secret"`
		} `json:"flags"`
		Env []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Secret bool   `json:"secret"`
			Mode   string `json:"mode"`
		} `json:"env"`
	} `json:"suggested"`
	Missing  []string `json:"missing"`
	Owned    bool     `json:"owned"`
	Launcher struct {
		Kind             string `json:"kind"`
		Unit             string `json:"unit"`
		ManagedByTclaude bool   `json:"managed_by_tclaude"`
	} `json:"launcher"`
	Comparison struct {
		Baseline    string                        `json:"baseline"`
		State       string                        `json:"state"`
		Differences []selfupdate.LaunchDifference `json:"differences"`
	} `json:"comparison"`
	Warnings []string `json:"warnings"`
	Access   string   `json:"access"`
	Redacted []string `json:"redacted"`
}

type serviceLaunchReadParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read grant; a peer's view has no environment values)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

type serviceLaunchSetParams struct {
	Flag    []string `long:"flag" optional:"true" help:"An agentd serve flag, as --name or --name=value (repeatable)"`
	Env     []string `long:"env" optional:"true" help:"An explicit, non-secret KEY=VALUE (repeatable)"`
	Inherit []string `long:"inherit" optional:"true" help:"A variable taken from agentd's environment when the unit is written, by name (repeatable; use it for secrets)"`
	Clear   bool     `long:"clear" help:"Remove the launch profile; autostart then keeps the observed launch again"`
	Node    string   `long:"node" optional:"true" help:"Trusted peer label or instance ID (needs node.update.settings; only an unrestricted peer may change flags or explicit variables)"`
	JSON    bool     `long:"json" help:"Output JSON"`
}

type serviceLaunchUseParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID (needs node.update.settings and unrestricted trust)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func serviceLaunchCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "launch",
		Short: "Show or set agentd's launch profile: the serve flags and environment the autostart unit runs with",
		Long: "The launch profile (service.launch in config.json) is what `tclaude service install` writes into the autostart unit, and what agentd is expected to run with. " +
			"Secret values are never stored: secret-looking variables are inherited by name (their value is read from the environment agentd started with when the unit is written, into the 0600 env file). " +
			"Where a unit tclaude did not write starts agentd, the profile is information only and that unit is never rewritten.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boa.CmdT[serviceLaunchReadParams]{
				Use: "show", Short: "Show the launch profile and how the running agentd compares with it",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLaunchReadParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLaunchShow(p, false, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLaunchReadParams]{
				Use: "suggest", Short: "Show the profile the running agentd suggests (its flags and environment, secrets redacted)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLaunchReadParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLaunchShow(p, true, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLaunchSetParams]{
				Use: "set", Short: "Replace the launch profile with the given flags and variables (or --clear it)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLaunchSetParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLaunchSet(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLaunchUseParams]{
				Use: "use-running", Short: "Set the launch profile from what agentd runs with now (secret flags are left out; secret variables are inherited)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLaunchUseParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLaunchUseRunning(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
		},
	}.ToCobra()
}

func getServiceLaunch(node string, stderr io.Writer) (serviceLaunchView, json.RawMessage, int) {
	var v serviceLaunchView
	raw, rc := serviceRequest(http.MethodGet, servicePath(node, "/launch"), nil, stderr)
	if rc != 0 {
		return v, nil, rc
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return v, nil, rcIOFailure
	}
	return v, raw, 0
}

func putServiceLaunch(node string, profile *config.LaunchProfile, version string, asJSON bool, stdout, stderr io.Writer) int {
	raw, rc := serviceRequest(http.MethodPut, servicePath(node, "/launch"), map[string]any{"profile": profile, "version": version}, stderr)
	if rc != 0 {
		return rc
	}
	if asJSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var v serviceLaunchView
	if err := json.Unmarshal(raw, &v); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	if profile == nil {
		fmt.Fprintln(stdout, "Launch profile cleared.")
	} else {
		fmt.Fprintln(stdout, "Launch profile saved. `tclaude service install` writes it into the autostart unit.")
	}
	printServiceLaunch(v, stdout)
	return 0
}

func runServiceLaunchShow(p *serviceLaunchReadParams, suggestion bool, stdout, stderr io.Writer) int {
	v, raw, rc := getServiceLaunch(p.Node, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if suggestion {
		printLaunchSuggestion(v, stdout)
	} else {
		printServiceLaunch(v, stdout)
	}
	return 0
}

func printLaunchSuggestion(v serviceLaunchView, w io.Writer) {
	fmt.Fprintln(w, "Running flags:")
	for _, f := range v.Suggested.Flags {
		note := ""
		if f.Secret {
			note = "  (secret: not taken into a profile; pass it through an inherited variable)"
		}
		fmt.Fprintf(w, "  %s%s\n", termtext.Cell(f.Arg), note)
	}
	fmt.Fprintln(w, "Running environment:")
	for _, e := range v.Suggested.Env {
		value := e.Value
		if value == "" && len(v.Redacted) > 0 {
			value = "(withheld from peers)"
		}
		fmt.Fprintf(w, "  %-7s %s=%s\n", e.Mode, termtext.Cell(e.Key), termtext.Cell(value))
	}
	for _, warning := range v.Warnings {
		fmt.Fprintln(w, "Note:", termtext.Cell(warning))
	}
}

func printServiceLaunch(v serviceLaunchView, w io.Writer) {
	if v.Profile == nil {
		fmt.Fprintln(w, "Launch profile:  none (autostart keeps the observed launch; `tclaude service launch use-running` sets one from it)")
	} else {
		fmt.Fprintln(w, "Launch profile:")
		for _, f := range v.Profile.Flags {
			fmt.Fprintf(w, "  flag     %s\n", termtext.Cell(f))
		}
		for _, e := range v.Profile.Env {
			value := e.Value
			if len(v.Redacted) > 0 {
				value = "(withheld from peers)"
			}
			fmt.Fprintf(w, "  env      %s=%s\n", termtext.Cell(e.Key), termtext.Cell(value))
		}
		for _, k := range v.Profile.InheritEnv {
			fmt.Fprintf(w, "  inherit  %s\n", termtext.Cell(k))
		}
		if len(v.Missing) > 0 {
			fmt.Fprintf(w, "  not set in agentd's environment: %s\n", termtext.Cell(strings.Join(v.Missing, ", ")))
		}
	}
	if !v.Owned {
		fmt.Fprintf(w, "Applied:         no; %s %s, which tclaude did not write, starts agentd\n", termtext.Cell(v.Launcher.Kind), termtext.Cell(v.Launcher.Unit))
	}
	switch c := v.Comparison; c.State {
	case "identical":
		fmt.Fprintf(w, "Running agentd:  matches the %s ✓\n", termtext.Cell(c.Baseline))
	case "differs":
		fmt.Fprintf(w, "Running agentd:  %d difference(s) from the %s:\n", len(c.Differences), termtext.Cell(c.Baseline))
		for _, d := range c.Differences {
			line := d.String()
			if d.Kind == "env" && d.Change == "changed" && d.Before != "" {
				line += fmt.Sprintf(" (%s → %s)", d.Before, d.After)
			}
			fmt.Fprintf(w, "  %s\n", termtext.Cell(line))
		}
	case "unavailable":
		fmt.Fprintf(w, "Running agentd:  not compared with the %s (its command line is not `agentd serve`)\n", termtext.Cell(c.Baseline))
	case "not_applied":
		fmt.Fprintln(w, "Running agentd:  started by hand, not from the unit; the profile applies to the unit started at boot")
	}
	for _, warning := range v.Warnings {
		fmt.Fprintln(w, "Note:", termtext.Cell(warning))
	}
}

func runServiceLaunchSet(p *serviceLaunchSetParams, stdout, stderr io.Writer) int {
	if p.Clear && (len(p.Flag)+len(p.Env)+len(p.Inherit) > 0) {
		fmt.Fprintln(stderr, "Error: --clear takes no --flag, --env or --inherit")
		return rcInvalidArg
	}
	v, _, rc := getServiceLaunch(p.Node, stderr)
	if rc != 0 {
		return rc
	}
	if p.Clear {
		return putServiceLaunch(p.Node, nil, v.Version, p.JSON, stdout, stderr)
	}
	profile := &config.LaunchProfile{Flags: append([]string{}, p.Flag...), Env: []config.LaunchEnv{}, InheritEnv: append([]string{}, p.Inherit...)}
	for _, kv := range p.Env {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			fmt.Fprintf(stderr, "Error: --env %q is not KEY=VALUE\n", kv)
			return rcInvalidArg
		}
		profile.Env = append(profile.Env, config.LaunchEnv{Key: k, Value: val})
	}
	return putServiceLaunch(p.Node, profile, v.Version, p.JSON, stdout, stderr)
}

func runServiceLaunchUseRunning(p *serviceLaunchUseParams, stdout, stderr io.Writer) int {
	v, _, rc := getServiceLaunch(p.Node, stderr)
	if rc != 0 {
		return rc
	}
	profile := &config.LaunchProfile{Flags: []string{}, Env: []config.LaunchEnv{}, InheritEnv: []string{}}
	for _, f := range v.Suggested.Flags {
		if f.Secret {
			fmt.Fprintf(stderr, "Left out %s: it carries a secret; pass it through an inherited variable instead.\n", termtext.Cell(f.Name))
			continue
		}
		profile.Flags = append(profile.Flags, f.Arg)
	}
	for _, e := range v.Suggested.Env {
		if e.Mode == "inherit" {
			profile.InheritEnv = append(profile.InheritEnv, e.Key)
		} else {
			profile.Env = append(profile.Env, config.LaunchEnv{Key: e.Key, Value: e.Value})
		}
	}
	return putServiceLaunch(p.Node, profile, v.Version, p.JSON, stdout, stderr)
}
