package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/launcher"
)

type serviceStatusParams struct {
	Refresh bool   `long:"refresh" help:"Detect the launcher again instead of reading the daemon's cached detection"`
	Node    string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read grant)"`
	JSON    bool   `long:"json" help:"Output JSON"`
}

type serviceRestartParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read and node.update.settings grants)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

type serviceInstallParams struct {
	NoArgs    bool   `long:"no-args" help:"Run serve with no flags instead of keeping the observed ones"`
	NoKeepEnv bool   `long:"no-keep-env" help:"Do not carry the launcher's observed environment and EnvironmentFiles into the unit (PATH is always captured)"`
	Node      string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read and node.update.settings grants; only an unrestricted peer may choose serve flags)"`
	JSON      bool   `long:"json" help:"Output JSON"`
}

type serviceUninstallParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read and node.update.settings grants)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

// serviceStatus mirrors agentd.NodeService; agent cannot import agentd.
type serviceStatus struct {
	Launcher       launcher.Info            `json:"launcher"`
	Autostart      launcher.AutostartStatus `json:"autostart"`
	RestartPending string                   `json:"restart_pending,omitempty"`
	Access         string                   `json:"access"`
	Redacted       []string                 `json:"redacted,omitempty"`
}

// servicePath is this node's service API, or a trusted peer's.
func servicePath(node, tail string) string {
	if node == "" {
		return "/v1/node/service" + tail
	}
	return "/v1/federation/peer/" + url.PathEscape(node) + "/node/service" + tail
}

type serviceRestartResult struct {
	Method   string `json:"method"`
	Launcher string `json:"launcher"`
}

// ServiceCmd is `tclaude service`: how agentd is launched, and restarting it
// through that launcher.
func ServiceCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "service",
		Short:       "Show how agentd is launched, restart it through that launcher, and start it at boot (operator only)",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boa.CmdT[serviceStatusParams]{
				Use:         "status",
				Short:       "Show the running agentd's launcher: kind, unit or label, observed arguments and environment (secrets redacted)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceStatusParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceStatus(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceRestartParams]{
				Use:         "restart",
				Short:       "Restart agentd gracefully, through systemd or launchd when it runs under one, keeping its launch arguments",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceRestartParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceRestart(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceInstallParams]{
				Use:   "install [-- serve flags...]",
				Short: "Start agentd at boot (systemd user unit) or login (macOS LaunchAgent), keeping the observed launch arguments and environment",
				Long: "Writes ~/.config/systemd/user/" + launcher.SystemdUnitName + " (with a 0600 ~/.config/tclaude/agentd.env holding PATH and the kept environment) or ~/Library/LaunchAgents/" + launcher.LaunchdLabel + ".plist, and enables it. It does not start it next to the running agentd; it takes effect at the next boot or login. " +
					"By default the unit runs the serve flags the running agentd was started with; pass different ones after `--` (e.g. `tclaude service install -- --dashboard-port 7878 --no-tray`), or --no-args for none. " +
					"Re-running rewrites the unit; a unit tclaude did not write is never overwritten. On Linux, starting at boot rather than login needs lingering, which needs root: the command prints the `sudo loginctl enable-linger` line to run.",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceInstallParams, _ *cobra.Command, args []string) {
					os.Exit(runServiceInstall(p, args, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceUninstallParams]{
				Use:         "uninstall",
				Short:       "Disable and remove the autostart unit tclaude wrote (a running agentd keeps running)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceUninstallParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceAutostart(map[string]any{"action": "uninstall"}, p.Node, p.JSON, cliStdout, cliStderr))
				},
			}.ToCobra(),
			serviceLaunchCmd(),
		},
	}.ToCobra()
}

func runServiceInstall(p *serviceInstallParams, args []string, stdout, stderr io.Writer) int {
	req := map[string]any{"action": "install"}
	if p.NoArgs && len(args) > 0 {
		fmt.Fprintln(stderr, "Error: --no-args conflicts with serve flags after --")
		return rcInvalidArg
	}
	if p.NoArgs {
		req["serve_args"] = []string{}
	} else if len(args) > 0 {
		req["serve_args"] = args
	}
	if p.NoKeepEnv {
		req["keep_env"] = false
	}
	return runServiceAutostart(req, p.Node, p.JSON, stdout, stderr)
}

func runServiceAutostart(req map[string]any, node string, asJSON bool, stdout, stderr io.Writer) int {
	raw, rc := serviceRequest(http.MethodPost, servicePath(node, "/autostart"), req, stderr)
	if rc != 0 {
		return rc
	}
	if asJSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var st serviceStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	if req["action"] == "uninstall" {
		fmt.Fprintln(stdout, "Autostart removed.")
	} else {
		fmt.Fprintf(stdout, "Autostart installed: %s\n", termtext.Cell(st.Autostart.Path))
		fmt.Fprintln(stdout, "It takes effect at the next boot or login; `tclaude service status` shows it.")
	}
	printAutostart(st.Autostart, envWithheld(st), stdout)
	return 0
}

// envWithheld reports a peer's view that leaves out environment values.
func envWithheld(st serviceStatus) bool { return len(st.Redacted) > 0 }

func envLine(e launcher.EnvVar, withheld bool) string {
	if withheld {
		return "  " + termtext.Cell(e.Key) + "=(withheld from peers)"
	}
	return "  " + termtext.Cell(e.Key) + "=" + termtext.Cell(e.Value)
}

