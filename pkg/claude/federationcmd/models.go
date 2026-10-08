package federationcmd

import (
	"net/http"
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type modelLeaseParams struct {
	Revoke string `long:"revoke" optional:"true" help:"Revoke one requester-paid worker lease by ID"`
}
type modelUsageParams struct {
	Day string `long:"day" optional:"true" help:"UTC date YYYY-MM-DD (default today)"`
}
type modelSwitchParams struct {
	Name string `long:"name" optional:"true" help:"Named gateway; omit to switch all gateways"`
	Peer string `long:"peer" optional:"true" help:"Trusted peer to block or unblock on the named gateway"`
}

func modelsCmd() *cobra.Command {
	switchCmd := func(use string, disabled bool) *cobra.Command {
		return boa.CmdT[modelSwitchParams]{Use: use, Short: "Switch model gateway access", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *modelSwitchParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodPost, "/v1/models/control", map[string]any{"name": p.Name, "peer": p.Peer, "disabled": disabled})
		}}.ToCobra()
	}
	return boa.CmdT[struct{}]{Use: "models", Short: "Inspect and control federated model gateways", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[struct{}]{Use: "status", Short: "Show policy and switches without credentials", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodGet, "/v1/models/control", nil)
		}}.ToCobra(),
		boa.CmdT[modelUsageParams]{Use: "usage", Short: "Show request metadata and daily token charges", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *modelUsageParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodGet, "/v1/models/usage?day="+url.QueryEscape(p.Day), nil)
		}}.ToCobra(),
		boa.CmdT[modelLeaseParams]{Use: "leases", Short: "Show requester-paid workers or revoke one lease", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *modelLeaseParams, _ *cobra.Command, _ []string) {
			if p.Revoke != "" {
				repoRequest(http.MethodPost, "/v1/models/leases", map[string]string{"id": p.Revoke})
			} else {
				repoRequest(http.MethodGet, "/v1/models/leases", nil)
			}
		}}.ToCobra(),
		switchCmd("disable", true), switchCmd("enable", false),
	}}.ToCobra()
}
