package usage

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
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
	if params.JSON {
		raw, err := usageapi.FetchUsageRaw()
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}

	resp, err := usageapi.FetchUsage()
	if err != nil {
		return err
	}

	if resp.FiveHour != nil {
		fmt.Printf("5-hour utilization:       %.1f%%%s\n", resp.FiveHour.Utilization, fmtResets(resp.FiveHour.ResetsAt))
	}
	if resp.SevenDay != nil {
		fmt.Printf("7-day utilization:        %.1f%%%s\n", resp.SevenDay.Utilization, fmtResets(resp.SevenDay.ResetsAt))
	}
	if resp.SevenDaySonnet != nil {
		fmt.Printf("7-day sonnet utilization: %.1f%%%s\n", resp.SevenDaySonnet.Utilization, fmtResets(resp.SevenDaySonnet.ResetsAt))
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

func fmtResets(resetsAt string) string {
	if resetsAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, resetsAt)
	if err != nil {
		return ""
	}
	local := t.Local()
	now := time.Now()
	var date string
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		date = "today"
	} else {
		date = local.Format("2006-01-02")
	}
	return ", resets " + date + " " + local.Format("15:04")
}
