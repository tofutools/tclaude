package agent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

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
	Print bool `long:"print" help:"Print the one-shot URL instead of opening a browser (expires in ~60s)"`
	Slop  bool `long:"slop" help:"Open the dashboard in 🎰 slop machine theme — a purely cosmetic re-skin, same data."`
}

func runDashboard(p *dashboardParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		URL string `json:"url"`
	}
	path := "/v1/dashboard/open"
	if p.Slop {
		path += "?slop=1"
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
func openBrowserURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		if wsl.IsWSL() {
			if cmdExe := findWindowsCmdPath(); cmdExe != "" {
				cmd = exec.Command(cmdExe, "/c", "start", "", url)
				break
			}
			if path, err := exec.LookPath("wslview"); err == nil {
				cmd = exec.Command(path, url)
				break
			}
		}
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

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
