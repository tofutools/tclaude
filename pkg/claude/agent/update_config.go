package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// updateSettingsPath is this node's settings API, or a trusted peer's.
func updateSettingsPath(node string) string { return updateAPIPath(node, "/settings") }

// updateSettingsView mirrors GET /v1/node/update/settings.
type updateSettingsView struct {
	Settings         config.SelfUpdateConfig `json:"settings"`
	MethodChosen     bool                    `json:"method_chosen"`
	Detected         *selfupdate.Detection   `json:"detected"`
	DetectionError   string                  `json:"detection_error"`
	Methods          []selfupdate.MethodInfo `json:"methods"`
	InstallDir       string                  `json:"install_dir"`
	InstallShadowed  bool                    `json:"install_dir_shadowed"`
	ShadowedBy       string                  `json:"shadowed_by"`
	Warnings         []string                `json:"warnings"`
	Redacted         []string                `json:"redacted"`
	NextScheduledRun *time.Time              `json:"next_scheduled_run"`
	LastScheduledRun *struct {
		At      time.Time `json:"at"`
		Trigger string    `json:"trigger"`
		Outcome string    `json:"outcome"`
		JobID   string    `json:"job_id"`
		Detail  string    `json:"detail"`
	} `json:"last_scheduled_run"`
}

type updateConfigShowParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.update grant)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

type updateConfigSetParams struct {
	Node                  string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.update and node.update.settings grants)"`
	ConfirmPackageManager bool   `long:"confirm-package-manager" help:"Confirm letting tclaude run a package manager (needed to switch to homebrew, nix_profile, mise, apt or dnf)"`
	JSON                  bool   `long:"json" help:"Output JSON"`
}

// updateConfigKeys documents every `tclaude update config set` key.
var updateConfigKeys = []struct{ key, help string }{
	{"method", "release | go_install | source_checkout | homebrew | nix_profile | mise | apt | dnf | custom | manual | none"},
	{"schedule", "off | daily | weekly"},
	{"weekday", "monday..sunday (weekly schedule)"},
	{"window", "local time window HH:MM-HH:MM, e.g. 03:00-05:00"},
	{"defer-while-working", "true | false: wait while agents are working"},
	{"update-on-start", "true | false: check/apply when the daemon starts"},
	{"post-setup", "on | off: run the new binary's tclaude setup --yes after an update"},
	{"setup-flags", "comma-separated setup flags, e.g. install-agent-skills,install-default-agent-permissions (empty clears)"},
	{"setup-harness", "harness for setup --harness (empty clears)"},
	{"checkout-path", "local tclaude git checkout for source_checkout"},
	{"checkout-ref", "branch or tag to build (empty: the release tag)"},
	{"custom-command", "/bin/sh command for the custom method"},
	{"custom-timeout", "custom command timeout in seconds"},
	{"package-manager-ack", "true | false: install next to a package-managed binary, into install-dir"},
	{"install-dir", "where to install next to a package-managed binary (default ~/.local/bin)"},
}

func updateConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Show or change how tclaude updates itself (updates.self)"}
	show := boa.CmdT[updateConfigShowParams]{Use: "show", Short: "Show the self-update settings, detected install method and available methods", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *updateConfigShowParams, _ *cobra.Command, _ []string) {
		os.Exit(runUpdateConfigShow(p, cliStdout, cliStderr))
	}}.ToCobra()
	keys := []string{}
	for _, k := range updateConfigKeys {
		keys = append(keys, "  "+k.key+": "+k.help)
	}
	set := boa.CmdT[updateConfigSetParams]{Use: "set <key> <value>", Short: "Change one self-update setting", Long: "Change one self-update setting. Keys:\n" + strings.Join(keys, "\n"), Args: cobra.ExactArgs(2), ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *updateConfigSetParams, _ *cobra.Command, args []string) {
		os.Exit(runUpdateConfigSet(p, args[0], args[1], cliStdout, cliStderr))
	}}.ToCobra()
	cmd.AddCommand(show, set)
	return cmd
}

