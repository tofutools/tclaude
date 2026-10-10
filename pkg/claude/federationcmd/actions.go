package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type actionParams struct {
	Action     string `pos:"true" help:"message|spawn|spawn-status|stop|retire|clone|move|teleport"`
	Node       string `long:"node" help:"Receiving trusted peer label or stable ID"`
	Agent      string `long:"agent" help:"Stable remote agent ID"`
	Group      string `long:"group" help:"Spawn group, or destination group on this node for move/teleport"`
	Job        int64  `long:"job" help:"Remote spawn request ID for spawn-status"`
	Brief      string `long:"brief" help:"Task briefing for spawn"`
	Profile    string `long:"profile" help:"Receiver-allowlisted spawn profile"`
	Name       string `long:"name" help:"Spawn worker name"`
	Role       string `long:"role" help:"Spawn worker role"`
	Body       string `long:"body" help:"Message text"`
	Subject    string `long:"subject" help:"Message subject"`
	FollowUp   string `long:"follow-up" help:"Clone initial prompt"`
	NoCopyConv bool   `long:"no-copy-conv" help:"Clone without conversation history"`
	Force      bool   `long:"force" help:"Force-stop the remote pane"`
	Clone      bool   `long:"clone" help:"Teleport a copy instead of retiring the source"`
	Note       string `long:"note" help:"Teleport handoff note"`
}

func actionCmd() *cobra.Command {
	return boa.CmdT[actionParams]{Use: "action", Short: "Run a grant-authorized action on a remote node (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *actionParams, _ *cobra.Command, _ []string) { os.Exit(runAction(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runAction(p *actionParams, stdout, stderr io.Writer) int {
	if p.Node == "" {
		return fail(stderr, fmt.Errorf("--node is required"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	method := "POST"
	var tail string
	var body any
	switch p.Action {
	case "spawn":
		if p.Group == "" || p.Brief == "" {
			return fail(stderr, fmt.Errorf("spawn requires --group and --brief"))
		}
		tail = "groups/" + url.PathEscape(p.Group) + "/spawn"
		body = map[string]any{"brief": p.Brief, "name": p.Name, "role": p.Role, "profile": p.Profile}
	case "spawn-status":
		if p.Job <= 0 {
			return fail(stderr, fmt.Errorf("spawn-status requires positive --job"))
		}
		method = "GET"
		tail = fmt.Sprintf("spawn-requests/%d", p.Job)
	case "message":
		if p.Agent == "" || p.Body == "" {
			return fail(stderr, fmt.Errorf("message requires --agent and --body"))
		}
		tail = "operator-message"
		body = map[string]any{"to": p.Agent, "subject": p.Subject, "body": p.Body}
	case "stop", "retire", "clone", "move", "teleport":
		if p.Agent == "" {
			return fail(stderr, fmt.Errorf("--agent stable ID is required"))
		}
		tail = "agents/" + url.PathEscape(p.Agent) + "/" + p.Action
		body = map[string]any{}
		switch p.Action {
		case "stop":
			if p.Force {
				tail += "?force=1"
			}
		case "clone":
			body = map[string]any{"follow_up": p.FollowUp, "no_copy_conv": p.NoCopyConv}
		case "move", "teleport":
			if p.Group == "" {
				return fail(stderr, fmt.Errorf("destination --group on this node is required"))
			}
			body = map[string]any{"group": p.Group}
			if p.Action == "teleport" {
				body = map[string]any{"group": p.Group, "note": p.Note, "clone": p.Clone}
			}
		}
	default:
		return fail(stderr, fmt.Errorf("unknown remote action"))
	}
	var result any
	if err := agent.DaemonRequest(method, "/v1/federation/peer/"+url.PathEscape(p.Node)+"/"+tail, body, &result, agent.DaemonOpts{NoRetry: true}); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, result)
}
