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
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/zalando/go-keyring"
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
	NoStartDaemon    bool   `long:"no-start-daemon" help:"Only open the dashboard; do not start agentd when it is not running"`
	ServeArgs        string `long:"serve-args" optional:"true" help:"The agentd serve flags the launcher starts agentd with, space-separated, one token per flag (e.g. \"--persist-operator-token --dashboard-port=8123 --no-print-human-token\"); kept until changed. A launch profile overrides them"`
	DefaultServeArgs bool   `long:"default-serve-args" help:"Go back to tclaude's default serve flags (--persist-operator-token --dashboard-port=<port> --no-print-human-token)"`
	Node             string `long:"node" optional:"true" help:"Trusted peer label or instance ID; omit for this node (needs the peer's node.read and node.update.settings grants)"`
	JSON             bool   `long:"json" help:"Output JSON"`
}

type serviceLauncherOpenParams struct {
	StartDaemon bool `long:"start-daemon" help:"Start agentd first when it does not answer (through the tclaude autostart unit when one is installed)"`
}

// appLauncherStatus mirrors agentd.NodeAppLauncher.
type appLauncherStatus struct {
	launcher.AppLauncherStatus
	ServeArgs        []string `json:"serve_args"`
	ServeArgsSource  string   `json:"serve_args_source"`
	DefaultServeArgs []string `json:"default_serve_args"`
	Access           string   `json:"access"`
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
				RunFunc: func(p *serviceLauncherInstallParams, cmd *cobra.Command, _ []string) {
					body := map[string]any{"start_daemon": !p.NoStartDaemon}
					switch {
					case p.DefaultServeArgs && cmd.Flags().Changed("serve-args"):
						fmt.Fprintln(cliStderr, "Error: --default-serve-args conflicts with --serve-args")
						os.Exit(rcInvalidArg)
					case p.DefaultServeArgs:
						body["serve_args"] = nil
					case cmd.Flags().Changed("serve-args"):
						body["serve_args"] = append([]string{}, strings.Fields(p.ServeArgs)...)
					}
					os.Exit(runServiceLauncher(http.MethodPost, launcherPath(p.Node, "/install"), body, p.JSON, "App launcher installed", cliStdout, cliStderr))
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
				Use: "open [-- serve flags...]",
				// The serve flags come after --, which the launcher script
				// always writes; nothing else is positional.
				Args: func(cmd *cobra.Command, args []string) error {
					if dash := cmd.ArgsLenAtDash(); len(args) > 0 && dash != 0 {
						return fmt.Errorf("serve flags go after --")
					}
					return nil
				},
				Short:       "Open the dashboard, starting agentd first with --start-daemon and the serve flags after -- (what the app launcher runs)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceLauncherOpenParams, _ *cobra.Command, args []string) {
					d := productionOpenDeps()
					d.ServeArgs = args
					os.Exit(runLauncherOpen(p.StartDaemon, d, cliStdout, cliStderr))
				},
			}.ToCobra(),
			serviceLauncherShellEnvCmd(),
		},
	}.ToCobra()
}

// serviceLauncherShellEnvCmd is what `open` runs inside the user's login
// shell to learn the environment a terminal would give agentd.
func serviceLauncherShellEnvCmd() *cobra.Command {
	c := boa.CmdT[struct{}]{
		Use:         "shell-env",
		Short:       "Print this process's environment for `service launcher open` (internal)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			if err := launcher.PrintShellEnv(os.Stdout); err != nil {
				os.Exit(rcIOFailure)
			}
		},
	}.ToCobra()
	c.Hidden = true
	return c
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
	source := map[string]string{"profile": "from the launch profile", "launcher": "the launcher's own setting", "default": "tclaude's defaults"}[st.ServeArgsSource]
	if source != "" {
		fmt.Fprintf(w, "  serve flags:   %s (%s)\n", termtext.Cell(strings.Join(st.ServeArgs, " ")), source)
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
	Exe string
	// ServeArgs are the serve flags a detached start uses.
	ServeArgs  []string
	DaemonUp   func() bool
	Dashboard  func(stderr io.Writer) int
	Autostart  launcher.Autostart
	Run        launcher.Runner
	StartServe func(argv, env []string, logFile string) error
	// Profile is read for the launch profile's explicit environment; its
	// flags are already in ServeArgs.
	Profile func() (*config.LaunchProfile, error)
	Environ []string
	// LoginEnv returns the environment the user's login shell has, which
	// a detached agentd starts with (merged over Environ); nil or an error
	// keeps Environ alone.
	LoginEnv func(environ []string) ([]string, error)
	// Locale is where a missing UTF-8 locale is chosen from.
	Locale launcher.LocaleSources
	// LogNote appends a line to LogFile, which is where a launcher's
	// output can be read.
	LogNote func(msg string)
	LogFile string
	Notify  func(msg string)
	Sleep   func(time.Duration)
	// WaitFor bounds the wait for a started agentd to answer.
	WaitFor time.Duration
	// UseKeychainToken makes a keychain-persisted operator token the
	// dashboard request's credential when no other is at hand.
	UseKeychainToken func()
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
		LoginEnv: func(environ []string) ([]string, error) {
			return loginShellEnv(exe, name, environ)
		},
		Locale: launcher.SystemLocaleSources(runtime.GOOS),
		LogNote: func(msg string) {
			appendLauncherLog(filepath.Join(home, ".tclaude", "data", "launcher-agentd.log"), msg)
		},
		LogFile:          filepath.Join(home, ".tclaude", "data", "launcher-agentd.log"),
		Notify:           desktopNotify,
		UseKeychainToken: useKeychainOperatorToken,
		Sleep:            time.Sleep,
		// agentd started by the LaunchAgent may first wait for the login
		// shell, up to ShellEnvTimeout.
		WaitFor: 30*time.Second + launcher.ShellEnvTimeout,
	}
}

