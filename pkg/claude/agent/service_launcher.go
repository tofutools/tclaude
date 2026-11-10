package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// `tclaude service launcher`: the app launcher (a desktop entry on Linux,
// ~/Applications/tclaude.app on macOS) that opens the dashboard. The daemon
// writes it (/v1/node/launcher), so it runs the daemon's tclaude binary.
// `open` is what the launcher runs.

type serviceLauncherParams struct {
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (status needs the peer's node.read grant, install and uninstall node.update.settings too)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

type serviceLauncherInstallParams struct {
	NoStartDaemon bool   `long:"no-start-daemon" help:"Only open the dashboard; do not start agentd when it is not running"`
	Node          string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read and node.update.settings grants)"`
	JSON          bool   `long:"json" help:"Output JSON"`
}

type serviceLauncherOpenParams struct {
	StartDaemon bool `long:"start-daemon" help:"Start agentd first when it does not answer (through the tclaude autostart unit when one is installed)"`
}

// appLauncherStatus mirrors agentd.NodeAppLauncher.
type appLauncherStatus struct {
	launcher.AppLauncherStatus
	Access string `json:"access"`
}

func launcherPath(node, tail string) string {
	if node == "" {
		return "/v1/node/launcher" + tail
	}
	return "/v1/federation/peer/" + url.PathEscape(node) + "/node/launcher" + tail
}

func serviceLauncherCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "launcher",
		Short:       "Add tclaude to the desktop's app launcher (Linux .desktop entry, macOS ~/Applications/tclaude.app)",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boa.CmdT[serviceLauncherParams]{
				Use:         "status",
				Short:       "Show whether the app launcher is installed, and which tclaude binary it runs",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLauncherParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLauncher(http.MethodGet, launcherPath(p.Node, ""), nil, p.JSON, "", cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLauncherInstallParams]{
				Use:   "install",
				Short: "Install (or rewrite) the app launcher that opens the dashboard, starting agentd first when it is not running",
				Long: "Linux: writes $XDG_DATA_HOME/applications/tclaude.desktop (default ~/.local/share), its icon and a launcher script. " +
					"macOS: writes ~/Applications/tclaude.app, a script bundle (no signing needed) marked as native on Apple Silicon, and registers it with Launch Services so Spotlight finds it. " +
					"The launcher runs the tclaude binary the daemon runs; agentd rewrites it when that binary moves or is updated. A launcher tclaude did not write is never overwritten.",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLauncherInstallParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLauncher(http.MethodPost, launcherPath(p.Node, "/install"), map[string]any{"start_daemon": !p.NoStartDaemon}, p.JSON, "App launcher installed", cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLauncherParams]{
				Use:         "uninstall",
				Short:       "Remove the app launcher tclaude wrote",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLauncherParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceLauncher(http.MethodPost, launcherPath(p.Node, "/uninstall"), struct{}{}, p.JSON, "App launcher removed", cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceLauncherOpenParams]{
				Use:         "open",
				Short:       "Open the dashboard, starting agentd first with --start-daemon (what the app launcher runs)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLauncherOpenParams, _ *cobra.Command, _ []string) {
					os.Exit(runLauncherOpen(p.StartDaemon, productionOpenDeps(), cliStdout, cliStderr))
				},
			}.ToCobra(),
		},
	}.ToCobra()
}

func runServiceLauncher(method, path string, body any, asJSON bool, done string, stdout, stderr io.Writer) int {
	raw, rc := serviceRequest(method, path, body, stderr)
	if rc != 0 {
		return rc
	}
	if asJSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var st appLauncherStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	if done != "" {
		fmt.Fprintln(stdout, done+".")
	}
	printAppLauncher(st, stdout)
	return 0
}

func printAppLauncher(st appLauncherStatus, w io.Writer) {
	if !st.Supported {
		fmt.Fprintln(w, "App launcher:    not supported on this platform")
		return
	}
	state := "not installed"
	switch {
	case st.Installed && !st.Managed:
		state = "a launcher tclaude did not write is installed"
	case st.Installed && st.Stale:
		state = "installed, out of date"
	case st.Installed:
		state = "installed"
	}
	fmt.Fprintf(w, "App launcher:    %s (%s)\n", state, termtext.Cell(st.Path))
	if st.Exe != "" {
		fmt.Fprintf(w, "  runs:          %s\n", termtext.Cell(st.Exe))
		fmt.Fprintf(w, "  start agentd:  %v\n", st.Options.StartDaemon)
	}
	if st.Stale {
		fmt.Fprintf(w, "  out of date:   %s (agentd rewrites it at its next start; `tclaude service launcher install` does it now)\n", termtext.Cell(st.StaleReason))
	}
	for _, warning := range st.Warnings {
		fmt.Fprintln(w, "Warning:", termtext.Cell(warning))
	}
	if st.Access == "read" {
		fmt.Fprintln(w, "Access:          read only (install and uninstall need the node's node.update.settings grant)")
	}
}

// openDeps is what `service launcher open` reaches outside the process;
// tests swap all of it.
type openDeps struct {
	GOOS string
	UID  int
	// Exe is this tclaude binary, which starts agentd when no unit does.
	Exe        string
	DaemonUp   func() bool
	Dashboard  func(stderr io.Writer) int
	Autostart  launcher.Autostart
	Run        launcher.Runner
	StartServe func(argv, env []string, logFile string) error
	Profile    func() (*config.LaunchProfile, error)
	Environ    []string
	LogFile    string
	Notify     func(msg string)
	Sleep      func(time.Duration)
	// WaitFor bounds the wait for a started agentd to answer.
	WaitFor time.Duration
}

