package federationcmd

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type awayParams struct {
	Cover string `long:"cover" optional:"true" help:"Trusted peer to cover; omit to show current away state"`
	Until string `long:"until" optional:"true" help:"End coverage after a duration (2h) or at an RFC3339 time"`
}

func awayCmd() *cobra.Command {
	return boa.CmdT[awayParams]{Use: "away", Short: "Select a covering peer operator, or show away state", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *awayParams, _ *cobra.Command, _ []string) {
		var out struct {
			Away *struct {
				Cover string    `json:"cover"`
				Until time.Time `json:"until"`
			} `json:"away"`
			Warnings []string `json:"warnings"`
		}
		if p.Cover == "" {
			if p.Until != "" {
				os.Exit(fail(os.Stderr, fmt.Errorf("--until requires --cover")))
			}
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			if err := agent.DaemonGet("/v1/federation/away", &out); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
		} else {
			until := time.Time{}
			if p.Until != "" {
				var err error
				if duration, e := time.ParseDuration(p.Until); e == nil {
					until = time.Now().Add(duration)
				} else {
					until, err = time.Parse(time.RFC3339, p.Until)
				}
				if err != nil || !until.After(time.Now()) {
					os.Exit(fail(os.Stderr, fmt.Errorf("--until must be a positive duration or future RFC3339 time")))
				}
			}
			if rc := post(os.Stderr, "/v1/federation/away", map[string]any{"cover": p.Cover, "until": until}, &out); rc != 0 {
				os.Exit(rc)
			}
		}
		if out.Away == nil {
			fmt.Println("Present; no covering peer")
		} else {
			fmt.Printf("Away; cover %s", out.Away.Cover)
			if !out.Away.Until.IsZero() {
				fmt.Printf(" until %s", out.Away.Until.Local().Format(time.RFC3339))
			}
			fmt.Println()
		}
		for _, w := range out.Warnings {
			fmt.Fprintln(os.Stderr, "Warning:", w)
		}
	}}.ToCobra()
}
func returnCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "return", Short: "End away forwarding and delegated approval authority", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
		var out any
		if rc := post(os.Stderr, "/v1/federation/return", struct{}{}, &out); rc != 0 {
			os.Exit(rc)
		}
		fmt.Println("Present; coverage ended")
	}}.ToCobra()
}

type awayAnswerParams struct {
	Ticket   string `pos:"true" help:"Exact request.epoch@instance ticket from an away notice"`
	Decision string `long:"decision" help:"One-shot approve or deny"`
}

func answerCmd() *cobra.Command {
	return boa.CmdT[awayAnswerParams]{Use: "answer", Short: "Answer an away access request once (human operators only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *awayAnswerParams, _ *cobra.Command, _ []string) {
		var out struct {
			EnvelopeID string `json:"envelope_id"`
			State      string `json:"state"`
		}
		if rc := post(os.Stderr, "/v1/federation/answer", map[string]any{"ticket": p.Ticket, "decision": p.Decision}, &out); rc != 0 {
			os.Exit(rc)
		}
		fmt.Printf("%s (envelope %s); receipt and final decision are in federation outbox\n", out.State, out.EnvelopeID)
	}}.ToCobra()
}
