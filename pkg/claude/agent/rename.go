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

// renameCmd is `tclaude agent rename "<title>"` — invokes the daemon's
// /v1/whoami/rename endpoint, which (when the caller has the
// `self.rename` permission) injects `/rename <title>` into the caller's
// own CC pane via tmux send-keys.
//
// `--target <selector>` swaps the action to ANOTHER agent — the
// manager pattern. Cross-agent calls require `agent.rename`, OR the
// caller being an owner of a group containing the target.
//
// Permissions live in ~/.tclaude/config.json under `agent.default_permissions`
// or in SQLite via `tclaude agent permissions grant`. The human controls
// them.
func renameCmd() *cobra.Command {
	return boa.CmdT[renameParams]{
		Use:         "rename",
		Short:       "Rename a conversation (self by default, or another with --target)",
		Long:        "Asks tclaude agentd to inject `/rename <title>` into a CC pane. By default the target is the calling agent itself (requires `self.rename`). Use --target <selector> to rename ANOTHER agent — the manager pattern (requires `agent.rename`, or being an owner of a group containing the target).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *renameParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *renameParams, _ *cobra.Command, _ []string) {
			os.Exit(runRename(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type renameParams struct {
	Title    string `pos:"true" optional:"true" help:"New conversation title (1-64 chars, [A-Za-z0-9_\\-\\[\\]{}() ] only; single spaces OK, no doubles). Omit when --auto is set."`
	Target   string `long:"target" optional:"true" help:"Rename ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.rename permission, or being an owner of a group containing the target."`
	Auto     bool   `long:"auto" help:"Ask the agent to choose its own title — queues an inbox instruction prompting it to call 'tclaude agent rename' with a 3-4-word kebab-case slug. Mutually exclusive with the positional title."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny. Self-target only."`
}

// isValidRenameTitle mirrors the daemon-side check in handlers.go.
// Kept in sync deliberately: the daemon is the actual security boundary,
// but we want a fast local error before sending a doomed request.
func isValidRenameTitle(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	if strings.Contains(t, "  ") {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		case r == '[' || r == ']' || r == '{' || r == '}':
		case r == '(' || r == ')':
		case r == ' ':
		default:
			return false
		}
	}
	return true
}

func runRename(p *renameParams, stdout, stderr io.Writer) int {
	title := strings.TrimSpace(p.Title)
	target := strings.TrimSpace(p.Target)
	if p.Auto && title != "" {
		fmt.Fprintln(stderr, "Error: --auto and a positional title are mutually exclusive — pick one.")
		return rcInvalidArg
	}
	if !p.Auto && !isValidRenameTitle(title) {
		fmt.Fprintln(stderr, "Error: REJECTED. Title must be 1-64 characters from [A-Za-z0-9_-[]{}() ].")
		fmt.Fprintln(stderr, "Single ASCII spaces are allowed; consecutive spaces, tabs, newlines, slashes,")
		fmt.Fprintln(stderr, "quotes, and unicode are NOT allowed and will not be allowed.")
		fmt.Fprintln(stderr, "This is a hard security gate against keystroke injection — not a style preference.")
		fmt.Fprintln(stderr, "Do not retry with a similar title; pick one that uses only the allowed characters.")
		return rcInvalidArg
	}
	if target != "" && p.AskHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	var resp struct {
		ConvID        string `json:"conv_id"`
		CallerConv    string `json:"caller_conv,omitempty"`
		CallerAgentID string `json:"caller_agent_id,omitempty"`
		Title         string `json:"title"`
		Auto          bool   `json:"auto,omitempty"`
		Note          string `json:"note,omitempty"`
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/whoami/rename"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/rename"
	}
	body := map[string]any{}
	if p.Auto {
		body["auto"] = true
	} else {
		body["title"] = title
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Auto {
		if resp.CallerConv != "" {
			fmt.Fprintf(stdout, "Auto-rename nudge sent to %s (by %s); the agent will pick its own title.\n",
				short(resp.ConvID), shortAgentID(resp.CallerAgentID, resp.CallerConv))
		} else {
			fmt.Fprintf(stdout, "Auto-rename nudge sent; the agent will pick its own title.\n")
		}
	} else if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "Renamed %s to %q (called by %s)\n", short(resp.ConvID), resp.Title, shortAgentID(resp.CallerAgentID, resp.CallerConv))
	} else {
		fmt.Fprintf(stdout, "Renamed %s to %q\n", short(resp.ConvID), resp.Title)
	}
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}
