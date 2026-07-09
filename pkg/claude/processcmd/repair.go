package processcmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type repairParams struct {
	RunID     string `pos:"true" help:"Process run id to repair"`
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
}

func repairCmd() *cobra.Command {
	return boa.CmdT[repairParams]{
		Use:         "repair",
		Short:       "Explain future process repair flow",
		Long:        "Explain the future process repair flow. Repair is intentionally not implemented yet.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(1),
		PreExecuteFunc: func(p *repairParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *repairParams, _ *cobra.Command, _ []string) {
			fmt.Fprintf(os.Stderr, "process repair for run %q is not implemented yet; run `tclaude process verify %q --store-root %q` for diagnostics and use a future repair flow.\n", p.RunID, p.RunID, p.StoreRoot)
			os.Exit(1)
		},
	}.ToCobra()
}
