package processcmd

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct{}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "process",
		Short:       "Inspect repeatable process runs",
		Long:        "Inspect repeatable process templates and instantiated runs.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesCmd(),
			runsCmd(),
			runCmd(),
			showCmd(),
			worklistCmd(),
			advanceCmd(),
			unblockCmd(),
			observeCmd(),
			resolveCmd(),
			reportCmd(),
			verifyCmd(),
			repairCmd(),
		},
		RunFunc: func(_ *Params, cmd *cobra.Command, _ []string) {
			if err := requireProcessesEnabled(); err != nil {
				exitWithError(err)
			}
			_ = cmd.Help()
		},
	}.ToCobra()
	cmd.Hidden = !processesEnabled()
	return cmd
}

func requireProcessesEnabled() error {
	if processesEnabled() {
		return nil
	}
	return fmt.Errorf("process commands are disabled; set features.processes=true in tclaude config to use this experimental surface")
}

func processesEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.ProcessesEnabled()
}

func exitWithError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
