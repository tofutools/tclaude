package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type templatesParams struct{}

func templatesCmd() *cobra.Command {
	return boa.CmdT[templatesParams]{
		Use:         "templates",
		Short:       "Inspect stored process templates",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{templatesLsCmd()},
		RunFunc: func(_ *templatesParams, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type templatesLsParams struct {
	StoreRoot string `long:"store-root" help:"Filesystem process-template store root"`
}

func templatesLsCmd() *cobra.Command {
	return boa.CmdT[templatesLsParams]{
		Use:         "ls",
		Short:       "List stored process templates",
		ParamEnrich: common.DefaultParamEnricher(),
		PreExecuteFunc: func(p *templatesLsParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *templatesLsParams, cmd *cobra.Command, _ []string) {
			exitWithError(runTemplatesLs(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runTemplatesLs(cmd *cobra.Command, p *templatesLsParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot)
	if err != nil {
		return err
	}
	records, err := fs.ListTemplates(cmd.Context())
	if err != nil {
		return err
	}
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tREF\tSTORED")
	for _, record := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", record.ID, record.Ref, formatTime(record.StoredAt))
	}
	return tw.Flush()
}

func runsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "runs",
		Short:       "Inspect daemon-owned process runs",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{processRunsLsCmd()},
		RunFunc: func(_ *struct{}, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}
