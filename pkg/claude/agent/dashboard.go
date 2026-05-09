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
// loopback HTTP dashboard in the default browser. The URL is the
// same loopback base the human-approval popup uses, so this is also
// where pending approvals will show up inline once that view ships.
//
// The CLI fetches the URL from /v1/info rather than guessing the
// port; the daemon binds :0 and reports the chosen port on startup.
func dashboardCmd() *cobra.Command {
	return boa.CmdT[dashboardParams]{
		Use:         "dashboard",
		Aliases:     []string{"ui"},
		Short:       "Open the agentd browser dashboard",
		Long:        "Looks up the daemon's loopback URL via /v1/info and opens it in the default browser. Pass --print to just print the URL (useful for scripts / piping into another opener).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *dashboardParams, _ *cobra.Command, _ []string) {
			os.Exit(runDashboard(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type dashboardParams struct {
	Print bool `long:"print" help:"Print the URL instead of opening a browser"`
}

func runDashboard(p *dashboardParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var info struct {
		PopupBaseURL string `json:"popup_base_url"`
	}
	if err := DaemonGet("/v1/info", &info); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if info.PopupBaseURL == "" {
		fmt.Fprintln(stderr, "Error: daemon has no loopback URL bound; the dashboard is unavailable in this process.")
		fmt.Fprintln(stderr, "       Restart the daemon with `tclaude agentd serve` and check the startup banner for the popup port.")
		return rcIOFailure
	}
	url := info.PopupBaseURL + "/"
	if p.Print {
		fmt.Fprintln(stdout, url)
		return rcOK
	}
	if err := openBrowserURL(url); err != nil {
		fmt.Fprintf(stderr, "Failed to open browser: %v\nURL: %s\n", err, url)
		return rcIOFailure
	}
	fmt.Fprintf(stdout, "Opening dashboard at %s\n", url)
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
