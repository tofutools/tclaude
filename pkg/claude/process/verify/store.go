package verify

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func StoreRun(ctx context.Context, runs store.Runs, runID string) Report {
	snapshot, err := runs.LoadRun(ctx, runID)
	if err != nil {
		return LoadError(runID, err)
	}
	return Snapshot(snapshot)
}
