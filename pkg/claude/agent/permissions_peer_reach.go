package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// peer-reach edits the peer=<peer>/<group> part of an agent's or group's
// grants without replacing the rest: `grant --scope peer=…` writes the whole
// row, so a second peer group would replace the first.

type peerReachRow struct {
	Kind    string   `json:"kind"`
	Group   string   `json:"group,omitempty"`
	Conv    string   `json:"conv,omitempty"`
	AgentID string   `json:"agent_id,omitempty"`
	Title   string   `json:"title,omitempty"`
	Slug    string   `json:"slug"`
	Peers   []string `json:"peers"`
}

type peerReachResp struct {
	Rows []peerReachRow `json:"rows"`
}

func permissionsPeerReachCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "peer-reach",
		Short: "Let agents use what peers share: add or remove peer=<peer>/<group> on their grants",
		Long: "A peer shares a remote group with this node (tclaude federation remote); a local agent uses it through a grant " +
			"scoped peer=<peer>/<group>. `add` merges that peer group into an agent's or local group's grant of each slug, keeping " +
			"the peer groups it already holds, and `remove` takes one out. A grant that holds the slug for local use is refused " +
			"rather than replaced: give the reach to a group the agent is in instead. Human-only.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{peerReachLsCmd(), peerReachEditCmd("add", false), peerReachEditCmd("remove", true)},
	}.ToCobra()
}

type peerReachLsParams struct {
	Peer string `long:"peer" optional:"true" help:"Only grants naming this peer (label or instance ID)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func peerReachLsCmd() *cobra.Command {
	return boa.CmdT[peerReachLsParams]{
		Use:         "ls",
		Short:       "List agents' and groups' peer-scoped grants",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *peerReachLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runPeerReachLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runPeerReachLs(p *peerReachLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/permissions/peer-reach"
	if p.Peer != "" {
		path += "?peer=" + url.QueryEscape(p.Peer)
	}
	var resp peerReachResp
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return printPeerReach(resp, p.JSON, stdout)
}

func printPeerReach(resp peerReachResp, asJSON bool, stdout io.Writer) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(resp.Rows) == 0 {
		fmt.Fprintln(stdout, "(no peer-scoped grants)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "HOLDER", MinWidth: 12, Weight: 0.6, Truncate: true},
		table.Column{Header: "SLUG", MinWidth: 12, Weight: 0.5, Truncate: true},
		table.Column{Header: "PEER GROUPS", MinWidth: 16, Weight: 1, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, r := range resp.Rows {
		holder := "group " + r.Group
		if r.Kind == "agent" {
			holder = shortAgentID(r.AgentID, r.Conv)
			if r.Title != "" {
				holder = r.Title + " (" + holder + ")"
			}
		}
		tbl.AddRow(table.Row{Cells: []string{holder, r.Slug, strings.Join(r.Peers, ", ")}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

type peerReachEditParams struct {
	Target string   `pos:"true" optional:"true" help:"The agent: a conv selector (UUID, prefix, or current title). Omit with --group."`
	Group  string   `long:"group" optional:"true" help:"A local group instead of an agent; every member gets the reach"`
	Peer   string   `long:"peer" help:"The peer and its group, as <peer>/<group> (a peer alone covers all its groups)"`
	Slug   []string `long:"slug" help:"Permission slug; repeat for several (see 'tclaude agent permissions slugs')"`
	JSON   bool     `long:"json" help:"Output JSON"`
}

func peerReachEditCmd(verb string, remove bool) *cobra.Command {
	short := "Add a peer group to an agent's or group's grants of the given slugs"
	if remove {
		short = "Remove a peer group from an agent's or group's grants of the given slugs"
	}
	return boa.CmdT[peerReachEditParams]{
		Use:         verb,
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *peerReachEditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completePermissionTargets)
			boa.GetParamT(ctx, &p.Slug).SetAlternativesFunc(completePermissionSlugs)
			return nil
		},
		RunFunc: func(p *peerReachEditParams, _ *cobra.Command, _ []string) {
			os.Exit(runPeerReachEdit(p, remove, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runPeerReachEdit(p *peerReachEditParams, remove bool, stdout, stderr io.Writer) int {
	if (p.Target == "") == (p.Group == "") {
		fmt.Fprintln(stderr, "Error: name the agent or pass --group, not both")
		return rcInvalidArg
	}
	if strings.TrimSpace(p.Peer) == "" || len(p.Slug) == 0 {
		fmt.Fprintln(stderr, "Error: --peer and at least one --slug are required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{"slugs": p.Slug, "peer": p.Peer, "remove": remove}
	if p.Group != "" {
		body["group"] = p.Group
	} else {
		body["target"] = p.Target
	}
	var resp peerReachResp
	if err := DaemonRequest(http.MethodPost, "/v1/permissions/peer-reach", body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return printPeerReach(resp, p.JSON, stdout)
}
