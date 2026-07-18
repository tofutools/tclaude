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

// dirCmd is `tclaude agent dir [selector]` — reports a directory an
// agent is working in, or (with --open) asks the daemon to spawn a
// terminal window there.
//
// Three directories are tracked per agent:
//
//   - the CURRENT working dir — the directory of the most-recent file
//     the agent edited, recorded by the PostToolUse hook. This is the
//     default: where the agent is actually building, which is often
//     NOT where Claude Code was launched.
//   - the WORKTREE dir (--worktree) — the git working-tree root that
//     contains the current dir (a linked-worktree root, or the main
//     repo root). Falls back to the start dir when the current dir
//     isn't in a git repo.
//   - the START dir (--start) — where Claude Code was launched.
//
// With no selector the target is the calling agent itself (resolved by
// the daemon from the socket peer's process tree). Pass a selector
// (title / conv-id / 8+-char prefix) to query another agent — useful
// from a manager agent or for a human at the CLI.
func dirCmd() *cobra.Command {
	return boa.CmdT[dirParams]{
		Use:   "dir",
		Short: "Report, repair, or open an agent directory",
		Long: "Prints a directory an agent is working in. With no selector the target\n" +
			"is the calling agent itself.\n\n" +
			"  tclaude agent dir                 # current working dir of self\n" +
			"  tclaude agent dir --worktree      # git worktree/repo root of self\n" +
			"  tclaude agent dir --start         # launch dir of self\n" +
			"  tclaude agent dir --repair        # recreate a deleted launch dir\n" +
			"  tclaude agent dir worker-1        # current working dir of worker-1\n" +
			"  tclaude agent dir --open          # open a terminal there instead\n\n" +
			"Opening a terminal and repairing a deleted startup directory go through\n" +
			"tclaude agentd, which runs outside the agent sandbox.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *dirParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *dirParams, _ *cobra.Command, _ []string) {
			os.Exit(runDir(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type dirParams struct {
	Selector string `pos:"true" optional:"true" help:"Agent to query: title, conv-id, or 8+-char prefix. Omit for self."`
	Start    bool   `long:"start" help:"Use the launch directory (where Claude Code started)."`
	Worktree bool   `long:"worktree" help:"Use the git worktree/repo root containing the current working dir (falls back to the launch dir if not in a git repo)."`
	Open     bool   `long:"open" help:"Open a terminal window in the directory (via tclaude agentd) instead of printing it."`
	Repair   bool   `long:"repair" help:"Recreate this agent's recorded startup directory via agentd. Accepts no selector or path."`
}

// whichDir maps the flags to the daemon's which selector. --worktree
// wins over --start so a caller that passes both gets the more
// specific answer; neither flag means the current working dir.
func (p *dirParams) whichDir() string {
	switch {
	case p.Worktree:
		return "worktree"
	case p.Start:
		return "start"
	default:
		return "current"
	}
}

func runDir(p *dirParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if p.Repair && (selector != "" || p.Open || p.Start || p.Worktree) {
		fmt.Fprintln(stderr, "Error: --repair targets only this agent's recorded startup directory and cannot be combined with a selector, --open, --start, or --worktree")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if p.Repair {
		var resp struct {
			Dir      string `json:"dir"`
			Repaired bool   `json:"repaired"`
		}
		if err := DaemonRequest(http.MethodPost, "/v1/whoami/dir/repair", struct{}{}, &resp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		if resp.Repaired {
			fmt.Fprintf(stdout, "Recreated startup directory: %s\n", resp.Dir)
		} else {
			fmt.Fprintf(stdout, "Startup directory already exists: %s\n", resp.Dir)
		}
		return rcOK
	}
	path := "/v1/whoami/dir"
	if selector != "" {
		path = "/v1/agent/" + url.PathEscape(selector) + "/dir"
	}
	which := p.whichDir()

	if p.Open {
		var resp struct {
			AgentID    string `json:"agent_id,omitempty"`
			ConvID     string `json:"conv_id"`
			Dir        string `json:"dir"`
			CallerConv string `json:"caller_conv,omitempty"`
		}
		if err := DaemonPost(path, map[string]string{"which": which}, &resp); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		if resp.CallerConv != "" {
			fmt.Fprintf(stdout, "Opened a terminal in %s for %s\n", resp.Dir, shortAgentID(resp.AgentID, resp.ConvID))
		} else {
			fmt.Fprintf(stdout, "Opened a terminal in %s\n", resp.Dir)
		}
		return rcOK
	}

	var resp struct {
		ConvID      string `json:"conv_id"`
		StartDir    string `json:"start_dir"`
		CurrentDir  string `json:"current_dir"`
		WorktreeDir string `json:"worktree_dir"`
		Source      string `json:"source"`
	}
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	dir := resp.CurrentDir
	switch which {
	case "start":
		dir = resp.StartDir
	case "worktree":
		dir = resp.WorktreeDir
	}
	if dir == "" {
		fmt.Fprintln(stderr, "Error: no known directory for this agent yet")
		return rcNotFound
	}
	// Print the bare path so the output stays pipeable (e.g. `cd "$(tclaude agent dir)"`).
	fmt.Fprintln(stdout, dir)
	return rcOK
}
