package agent

import (
	"fmt"
	"io"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type tokenParams struct {
	Export bool `long:"export" help:"Print a ready-to-eval shell export line instead of the bare token"`
}

// tokenCmd implements `tclaude agent token` — it fetches the agentd
// operator token so the human operator can authenticate their CLI:
//
//	export TCLAUDE_HUMAN_TOKEN="$(tclaude agent token)"
//
// agentd gates GET /v1/auth/token on the caller having no Claude Code
// ancestor, so an agent invocation is refused — the token authenticates
// the human operator, not agents.
func tokenCmd() *cobra.Command {
	return boa.CmdT[tokenParams]{
		Use:   "token",
		Short: "Print the agentd operator token (human operator only)",
		Long: "Fetches the operator token from agentd so the human operator's `tclaude agent` " +
			"commands authenticate. Set it once per shell:\n\n" +
			"  export TCLAUDE_HUMAN_TOKEN=\"$(tclaude agent token)\"\n\n" +
			"An agent caller is refused — the token authenticates the human operator, not agents. " +
			"The token changes every time agentd restarts; re-run the export after a restart.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *tokenParams, _ *cobra.Command, _ []string) {
			os.Exit(runToken(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runToken(p *tokenParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := DaemonGet("/v1/auth/token", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Token == "" {
		fmt.Fprintln(stderr, "Error: daemon returned an empty operator token")
		return rcIOFailure
	}
	if p.Export {
		fmt.Fprintf(stdout, "export %s=%q\n", HumanTokenEnvVar, resp.Token)
	} else {
		fmt.Fprintln(stdout, resp.Token)
	}
	return rcOK
}
