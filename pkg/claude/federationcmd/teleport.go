package federationcmd

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

func teleportControlCmd() *cobra.Command {
	children := []*cobra.Command{}
	for _, mode := range []string{"status", "on", "off"} {
		children = append(children, boa.CmdT[struct{}]{Use: mode, Short: "Inspect or set this instance's teleport freeze", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runTeleportControl(mode, os.Stdout, os.Stderr))
		}}.ToCobra())
	}
	return boa.CmdT[struct{}]{Use: "teleport", Short: "Enable or freeze teleports on this instance (operator only)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: children}.ToCobra()
}

func runTeleportControl(mode string, stdout, stderr io.Writer) int {
	method := http.MethodPut
	var body any
	switch mode {
	case "status":
		method = http.MethodGet
	case "on", "off":
		body = map[string]bool{"disabled": mode == "off"}
	default:
		return fail(stderr, fmt.Errorf("unknown teleport mode %q", mode))
	}
	var out any
	if err := agent.DaemonRequest(method, "/v1/federation/teleport", body, &out, agent.DaemonOpts{}); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, out)
}
