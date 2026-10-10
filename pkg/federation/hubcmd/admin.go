package hubcmd

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

func adminCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "admin", Short: "Host-only hub administrator recovery", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[dbParam]{Use: "reset", Short: "Break-glass: remove all admins and issue a new private claim token", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *dbParam, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			token, err := st.PrepareAdminClaim(true, time.Now())
			if err != nil {
				fail(err)
			}
			fmt.Fprintf(os.Stdout, "All hub admins cleared. Claim token (single-use,valid24h): %s\nPrivate claim file: %s\n", token, st.ClaimPath())
		}}.ToCobra(),
	}}.ToCobra()
}