func updateDaemonFail(err error, stderr io.Writer) int {
	var de *DaemonError
	if errors.As(err, &de) && json.Valid(de.Raw) {
		var body struct {
			Error  string   `json:"error"`
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(de.Raw, &body) == nil && (body.Error != "" || len(body.Errors) > 0) {
			if len(body.Errors) == 0 {
				body.Errors = []string{body.Error}
			}
			for _, e := range body.Errors {
				fmt.Fprintln(stderr, "Error:", termtext.Cell(e))
			}
			return MapDaemonErrorToRC(err)
		}
	}
	fmt.Fprintln(stderr, "Error:", err)
	return MapDaemonErrorToRC(err)
}

func fetchUpdateSettings(node string, stderr io.Writer) (json.RawMessage, *updateSettingsView, int) {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return nil, nil, rc
	}
	var raw json.RawMessage
	if err := DaemonRequest(http.MethodGet, updateSettingsPath(node), nil, &raw, DaemonOpts{Timeout: 20 * time.Second}); err != nil {
		return nil, nil, updateDaemonFail(err, stderr)
	}
	var view updateSettingsView
	if err := json.Unmarshal(raw, &view); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return nil, nil, rcIOFailure
	}
	return raw, &view, 0
}

func runUpdateConfigShow(p *updateConfigShowParams, stdout, stderr io.Writer) int {
	raw, view, rc := fetchUpdateSettings(p.Node, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		_, _ = stdout.Write(append(raw, '\n'))
		return 0
	}
	printUpdateSettings(view, stdout)
	return 0
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func printUpdateSettings(v *updateSettingsView, out io.Writer) {
	s := v.Settings
	c := termtext.Cell
	method := "not chosen (no automatic updates; tclaude update --apply uses the detected method)"
	if s.Method != "" {
		method = s.Method
	}
	fmt.Fprintf(out, "Method:             %s\n", c(method))
	if d := v.Detected; d != nil {
		fmt.Fprintf(out, "Detected:           %s (%s)\n", c(d.Method), c(d.BuildInfo))
		fmt.Fprintf(out, "                    %s\n", c(d.Reason))
		if d.UntrackHint != "" {
			fmt.Fprintf(out, "Untrack with:       %s\n", c(d.UntrackHint))
		}
	} else if v.DetectionError != "" {
		fmt.Fprintf(out, "Detected:           unavailable: %s\n", c(v.DetectionError))
	}
	sched := s.Schedule
	if sched.Frequency == config.ScheduleOff {
		fmt.Fprintf(out, "Schedule:           off\n")
	} else {
		day := ""
		if sched.Frequency == config.ScheduleWeekly {
			day = " " + sched.Weekday
		}
		fmt.Fprintf(out, "Schedule:           %s%s %s-%s local, defer while agents work: %s\n", c(sched.Frequency), c(day), c(sched.WindowStart), c(sched.WindowEnd), onOff(sched.DeferWhileAgentsWorking))
	}
	if v.NextScheduledRun != nil {
		fmt.Fprintf(out, "Next run:           %s\n", v.NextScheduledRun.Local().Format(time.RFC3339))
	}
	if r := v.LastScheduledRun; r != nil {
		fmt.Fprintf(out, "Last automatic run: %s %s: %s %s\n", r.At.Local().Format(time.RFC3339), c(r.Trigger), c(r.Outcome), c(r.Detail))
	}
	fmt.Fprintf(out, "Update on start:    %s\n", onOff(s.UpdateOnStart))
	if s.PostUpdateSetup.Enabled {
		fmt.Fprintf(out, "Post-update setup:  on: tclaude %s\n", c(strings.Join(s.PostUpdateSetup.SetupArgs(), " ")))
	} else {
		fmt.Fprintf(out, "Post-update setup:  off\n")
	}
	if s.SourceCheckout.Path != "" {
		ref := s.SourceCheckout.Ref
		if ref == "" {
			ref = "(release tag)"
		}
		fmt.Fprintf(out, "Source checkout:    %s @ %s\n", c(s.SourceCheckout.Path), c(ref))
	}
	if slices.Contains(v.Redacted, "custom.command") {
		fmt.Fprintf(out, "Custom command:     (set on the node; withheld from peers)\n")
	} else if s.Custom.Command != "" {
		fmt.Fprintf(out, "Custom command:     %s (timeout %ds)\n", c(s.Custom.Command), s.Custom.TimeoutSeconds)
	}
	fmt.Fprintf(out, "Install dir:        %s (package-manager ack: %s)\n", c(v.InstallDir), onOff(s.PackageManagerAck))
	if v.InstallShadowed {
		fmt.Fprintf(out, "                    shadowed on PATH by %s\n", c(v.ShadowedBy))
	}
	for _, w := range v.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", c(w))
	}
	fmt.Fprintln(out, "Methods:")
	for _, m := range v.Methods {
		state := "available"
		if !m.Available {
			state = "unavailable: " + m.UnavailableReason
		}
		line := fmt.Sprintf("  %-16s %s (%s)", m.ID, m.Label, state)
		if m.Command != "" {
			line += " runs: " + m.Command
		}
		fmt.Fprintln(out, c(line))
	}
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "on", "yes", "1":
		return true, nil
	case "false", "off", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", v)
}

