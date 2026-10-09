package hubcmd

import (
	"encoding/json"
	"fmt"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"os"
	"time"
)

type identityRecoveryParams struct {
	dbParam
	Old         string `pos:"true" help:"Previous admitted instance ID"`
	New         string `pos:"true" help:"Replacement key-derived instance ID"`
	Fingerprint string `long:"fingerprint" optional:"true" help:"Verified replacement fingerprint; required with --apply"`
	Apply       bool   `long:"apply" help:"Replace admission and spaces (default: preview)"`
}
type revokeOldParams struct {
	dbParam
	Instance string `pos:"true" help:"Retired or compromised key-derived instance ID"`
	Apply    bool   `long:"apply" help:"Revoke predecessor and pending successors (default: preview)"`
}

func identityCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "identity", Short: "Manage admission continuity after rotation or key loss", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[identityRecoveryParams]{Use: "recover", Short: "Explicitly transfer old admission and spaces to a replacement", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *identityRecoveryParams, _ *cobra.Command, _ []string) {
			if !proto.ValidInstanceID(p.Old) || !proto.ValidInstanceID(p.New) {
				fail(fmt.Errorf("use immutable key-derived instance IDs"))
			}
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			previous, err := st.Get(p.Old)
			if err != nil {
				fail(err)
			}
			if previous == nil {
				fail(fmt.Errorf("old identity is not admitted"))
			}
			next, err := st.Get(p.New)
			if err != nil {
				fail(err)
			}
			preview := map[string]any{"old": previous, "replacement": next, "new": p.New, "new_fingerprint": proto.InstanceFingerprint(p.New), "warning": "Replacement inherits old spaces; any current replacement spaces are replaced. Old admission is revoked. Peers still require explicit local recovery."}
			if p.Apply {
				if p.Fingerprint != proto.InstanceFingerprint(p.New) {
					fail(fmt.Errorf("verify replacement out of band and pass --fingerprint %s", proto.InstanceFingerprint(p.New)))
				}
				if err = st.RecoverIdentity(p.Old, p.New, time.Now()); err != nil {
					fail(err)
				}
				preview["applied"] = true
			}
			if err = json.NewEncoder(os.Stdout).Encode(preview); err != nil {
				fail(err)
			}
		}}.ToCobra(),
		boa.CmdT[revokeOldParams]{Use: "revoke-old", Short: "Block predecessor replay and pending automatic rotation", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *revokeOldParams, _ *cobra.Command, _ []string) {
			if !proto.ValidInstanceID(p.Instance) {
				fail(fmt.Errorf("use an immutable instance ID"))
			}
			if !p.Apply {
				fmt.Println("Revoke predecessor", p.Instance, "and its pending rotations; accepted successor stays current. Apply with --apply.")
				return
			}
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			if err = st.RevokeOldIdentity(p.Instance, time.Now()); err != nil {
				fail(err)
			}
			fmt.Println("revoked", p.Instance)
		}}.ToCobra(),
	}}.ToCobra()
}
