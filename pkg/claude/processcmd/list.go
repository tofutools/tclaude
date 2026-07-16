package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	"github.com/tofutools/tclaude/pkg/common"
)

type templatesParams struct{}

func templatesCmd() *cobra.Command {
	return boa.CmdT[templatesParams]{
		Use:         "templates",
		Short:       "Inspect stored process templates",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesLsCmd(),
		},
		RunFunc: func(_ *templatesParams, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type templatesLsParams struct {
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
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
	fs, err := openStore(p.StoreRoot, true)
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

type runsParams struct{}

func runsCmd() *cobra.Command {
	return boa.CmdT[runsParams]{
		Use:         "runs",
		Short:       "Inspect process runs",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			runsLsCmd(),
		},
		RunFunc: func(_ *runsParams, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type runsLsParams struct {
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
}

func runsLsCmd() *cobra.Command {
	return boa.CmdT[runsLsParams]{
		Use:         "ls",
		Short:       "List process runs",
		ParamEnrich: common.DefaultParamEnricher(),
		PreExecuteFunc: func(p *runsLsParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *runsLsParams, cmd *cobra.Command, _ []string) {
			exitWithError(runRunsLs(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runRunsLs(cmd *cobra.Command, p *runsLsParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	runs, err := fs.ListRuns(cmd.Context())
	if err != nil {
		return err
	}
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tSTATUS\tTEMPLATE\tCREATED")
	for _, run := range runs {
		status := "load_error"
		if schema, schemaErr := fs.RunStateSchemaVersion(cmd.Context(), run.ID); schemaErr == nil {
			if schema == pathv1.CheckpointStateSchemaVersion {
				if snapshot, loadErr := fs.LoadPathV1RunView(cmd.Context(), run.ID); loadErr == nil {
					if _, verifyErr := pathv1.VerifyExecutionInput(cmd.Context(), snapshot.CheckpointJSON, snapshot.TemplateSource); verifyErr == nil {
						status = pathv1.CurrentRunStatus(snapshot.Checkpoint)
					}
				}
			} else if schema > 0 && schema <= pathv1.LegacyMaxSchemaVersion {
				if snapshot, loadErr := fs.LoadRun(cmd.Context(), run.ID); loadErr == nil && snapshot.State != nil {
					report := processverify.Snapshot(snapshot)
					status = string(report.EffectiveStatus)
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", run.ID, status, run.TemplateRef, formatTime(run.CreatedAt))
	}
	return tw.Flush()
}
