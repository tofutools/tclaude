package federationcmd

import (
	"fmt"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"io"
	"net/url"
	"os"
	"text/tabwriter"
)

type moveAgentParams struct {
	Agent        string `pos:"true" help:"Source agent selector; agents can use self."`
	Peer         string `pos:"true" help:"Trusted destination peer."`
	Group        string `long:"group" help:"Receiving group granting agents.receive."`
	AllowFlagged bool   `long:"allow-flagged" help:"Explicitly allow suspected credentials in the history."`
	JSON         bool   `long:"json" help:"Output JSON."`
}

func moveAgentCmd() *cobra.Command {
	return boa.CmdT[moveAgentParams]{Use: "move-agent", Short: "Clone with history, retiring the source after running confirmation", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *moveAgentParams, _ *cobra.Command, _ []string) {
		os.Exit(runAgentOffer(&shareAgentParams{Agent: p.Agent, Peer: p.Peer, Group: p.Group, History: true, AllowFlagged: p.AllowFlagged, JSON: p.JSON}, "/v1/federation/move-agent", os.Stdout, os.Stderr))
	}}.ToCobra()
}

type moveIDParams struct {
	ID string `pos:"true" help:"Move offer ID."`
}

func movesGet(path string) {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		os.Exit(rc)
	}
	var out any
	if err := agent.DaemonGet(path, &out); err != nil {
		os.Exit(fail(os.Stderr, err))
	}
	os.Exit(printJSON(os.Stdout, out))
}

type movesListParams struct {
	JSON bool `long:"json" help:"Output JSON."`
}

func runMovesList(p *movesListParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out struct {
		Moves []db.FederationAgentMove `json:"moves"`
	}
	if err := agent.DaemonGet("/v1/federation/moves", &out); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, out)
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDIRECTION\tPEER\tSTATE\tSOURCE\tTARGET\tGROUP\tTRANSFER")
	for _, m := range out.Moves {
		progress := "-"
		if m.Transfer != nil {
			progress = fmt.Sprintf("%.1f/%.1f MiB", float64(m.Transfer.BytesDone)/(1<<20), float64(m.Transfer.BytesTotal)/(1<<20))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Direction, m.Peer, m.State, dash(m.SourceAgent), dash(m.TargetAgent), dash(m.Group), progress)
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}
func movesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "moves", Short: "Inspect or abandon durable agent moves (operator only)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[movesListParams]{Use: "ls", Short: "List incoming and outgoing moves, including moved-from provenance", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *movesListParams, _ *cobra.Command, _ []string) { os.Exit(runMovesList(p, os.Stdout, os.Stderr)) }}.ToCobra(),
		boa.CmdT[moveIDParams]{Use: "show", Short: "Show one move and its source/destination identities", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *moveIDParams, _ *cobra.Command, _ []string) {
			movesGet("/v1/federation/moves/" + url.PathEscape(p.ID))
		}}.ToCobra(),
		boa.CmdT[moveIDParams]{Use: "abandon", Short: "Keep the source and ignore late confirmations; destination clones remain", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *moveIDParams, _ *cobra.Command, _ []string) {
			var out any
			if rc := offerPost(os.Stderr, "/v1/federation/moves/"+url.PathEscape(p.ID)+"/abandon", nil, &out); rc != 0 {
				os.Exit(rc)
			}
			os.Exit(printJSON(os.Stdout, out))
		}}.ToCobra(),
	}}.ToCobra()
}