func productionOpenDeps() openDeps {
	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return openDeps{
		GOOS: runtime.GOOS, UID: os.Getuid(), Exe: exe,
		DaemonUp: DaemonAvailable,
		Dashboard: func(stderr io.Writer) int {
			return runDashboard(&dashboardParams{}, io.Discard, stderr)
		},
		Autostart: launcher.Autostart{GOOS: runtime.GOOS, UID: os.Getuid(), User: name,
			Paths: launcher.AutostartPaths(runtime.GOOS, home, os.Getenv("XDG_CONFIG_HOME")),
			Run:   launcher.ExecRunner, FS: launcher.OSFS{}},
		Run:        launcher.ExecRunner,
		StartServe: startDetachedServe,
		Profile: func() (*config.LaunchProfile, error) {
			cfg, err := config.Load()
			if err != nil {
				return nil, err
			}
			return cfg.ServiceLaunch(), nil
		},
		Environ: os.Environ(),
		LogFile: filepath.Join(home, ".tclaude", "data", "launcher-agentd.log"),
		Notify:  desktopNotify,
		Sleep:   time.Sleep,
		WaitFor: 30 * time.Second,
	}
}

// runLauncherOpen opens the dashboard. With startDaemon, an agentd that does
// not answer is started first: through the tclaude autostart unit when one
// is installed (so it runs as the service would), else as a detached
// `tclaude agentd serve` with the launch profile's flags. A failure is shown
// as a desktop notification too, since a launcher has no terminal.
func runLauncherOpen(startDaemon bool, d openDeps, stdout, stderr io.Writer) int {
	fail := func(msg string) int {
		fmt.Fprintln(stderr, "Error:", msg)
		d.Notify(msg)
		return rcIOFailure
	}
	if !d.DaemonUp() {
		if !startDaemon {
			return fail("tclaude agentd is not running. Start it with `tclaude agentd serve`, or install the launcher with the option to start it.")
		}
		how, err := startDaemonForLauncher(d)
		if err != nil {
			return fail("Could not start tclaude agentd: " + err.Error())
		}
		fmt.Fprintf(stdout, "Starting agentd (%s)…\n", how)
		deadline := d.WaitFor
		for step := 250 * time.Millisecond; !d.DaemonUp(); deadline -= step {
			if deadline <= 0 {
				where := d.LogFile
				if how != "detached" {
					where = "the " + how + " log"
				}
				return fail(fmt.Sprintf("tclaude agentd did not answer within %s after starting it (%s); see %s.", d.WaitFor, how, where))
			}
			d.Sleep(step)
		}
	}
	var errBuf bytes.Buffer
	if rc := d.Dashboard(io.MultiWriter(stderr, &errBuf)); rc != rcOK {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errBuf.String()), "Error:"))
		if msg == "" {
			msg = "the dashboard could not be opened"
		}
		d.Notify("tclaude: " + msg)
		return rc
	}
	return rcOK
}

// startDaemonForLauncher starts agentd and says how.
func startDaemonForLauncher(d openDeps) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if st := d.Autostart.Status(ctx); st.Installed && st.Managed {
		if d.GOOS == "darwin" {
			target := "gui/" + strconv.Itoa(d.UID)
			if _, err := d.Run(ctx, "launchctl", "kickstart", target+"/"+launcher.LaunchdLabel); err == nil {
				return "launchd", nil
			}
			// Not loaded (installed since login, or booted out): load it;
			// RunAtLoad starts it.
			if _, err := d.Run(ctx, "launchctl", "bootstrap", target, d.Autostart.Paths.Unit); err != nil {
				return "", fmt.Errorf("launchctl bootstrap %s: %w", d.Autostart.Paths.Unit, err)
			}
			return "launchd", nil
		}
		if _, err := d.Run(ctx, "systemctl", "--user", "start", launcher.SystemdUnitName); err != nil {
			return "", fmt.Errorf("systemctl --user start %s: %w", launcher.SystemdUnitName, err)
		}
		return "systemd", nil
	}
	if d.Exe == "" {
		return "", fmt.Errorf("cannot tell which tclaude binary to run")
	}
	argv := []string{d.Exe, "agentd", "serve"}
	env := append([]string(nil), d.Environ...)
	profile, err := d.Profile()
	if err != nil {
		return "", fmt.Errorf("config.json could not be read, so the launch profile is unknown: %w", err)
	}
	if profile != nil {
		argv = append(argv, profile.Flags...)
		// The profile's inherited variables come from this environment,
		// which the launcher script gave PATH; explicit ones override.
		for _, e := range profile.Env {
			env = append(env, e.Key+"="+e.Value)
		}
	}
	if err := d.StartServe(argv, env, d.LogFile); err != nil {
		return "", err
	}
	return "detached", nil
}

// desktopNotify shows msg as a desktop notification, best effort. The
// message travels as an argument, never as script text.
func desktopNotify(msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		_, _ = launcher.ExecRunner(ctx, "osascript", "-e", "on run argv", "-e", `display notification (item 1 of argv) with title "tclaude"`, "-e", "end run", msg)
		return
	}
	_, _ = launcher.ExecRunner(ctx, "notify-send", "--app-name=tclaude", "tclaude", msg)
}