// runLauncherOpen opens the dashboard. With startDaemon, an agentd that does
// not answer is started first: through the tclaude autostart unit when one
// is installed (so it runs as the service would), else (or when the unit
// does not start) as a detached `tclaude agentd serve` with the serve flags
// the launcher was written with. A failure is shown
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
				if !strings.HasPrefix(how, "detached") {
					where = "the " + how + " log"
				}
				return fail(fmt.Sprintf("tclaude agentd did not answer within %s after starting it (%s); see %s.", d.WaitFor, how, where))
			}
			d.Sleep(step)
		}
	}
	if d.UseKeychainToken != nil {
		d.UseKeychainToken()
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
	var unitErr error
	if st := d.Autostart.Status(ctx); st.Installed && st.Managed {
		how, err := startAutostartUnit(ctx, d)
		if err == nil {
			return how, nil
		}
		// A unit left behind where its supervisor no longer runs (no user
		// systemd, say): start agentd directly rather than not at all.
		unitErr = err
	}
	if d.Exe == "" {
		return "", fmt.Errorf("cannot tell which tclaude binary to run")
	}
	argv := append([]string{d.Exe, "agentd", "serve"}, d.ServeArgs...)
	env := append([]string(nil), d.Environ...)
	var envNote string
	if d.LoginEnv != nil {
		// A launcher has the desktop's bare environment; agentd, and every
		// agent it starts, should have what a terminal has.
		if login, err := d.LoginEnv(env); err != nil {
			envNote = "; without the login shell's environment: " + err.Error()
			if d.LogNote != nil {
				d.LogNote("ERROR tclaude launcher: starting agentd" + envNote)
			}
			// Nobody watches a launcher's terminal: say it on the desktop,
			// and have agentd say it in the dashboard's Messages.
			d.Notify("tclaude: your login shell did not give agentd its environment within " +
				launcher.ShellEnvTimeout.String() + ". agentd started anyway; agents may not start or may show offline. See Messages in the dashboard.")
			env = append(env, launcher.LoginEnvErrorVar+"="+err.Error())
		} else {
			env = launcher.MergeLoginEnv(env, login)
		}
		// The terminal app, not the profile, usually sets the locale; give
		// agentd the user's own UTF-8 one when neither the launcher nor the
		// login shell has it.
		value, source := launcher.ChooseLocale(env, nil, d.Locale)
		if value != "" {
			env = launcher.WithLocale(env, value)
			if d.LogNote != nil {
				d.LogNote("tclaude launcher: no UTF-8 locale; agentd gets " + value + " (from " + source + ")")
			}
		}
	}
	profile, err := d.Profile()
	if err != nil {
		return "", fmt.Errorf("config.json could not be read, so the launch profile is unknown: %w", err)
	}
	if profile != nil {
		// The profile's inherited variables come from this environment,
		// which the launcher script gave PATH; explicit ones override.
		for _, e := range profile.Env {
			env = append(env, e.Key+"="+e.Value)
		}
	}
	if err := d.StartServe(argv, env, d.LogFile); err != nil {
		if unitErr != nil {
			return "", fmt.Errorf("%v; then starting it directly: %w", unitErr, err)
		}
		return "", err
	}
	if unitErr != nil {
		return "detached, after the autostart unit failed: " + unitErr.Error() + envNote, nil
	}
	return "detached" + envNote, nil
}

// loginShellEnv asks the user's login shell for its environment by
// running exe's `service launcher shell-env` in it.
func loginShellEnv(exe, username string, environ []string) ([]string, error) {
	if exe == "" {
		return nil, fmt.Errorf("cannot tell which tclaude binary to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), launcher.ShellEnvTimeout+5*time.Second)
	defer cancel()
	shell := launcher.LoginShell(ctx, runtime.GOOS, environ, username, launcher.ExecRunner)
	argv, err := launcher.LoginShellCommand(shell, []string{exe, "service", "launcher", "shell-env"})
	if err != nil {
		return nil, err
	}
	return launcher.LoginShellEnv(ctx, argv, environ)
}

func startAutostartUnit(ctx context.Context, d openDeps) (string, error) {
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

// keychainOperatorToken reads the OS keychain; tests swap it.
var keychainOperatorToken = keyring.Get

// useKeychainOperatorToken sets TCLAUDE_HUMAN_TOKEN from the OS keychain
// when neither it nor the token file holds the operator token, so a daemon
// that persists its token in the keychain (--persist-operator-token-keychain)
// can be signed in to from the launcher, which has no terminal environment.
// Only `service launcher open` reads the keychain; other commands keep the
// env-or-file lookup. Bounded, so a keychain that never answers does not
// hang the launcher.
func useKeychainOperatorToken() {
	// Never for an agent, and never toward a socket someone else named:
	// the token would go to whatever listens there.
	if hasAgentHint() || agentipc.ExplicitSocketPath() != "" || OperatorToken() != "" {
		return
	}
	ch := make(chan string, 1)
	go func() {
		// agentd's keychainService / keychainUser; keep them in sync.
		tok, err := keychainOperatorToken("tclaude", "operator_token")
		if err != nil {
			tok = ""
		}
		ch <- strings.TrimSpace(tok)
	}()
	select {
	case tok := <-ch:
		if tok != "" {
			_ = os.Setenv(HumanTokenEnvVar, tok)
		}
	// Long enough to answer a keychain prompt; a keychain with no session
	// fails at once rather than waiting.
	case <-time.After(30 * time.Second):
	}
}
