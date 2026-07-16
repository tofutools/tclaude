package processcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
	"github.com/tofutools/tclaude/pkg/common"
)

type worklistParams struct {
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
	Assignee  string `long:"assignee" optional:"true" help:"Only items assigned to this exact actor or role"`
	Kind      string `long:"kind" optional:"true" help:"Only items of this work kind"`
	Run       string `long:"run" optional:"true" help:"Only items for this process run"`
	Status    string `long:"status" optional:"true" help:"Only items with this status"`
}

func worklistCmd() *cobra.Command {
	return boa.CmdT[worklistParams]{
		Use:         "worklist",
		Short:       "List explicit process work obligations",
		ParamEnrich: common.DefaultParamEnricher(),
		PreExecuteFunc: func(p *worklistParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *worklistParams, cmd *cobra.Command, _ []string) {
			exitWithError(runWorklist(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runWorklist(cmd *cobra.Command, p *worklistParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	runs, err := fs.ListRuns(cmd.Context())
	if err != nil {
		return err
	}
	snapshots := make([]store.Snapshot, 0, len(runs))
	v7Items := make([]worklist.Item, 0)
	for _, run := range runs {
		if run.ID == "" {
			continue
		}
		schema, schemaErr := fs.RunStateSchemaVersion(cmd.Context(), run.ID)
		if schemaErr != nil {
			fmt.Fprintf(out, "Warning: skipped unreadable process run %s: %v\n", run.ID, schemaErr)
			continue
		}
		if schema == pathv1.CheckpointStateSchemaVersion {
			snapshot, loadErr := fs.LoadPathV1RunView(cmd.Context(), run.ID)
			if loadErr != nil {
				fmt.Fprintf(out, "Warning: skipped unreadable process run %s: %v\n", run.ID, loadErr)
				continue
			}
			items, deriveErr := worklist.DerivePathV1(cmd.Context(), snapshot, func(_ context.Context, commandID string) (string, error) {
				agent, lookupErr := db.AgentForProcessCommand(commandID)
				if lookupErr != nil || agent == nil {
					return "", lookupErr
				}
				return "agent:" + agent.AgentID, nil
			})
			if deriveErr != nil {
				fmt.Fprintf(out, "Warning: skipped unreadable process run %s: %v\n", run.ID, deriveErr)
				continue
			}
			v7Items = append(v7Items, items...)
			continue
		}
		if schema <= 0 || schema > pathv1.LegacyMaxSchemaVersion {
			fmt.Fprintf(out, "Warning: skipped unreadable process run %s: unsupported state schema %d\n", run.ID, schema)
			continue
		}
		snapshot, loadErr := fs.LoadRun(cmd.Context(), run.ID)
		if loadErr != nil {
			fmt.Fprintf(out, "Warning: skipped unreadable process run %s: %v\n", run.ID, loadErr)
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	items := append(worklist.Derive(snapshots), v7Items...)
	slices.SortFunc(items, func(a, b worklist.Item) int { return strings.Compare(a.ID, b.ID) })
	items = worklist.ApplyFilter(items, worklist.Filter{
		Assignee: strings.TrimSpace(p.Assignee), Kind: worklist.Kind(strings.TrimSpace(p.Kind)),
		Run: strings.TrimSpace(p.Run), Status: state.WaitStatus(strings.TrimSpace(p.Status)),
	})
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tRUN\tNODE\tKIND\tASSIGNEE\tSTATUS\tDUE\tNUDGE\tSUMMARY\tACTIONS")
	for _, item := range items {
		nudge := "-"
		if item.Nudge != nil {
			nudge = fmt.Sprintf("%d/%d next %s", item.Nudge.BudgetUsed, item.Nudge.BudgetMax, formatTime(item.Nudge.NextContactAt))
			if item.Nudge.Paused {
				nudge += " paused"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			item.ID, item.Run, item.Node, item.Kind, item.Assignee, item.Status,
			formatTime(item.DueAt), nudge, item.Summary, strings.Join(item.AvailableActions, ","))
	}
	return tw.Flush()
}
