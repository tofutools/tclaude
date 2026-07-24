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
			// Local argument validation stays ahead of any daemon probe.
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *templatesLsParams, cmd *cobra.Command, _ []string) {
			exitProcessRuntime(runTemplatesLs(cmd, p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runTemplatesLs(cmd *cobra.Command, p *templatesLsParams, out, stderr io.Writer) error {
	// templates ls reads a filesystem store rather than a daemon route, so it
	// has no operation response to carry the feature gate. Resolve the flag
	// through the daemon capability projection — never client-side config.
	if err := requireProcessesEnabledViaDaemon(stderr); err != nil {
		return err
	}
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
