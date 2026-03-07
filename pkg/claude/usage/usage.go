package usage

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type Params struct {
	JSON bool `long:"json" help:"Output raw JSON from the API"`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:         "usage",
		Short:       "Show Claude Code subscription usage limits",
		Long:        "Shows current subscription usage limits by querying the Anthropic API.\nDisplays 5-hour and 7-day utilization percentages.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := runUsage(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runUsage(params *Params) error {
	token, err := usageapi.GetAccessToken()
	if err != nil {
		return err
	}

	if params.JSON {
		raw, err := usageapi.FetchRawWithRetry(token)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}

	resp, err := usageapi.FetchWithRetry(token)
	if err != nil {
		return err
	}

	if resp.FiveHour != nil {
		fmt.Printf("5-hour utilization:       %.1f%%\n", resp.FiveHour.Utilization)
	}
	if resp.SevenDay != nil {
		fmt.Printf("7-day utilization:        %.1f%%\n", resp.SevenDay.Utilization)
	}
	if resp.SevenDaySonnet != nil {
		fmt.Printf("7-day sonnet utilization: %.1f%%\n", resp.SevenDaySonnet.Utilization)
	}
	if resp.ExtraUsage != nil {
		eu := resp.ExtraUsage
		if eu.IsEnabled {
			fmt.Printf("extra usage:              enabled\n")
			if eu.UsedCredits != nil && eu.MonthlyLimit != nil {
				fmt.Printf("  used:                   %.2f / %.2f\n", *eu.UsedCredits/100, *eu.MonthlyLimit/100)
			}
			if eu.Utilization != nil {
				fmt.Printf("  utilization:            %.1f%%\n", *eu.Utilization)
			}
		} else {
			fmt.Printf("extra usage:              disabled\n")
		}
	}

	return nil
}
