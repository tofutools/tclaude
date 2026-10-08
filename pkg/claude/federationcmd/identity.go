package federationcmd

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"os"
)

type identityRotateParams struct {
	Apply bool `long:"apply" help:"Stage and publish the key rotation (default: preview)"`
}
type identityRecoverParams struct {
	Old         string `pos:"true" help:"Old trusted peer ID or label"`
	New         string `pos:"true" help:"Replacement immutable instance ID"`
	Fingerprint string `long:"fingerprint" optional:"true" help:"Replacement fingerprint verified out of band; required for --apply"`
	Apply       bool   `long:"apply" help:"Transfer the displayed authority to the replacement (default: preview)"`
}
type identityRevokeParams struct {
	Old   string `pos:"true" help:"Retired or compromised instance ID (or trusted peer label)"`
	Apply bool   `long:"apply" help:"Revoke the predecessor (default: preview)"`
}

func identityActionCommands() []*cobra.Command {
	return []*cobra.Command{
		boa.CmdT[identityRotateParams]{Use: "recover-local", Short: "Explicitly create an unlinked replacement after local key loss or compromise", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *identityRotateParams, _ *cobra.Command, _ []string) {
			os.Exit(runIdentityAction("recover-local", map[string]any{"apply": p.Apply}))
		}}.ToCobra(),
		boa.CmdT[identityRotateParams]{Use: "rotate", Short: "Rotate to a linked successor signing key", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *identityRotateParams, _ *cobra.Command, _ []string) {
			os.Exit(runIdentityAction("rotate", map[string]any{"apply": p.Apply}))
		}}.ToCobra(),
		boa.CmdT[identityRecoverParams]{Use: "recover-peer", Short: "Explicitly rebind trust after a peer loses its old key", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *identityRecoverParams, _ *cobra.Command, _ []string) {
			os.Exit(runIdentityAction("recover", map[string]any{"old": p.Old, "new": p.New, "fingerprint": p.Fingerprint, "apply": p.Apply}))
		}}.ToCobra(),
		boa.CmdT[identityRevokeParams]{Use: "revoke-old", Short: "Block an old key and its pending transitions", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *identityRevokeParams, _ *cobra.Command, _ []string) {
			os.Exit(runIdentityAction("revoke", map[string]any{"old": p.Old, "apply": p.Apply}))
		}}.ToCobra(),
		boa.CmdT[struct{}]{Use: "rotations", Short: "Show pending, accepted and revoked identity transitions", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var result any
			if err := agent.DaemonGet("/v1/federation/identity/rotations", &result); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			os.Exit(printJSON(os.Stdout, result))
		}}.ToCobra(),
	}
}
func runIdentityAction(action string, request any) int {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		return rc
	}
	var result any
	if err := agent.DaemonPost("/v1/federation/identity/"+action, request, &result); err != nil {
		return fail(os.Stderr, err)
	}
	return printJSON(os.Stdout, result)
}
