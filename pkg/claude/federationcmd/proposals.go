package federationcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type proposalsParams struct {
	Action         string   `pos:"true" help:"targets | create | list | show | accept | decline | counter | withdraw"`
	Peer           string   `long:"peer" optional:"true" help:"Trusted node label or instance ID (create)"`
	ID             string   `long:"id" optional:"true" help:"Proposal ID (show/decisions)"`
	Revision       int      `long:"revision" optional:"true" help:"Exact reviewed revision (decisions)"`
	File           string   `long:"file" optional:"true" help:"JSON array of proposal items (create/counter); - reads stdin"`
	Items          []string `long:"item" short:"-" optional:"true" help:"Selected item ID (repeat to accept several)"`
	WithoutMembers []string `long:"without-members" short:"-" optional:"true" help:"Selected visibility item ID whose roster is excluded"`
	Note           string   `long:"note" optional:"true" help:"Proposal note (create)"`
}

func proposalsCmd() *cobra.Command {
	return boa.CmdT[proposalsParams]{Use: "proposals", Short: "Propose and decide peer group sharing and communication links (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *proposalsParams, _ *cobra.Command, _ []string) {
		os.Exit(runProposals(p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runProposals(p *proposalsParams, stdin io.Reader, stdout, stderr io.Writer) int {
	method, path := "GET", "/v1/federation/proposals"
	var body any
	readItems := func() ([]proto.ProposalItem, error) {
		if p.File == "" {
			return nil, fmt.Errorf("--file is required")
		}
		input := stdin
		if p.File != "-" {
			f, err := os.Open(p.File)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			input = f
		}
		var items []proto.ProposalItem
		dec := json.NewDecoder(io.LimitReader(input, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&items); err != nil {
			return nil, err
		}
		return items, nil
	}
	switch p.Action {
	case "targets":
		path = "/v1/federation/link-targets"
	case "list":
	case "show":
		if p.ID == "" {
			return fail(stderr, fmt.Errorf("show requires --id"))
		}
		path += "/" + url.PathEscape(p.ID)
	case "create":
		if p.Peer == "" {
			return fail(stderr, fmt.Errorf("create requires --peer"))
		}
		items, err := readItems()
		if err != nil {
			return fail(stderr, err)
		}
		method = "POST"
		body = map[string]any{"peer": p.Peer, "note": p.Note, "items": items}
	case "accept", "decline", "counter", "withdraw":
		if p.ID == "" || p.Revision < 1 {
			return fail(stderr, fmt.Errorf("decisions require --id and --revision"))
		}
		method = "POST"
		path += "/" + url.PathEscape(p.ID) + "/decision"
		in := map[string]any{"revision": p.Revision, "decision": p.Action}
		if p.Action == "accept" {
			if len(p.Items) == 0 {
				return fail(stderr, fmt.Errorf("accept requires at least one --item"))
			}
			in["item_ids"] = p.Items
			in["without_members"] = p.WithoutMembers
		}
		if p.Action == "counter" {
			items, err := readItems()
			if err != nil {
				return fail(stderr, err)
			}
			in["items"] = items
		}
		body = in
	default:
		return fail(stderr, fmt.Errorf("unknown proposal action %q", strings.TrimSpace(p.Action)))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out json.RawMessage
	if err := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{NoRetry: true}); err != nil {
		return failPeerView(stderr, err)
	}
	return printJSON(stdout, out)
}
