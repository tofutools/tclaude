package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
	"github.com/tofutools/tclaude/pkg/common"
)

// dashboardCmd is `tclaude agent dashboard` — opens the daemon's
// loopback HTTP dashboard in the default browser.
//
// The CLI calls /v1/dashboard/open on the daemon's Unix socket, which
// is human-only (peer-credential auth refuses agents). The daemon
// mints a short-lived, single-use init token and returns a URL with
// it embedded; the browser exchanges that token for the dashboard
// session cookie. This is what keeps the dashboard's admin /api/*
// surface unreachable by agents — see agentd/dashboard.go.
func dashboardCmd() *cobra.Command {
	return boa.CmdT[dashboardParams]{
		Use:         "dashboard",
		Aliases:     []string{"ui"},
		Short:       "Open the agentd browser dashboard",
		Long:        "Asks the daemon (via the human-only /v1/dashboard/open endpoint) for a one-shot dashboard URL and opens it in the default browser. Pass --print to print the URL instead — note it carries a single-use token that expires in ~60s, so use it immediately.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *dashboardParams, _ *cobra.Command, _ []string) {
			os.Exit(runDashboard(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type dashboardParams struct {
	Print  bool `long:"print" help:"Print the one-shot URL instead of opening a browser (expires in ~60s)"`
	Slop   bool `long:"slop" help:"Open the dashboard in 🎰 slop machine theme — a purely cosmetic re-skin, same data."`
	Wizard bool `long:"wizard" help:"Open the dashboard in 🧙 wizard theme — a purely cosmetic re-skin, same data. Mutually exclusive with --slop (slop wins)."`
}

func runDashboard(p *dashboardParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		URL string `json:"url"`
	}
	path := "/v1/dashboard/open"
	// slop and wizard are mutually exclusive re-skins; slop wins if both
	// flags are set, matching the client's applySlopThemeIfRequested.
	if p.Slop {
		path += "?slop=1"
	} else if p.Wizard {
		path += "?wizard=1"
	}
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.URL == "" {
		fmt.Fprintln(stderr, "Error: daemon has no loopback URL bound; the dashboard is unavailable in this process.")
		fmt.Fprintln(stderr, "       Restart the daemon with `tclaude agentd serve` and check the startup banner for the popup port.")
		return rcIOFailure
	}
	if p.Print {
		fmt.Fprintln(stdout, resp.URL)
		return rcOK
	}
	if err := openBrowserURL(resp.URL); err != nil {
		fmt.Fprintf(stderr, "Failed to open browser: %v\nURL: %s\n", err, resp.URL)
		return rcIOFailure
	}
	fmt.Fprintln(stdout, "Opening dashboard in your browser…")
	return rcOK
}

// openBrowserURL mirrors agentd/popup.go's openBrowser. Duplicated
// rather than imported because the agentd package doesn't expose it
// (it's an implementation detail of the daemon's popup spawner) and
// the WSL ordering is the same here. Keep both copies in sync if the
// platform matrix grows.
//
// Start() alone only catches a missing launcher binary. The interesting
// failures are exit codes: xdg-open reports "no method available" (3) or
// "action failed" (4) on a headless/misconfigured host, and open(1) and
// `start` fail similarly — all with a zero-valued Start(). That matters
// most here, where the caller turns the error into a user-visible
// "Failed to open browser" and rcIOFailure; without it the CLI printed
// "Opening dashboard in your browser…" and exited 0 with nothing opened.
//
// The wait is bounded because success is not always a prompt exit: with a
// desktop environment xdg-open delegates and returns immediately, but its
// generic fallback execs the browser in the FOREGROUND and does not
// return until the browser is closed. A launcher still running after
// browserLaunchProbe is treated as a successful launch, and reaped by the
// background goroutine.
//
// cmd.Stderr stays nil (/dev/null) so cmd.Wait() tracks the LAUNCHER and
// not the browser it forked — see openBrowser in agentd/popup.go for the
// full reasoning; launcherExitHint supplies the diagnostic instead.
func openBrowserURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", escapeForCmdExe(url))
	default:
		if wsl.IsWSL() {
			if cmdExe := findWindowsCmdPath(); cmdExe != "" {
				cmd = exec.Command(cmdExe, "/c", "start", "", escapeForCmdExe(url))
				break
			}
			if path, err := exec.LookPath("wslview"); err == nil {
				cmd = exec.Command(path, url)
				break
			}
		}
		cmd = exec.Command("xdg-open", url)
	}
	name := filepath.Base(cmd.Path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if hint := launcherExitHint(name, exit.ExitCode()); hint != "" {
				return fmt.Errorf("%s: %w: %s", name, err, hint)
			}
		}
		return fmt.Errorf("%s: %w", name, err)
	case <-time.After(browserLaunchProbe):
		return nil
	}
}

// browserLaunchProbe bounds how long openBrowserURL waits for a launcher to
// exit before calling the launch good. Real launcher failures are immediate
// (bad URL, no handler, no display); anything still alive this long has
// handed off or is fronting the browser itself.
const browserLaunchProbe = 2 * time.Second

// launcherExitHint turns a launcher's exit status into the human-readable
// cause the CLI prints after "Failed to open browser: ". Only xdg-open
// documents a status table — open(1) and cmd.exe's `start` do not, so they
// get "" and the bare status. Duplicated from agentd/popup.go.
func launcherExitHint(name string, code int) string {
	if name != "xdg-open" {
		return ""
	}
	switch code {
	case 1:
		return "error in command line syntax"
	case 2:
		return "the file or URL does not exist"
	case 3:
		return "no method available for opening it (no browser or desktop opener found)"
	case 4:
		return "the opening action failed"
	}
	return ""
}

// escapeForCmdExe escapes cmd.exe metacharacters (`^&<>|`) by prefixing
// each with `^`. Without this `cmd /c start "" URL` splits the command
// line at `&`, dropping the rest of the URL — exactly what happens to
// `http://…?init_token=X&slop=1` on WSL and native Windows, where the
// browser ends up at `…?init_token=X` and the slop theme never
// activates. wslview and xdg-open don't parse the URL through a shell,
// so they get the raw string unchanged.
//
// Order matters: `^` must be in the replacer table so an existing `^`
// in the URL doesn't get reinterpreted as an escape lead-in. The
// stdlib NewReplacer processes the input left-to-right without
// re-scanning its own output, so `^&` → `^^^&` (literal `^` then
// literal `&`) — correct.
//
// Duplicated from agentd/popup.go — see openBrowser there. Keep both
// copies in sync.
func escapeForCmdExe(s string) string {
	return cmdExeEscaper.Replace(s)
}

var cmdExeEscaper = strings.NewReplacer(
	"^", "^^",
	"&", "^&",
	"<", "^<",
	">", "^>",
	"|", "^|",
)

func findWindowsCmdPath() string {
	for _, p := range []string{
		"/mnt/c/Windows/System32/cmd.exe",
		"/mnt/c/Windows/SysWOW64/cmd.exe",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
