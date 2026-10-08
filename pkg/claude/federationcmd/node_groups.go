package federationcmd

import (
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type nodeGroupNameParams struct {
	Name string `pos:"true" help:"Local node pool name."`
}
type nodeGroupMemberParams struct {
	Name string `pos:"true" help:"Local node pool name."`
	Peer string `pos:"true" help:"Trusted peer label or instance ID."`
}
type nodeGroupRemoveParams struct {
	Name string `pos:"true" help:"Local node pool name."`
	Peer string `pos:"true" optional:"true" help:"Peer to remove; omit to delete the pool and its grants."`
}

func nodeGroupMutation(method, path string, in any) {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		os.Exit(rc)
	}
	var out any
	if err := agent.DaemonRequest(method, path, in, &out, agent.DaemonOpts{}); err != nil {
		os.Exit(fail(os.Stderr, err))
	}
	os.Exit(printJSON(os.Stdout, out))
}

// NodeGroupsCmd is attached beneath the nodes discovery command.
func NodeGroupsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "groups", Aliases: []string{"group"}, Short: "Manage local pools of trusted peers and live membership policy", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[nodeGroupNameParams]{Use: "create", Short: "Create an empty local pool", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeGroupNameParams, _ *cobra.Command, _ []string) {
			nodeGroupMutation(http.MethodPost, "/v1/federation/nodes/groups", map[string]any{"name": p.Name})
		}}.ToCobra(),
		boa.CmdT[nodeGroupMemberParams]{Use: "add", Short: "Add a trusted peer; pool grants apply immediately", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeGroupMemberParams, _ *cobra.Command, _ []string) {
			nodeGroupMutation(http.MethodPost, "/v1/federation/nodes/groups/"+url.PathEscape(p.Name)+"/members", map[string]any{"peer": p.Peer})
		}}.ToCobra(),
		boa.CmdT[nodeGroupRemoveParams]{Use: "rm", Short: "Remove a peer, or delete the pool and its grants", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeGroupRemoveParams, _ *cobra.Command, _ []string) {
			path := "/v1/federation/nodes/groups/" + url.PathEscape(p.Name)
			var in any
			if p.Peer != "" {
				path += "/members"
				in = map[string]any{"peer": p.Peer}
			}
			nodeGroupMutation(http.MethodDelete, path, in)
		}}.ToCobra(),
		boa.CmdT[jsonParam]{Use: "ls", Short: "List local pools with immutable IDs and current trusted members", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jsonParam, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var out struct {
				Groups []struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Members []struct {
						InstanceID string `json:"instance_id"`
						Label      string `json:"label"`
					} `json:"members"`
				} `json:"groups"`
			}
			if err := agent.DaemonGet("/v1/federation/nodes/groups", &out); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, out))
			}
			for _, g := range out.Groups {
				fmt.Printf("%s (%s): %d peers\n", g.Name, g.ID, len(g.Members))
				for _, peer := range g.Members {
					fmt.Printf("  %s %s\n", peer.Label, peer.InstanceID)
				}
			}
		}}.ToCobra(),
	}}.ToCobra()
}