// applyUpdateConfigKey sets one key on s; validation is the daemon's (the
// same config.SelfUpdateConfig.Validate the Config tab uses).
func applyUpdateConfigKey(s *config.SelfUpdateConfig, key, value string) error {
	var err error
	switch key {
	case "method":
		if value == "none" {
			value = ""
		}
		s.Method = value
	case "schedule":
		s.Schedule.Frequency = value
	case "weekday":
		s.Schedule.Weekday = strings.ToLower(value)
	case "window":
		start, end, ok := strings.Cut(value, "-")
		if !ok {
			return fmt.Errorf("window must be HH:MM-HH:MM")
		}
		s.Schedule.WindowStart, s.Schedule.WindowEnd = strings.TrimSpace(start), strings.TrimSpace(end)
	case "defer-while-working":
		s.Schedule.DeferWhileAgentsWorking, err = parseBool(value)
	case "update-on-start":
		s.UpdateOnStart, err = parseBool(value)
	case "post-setup":
		s.PostUpdateSetup.Enabled, err = parseBool(value)
	case "setup-flags":
		s.PostUpdateSetup.Flags = []string{}
		for _, f := range strings.Split(value, ",") {
			if f = strings.TrimPrefix(strings.TrimSpace(f), "--"); f != "" {
				s.PostUpdateSetup.Flags = append(s.PostUpdateSetup.Flags, f)
			}
		}
	case "setup-harness":
		s.PostUpdateSetup.Harness = value
	case "checkout-path":
		s.SourceCheckout.Path = value
	case "checkout-ref":
		s.SourceCheckout.Ref = value
	case "custom-command":
		s.Custom.Command = value
	case "custom-timeout":
		s.Custom.TimeoutSeconds, err = strconv.Atoi(value)
	case "package-manager-ack":
		s.PackageManagerAck, err = parseBool(value)
	case "install-dir":
		s.InstallDir = value
	default:
		known := []string{}
		for _, k := range updateConfigKeys {
			known = append(known, k.key)
		}
		return fmt.Errorf("unknown key %q; keys: %s", key, strings.Join(known, ", "))
	}
	return err
}

func runUpdateConfigSet(p *updateConfigSetParams, key, value string, stdout, stderr io.Writer) int {
	_, view, rc := fetchUpdateSettings(p.Node, stderr)
	if rc != 0 {
		return rc
	}
	s := view.Settings.Resolved()
	if err := applyUpdateConfigKey(&s, key, value); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcInvalidArg
	}
	body := map[string]any{"settings": s}
	if p.ConfirmPackageManager {
		body["confirm_package_manager"] = true
	}
	var raw json.RawMessage
	if err := DaemonRequest(http.MethodPut, updateSettingsPath(p.Node), body, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true}); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && de.Code == "confirmation_required" {
			fmt.Fprintf(stderr, "Error: letting tclaude run a package manager is an explicit opt-in; rerun with --confirm-package-manager\n")
			return rcInvalidArg
		}
		return updateDaemonFail(err, stderr)
	}
	if p.JSON {
		_, _ = stdout.Write(append(raw, '\n'))
		return 0
	}
	var next updateSettingsView
	if err := json.Unmarshal(raw, &next); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	printUpdateSettings(&next, stdout)
	return 0
}
