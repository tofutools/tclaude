package hubcmd

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type grantExecParams struct {
	DBParam
	Instance string `pos:"true" help:"Existing hub administrator immutable instance ID"`
}

type resetParams struct {
	DBParam
	ClaimOutputParams
}

func adminCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "admin", Short: "Host-only hub administrator recovery", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[grantExecParams]{Use: "grant-exec", Short: "Host-only: seed elevated hub.exec on an existing administrator", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *grantExecParams, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			if err := st.GrantExec(p.Instance); err != nil {
				fail(err)
			}
			fmt.Fprintln(os.Stdout, "Granted hub.exec. Scripts still require the hub-local accept_remote_scripts switch; they run with the hub service user's full authority.")
		}}.ToCobra(),

		boa.CmdT[resetParams]{Use: "rotate-claim", Short: "Host-only: replace an unredeemed claim token without changing admins", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *resetParams, _ *cobra.Command, _ []string) {
			if err := validateClaimOutput(p.PrintClaimToken); err != nil {
				fail(err)
			}
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			token, err := st.RotateAdminClaim(time.Now())
			if err != nil {
				fail(err)
			}
			fmt.Fprintln(os.Stdout, "Outstanding claim token rotated; existing admins unchanged.")
			printClaimOutput(os.Stdout, token, st.ClaimPath(), p.PrintClaimToken)
		}}.ToCobra(),
		boa.CmdT[resetParams]{Use: "reset", Short: "Break-glass: remove all admins and issue a new private claim token", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *resetParams, _ *cobra.Command, _ []string) {
			if err := validateClaimOutput(p.PrintClaimToken); err != nil {
				fail(err)
			}
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			token, err := st.PrepareAdminClaim(true, time.Now())
			if err != nil {
				fail(err)
			}
			fmt.Fprintln(os.Stdout, "All hub admins cleared.")
			printClaimOutput(os.Stdout, token, st.ClaimPath(), p.PrintClaimToken)
		}}.ToCobra(),
	}}.ToCobra()
}