func printAutostart(a launcher.AutostartStatus, withheld bool, w io.Writer) {
	if !a.Supported {
		fmt.Fprintln(w, "Autostart:       not supported on this platform")
		return
	}
	state := "not installed"
	switch {
	case a.Installed && !a.Managed:
		state = "a unit tclaude did not write is installed"
	case a.Installed && a.Enabled:
		state = "installed, enabled"
	case a.Installed:
		state = "installed, disabled"
	}
	fmt.Fprintf(w, "Autostart:       %s (%s)\n", state, termtext.Cell(a.Path))
	if len(a.Argv) > 0 {
		fmt.Fprintf(w, "  command:       %s\n", termtext.Cell(strings.Join(a.Argv, " ")))
	}
	for _, e := range a.Env {
		fmt.Fprintln(w, envLine(e, withheld))
	}
	if a.Linger != nil && !a.Linger.Enabled {
		fmt.Fprintf(w, "  lingering is off: to start at boot rather than login, run: %s\n", termtext.Cell(a.Linger.Command))
	}
	for _, warning := range a.Warnings {
		fmt.Fprintln(w, "Warning:", termtext.Cell(warning))
	}
}

func serviceRequest(method, path string, in any, stderr io.Writer) (json.RawMessage, int) {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return nil, rc
	}
	var raw json.RawMessage
	if err := DaemonRequest(method, path, in, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true, RetryOutput: stderr}); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && json.Valid(de.Raw) {
			fmt.Fprintln(stderr, string(de.Raw))
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
		return nil, MapDaemonErrorToRC(err)
	}
	return raw, 0
}

func runServiceStatus(p *serviceStatusParams, stdout, stderr io.Writer) int {
	path := servicePath(p.Node, "")
	if p.Refresh {
		path += "?refresh=1"
	}
	raw, rc := serviceRequest(http.MethodGet, path, nil, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var st serviceStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	printServiceStatus(st, stdout)
	return 0
}

func printServiceStatus(st serviceStatus, w io.Writer) {
	l := st.Launcher
	fmt.Fprintf(w, "Launcher:        %s\n", termtext.Cell(l.Kind))
	if l.Unit != "" {
		fmt.Fprintf(w, "Unit:            %s\n", termtext.Cell(l.Unit))
	}
	if l.Label != "" {
		fmt.Fprintf(w, "Label:           %s/%s\n", termtext.Cell(l.Domain), termtext.Cell(l.Label))
	}
	if l.UnitPath != "" {
		fmt.Fprintf(w, "Unit file:       %s\n", termtext.Cell(l.UnitPath))
	}
	if l.Kind == launcher.KindSystemdUser || l.Kind == launcher.KindSystemdSystem || l.Kind == launcher.KindLaunchd {
		fmt.Fprintf(w, "Managed:         %v\n", l.ManagedByTclaude)
	}
	if len(l.Argv) > 0 {
		fmt.Fprintf(w, "Unit command:    %s\n", termtext.Cell(strings.Join(l.Argv, " ")))
	}
	fmt.Fprintf(w, "Process command: %s\n", termtext.Cell(strings.Join(l.ProcessArgv, " ")))
	if l.RestartPolicy != "" {
		fmt.Fprintf(w, "Restart policy:  %s\n", termtext.Cell(l.RestartPolicy))
	}
	if l.KillMode != "" {
		fmt.Fprintf(w, "KillMode:        %s\n", termtext.Cell(l.KillMode))
	}
	for _, f := range l.EnvironmentFiles {
		fmt.Fprintf(w, "Env file:        %s\n", termtext.Cell(f))
	}
	for _, e := range l.Env {
		fmt.Fprintln(w, envLine(e, envWithheld(st)))
	}
	if t := l.Tmux; t != nil {
		where := "not running"
		if t.Running {
			where = "in its own cgroup " + t.Cgroup
			if t.InUnit {
				where = "inside agentd's unit (" + t.Cgroup + ")"
			}
		}
		fmt.Fprintf(w, "Tmux server:     %s\n", termtext.Cell(where))
	}
	method := l.RestartMethod
	if method == launcher.RestartExit {
		method = fmt.Sprintf("exit (status %d, Restart=%s starts it again)", l.RestartExitCode, l.RestartPolicy)
	}
	fmt.Fprintf(w, "Restart method:  %s\n", termtext.Cell(method))
	if st.RestartPending != "" {
		fmt.Fprintf(w, "Restart pending: %s\n", termtext.Cell(st.RestartPending))
	}
	for _, warning := range l.Warnings {
		fmt.Fprintln(w, "Warning:", termtext.Cell(warning))
	}
	printAutostart(st.Autostart, envWithheld(st), w)
	if st.Access == "read" {
		fmt.Fprintln(w, "Access:          read only (restart and autostart need the node's node.update.settings grant)")
	}
}

func runServiceRestart(p *serviceRestartParams, stdout, stderr io.Writer) int {
	raw, rc := serviceRequest(http.MethodPost, servicePath(p.Node, "/restart"), struct{}{}, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var res serviceRestartResult
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	how := "re-executing in place with the same arguments"
	switch res.Method {
	case launcher.RestartSupervisor:
		how = "through " + res.Launcher
	case launcher.RestartExit:
		how = "by exiting; " + res.Launcher + " starts it again through the unit's own command"
	}
	fmt.Fprintf(stdout, "agentd is restarting %s.\n", termtext.Cell(how))
	return 0
}
