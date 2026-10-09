package federationcmd

import (
	"net/http"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

func teleportControlCmd() *cobra.Command {
	children := []*cobra.Command{}
	for _, mode := range []string{"on", "off"} {
		children = append(children, boa.CmdT[struct{}]{Use: mode, Short: "Set this instance's teleport freeze", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			var out any
			if err := agent.DaemonRequest(http.MethodPut, "/v1/federation/teleport", map[string]bool{"disabled": mode == "off"}, &out, agent.DaemonOpts{}); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			os.Exit(printJSON(os.Stdout, out))
		}}.ToCobra())
	}
	return boa.CmdT[struct{}]{Use: "teleport", Short: "Enable or freeze teleports on this instance (operator only)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: children}.ToCobra()
}
