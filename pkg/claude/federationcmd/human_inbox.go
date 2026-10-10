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

// The human inbox of another of your own nodes: its agents' notifications and
// pending ask-human requests. The peer serves these only to a node it trusts
// unrestricted (human.inbox.read / human.inbox.answer), and only through this
// operator-only proxy; decisions are one-shot.

type humanInboxParams struct {
	Peer string `pos:"true" help:"Your own node (trusted unrestricted): label or stable ID"`
}

type humanInboxReplyParams struct {
	Peer string `pos:"true" help:"Your own node: label or stable ID"`
	ID   int64  `pos:"true" help:"Human message ID on that node"`
	Body string `long:"body" help:"Reply text"`
}

type humanInboxDecideParams struct {
	Peer     string `pos:"true" help:"Your own node: label or stable ID"`
	ID       string `pos:"true" help:"Pending access request ID on that node"`
	Decision string `pos:"true" help:"approve|deny (one-shot; always-allow stays local)"`
}

func humanInboxCmd() *cobra.Command {
	cmd := boa.CmdT[humanInboxParams]{Use: "human-inbox", Short: "List another own node's human notifications and pending access requests (operator only)", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *humanInboxParams, _ *cobra.Command, _ []string) { os.Exit(runHumanInbox(p, os.Stdout, os.Stderr)) }}.ToCobra()
	cmd.AddCommand(boa.CmdT[humanInboxReplyParams]{Use: "reply", Short: "Reply to an agent's notification on another own node", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *humanInboxReplyParams, _ *cobra.Command, _ []string) { os.Exit(runHumanInboxReply(p, os.Stdout, os.Stderr)) }}.ToCobra())
	cmd.AddCommand(boa.CmdT[humanInboxDecideParams]{Use: "decide", Short: "Approve or deny one pending access request on another own node, once", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *humanInboxDecideParams, _ *cobra.Command, _ []string) { os.Exit(runHumanInboxDecide(p, os.Stdout, os.Stderr)) }}.ToCobra())
	return cmd
}

func humanInboxRequest(peer, method, tail string, body any, stdout, stderr io.Writer) int {
	if peer == "" {
		return fail(stderr, fmt.Errorf("a peer is required"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var result any
	if err := agent.DaemonRequest(method, "/v1/federation/peer/"+url.PathEscape(peer)+"/"+tail, body, &result, agent.DaemonOpts{NoRetry: true}); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, result)
}

func runHumanInbox(p *humanInboxParams, stdout, stderr io.Writer) int {
	return humanInboxRequest(p.Peer, "GET", "human-inbox", nil, stdout, stderr)
}

func runHumanInboxReply(p *humanInboxReplyParams, stdout, stderr io.Writer) int {
	if p.ID <= 0 || p.Body == "" {
		return fail(stderr, fmt.Errorf("reply requires a message ID and --body"))
	}
	return humanInboxRequest(p.Peer, "POST", "human-inbox/reply", map[string]any{"id": p.ID, "body": p.Body}, stdout, stderr)
}

func runHumanInboxDecide(p *humanInboxDecideParams, stdout, stderr io.Writer) int {
	if p.ID == "" || (p.Decision != "approve" && p.Decision != "deny") {
		return fail(stderr, fmt.Errorf("decide requires a request ID and approve|deny"))
	}
	return humanInboxRequest(p.Peer, "POST", "human-inbox/access/"+url.PathEscape(p.ID), map[string]any{"decision": p.Decision}, stdout, stderr)
}
