//go:build linux || darwin

package store

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// UpgradeNeeded derives the only scheduler-facing migration-readiness result
// from an append-lock-held coherent execution view. The returned value is
// detached: its IDs and admin records are copied, bounded, and sorted before
// WithExecutionView releases either lock.
func (s *FS) UpgradeNeeded(ctx context.Context, runID string) (pathv1.UpgradeNeeded, error) {
	var result pathv1.UpgradeNeeded
	err := s.WithExecutionView(ctx, runID, func(view ExecutionView) error {
		var err error
		result, err = pathv1.AssessUpgradeNeeded(
			ctx,
			view.LegacyCheckpointJSON,
			view.Snapshot.State,
			view.Snapshot.Run.TemplateRef,
			view.TemplateSourceHash,
			view.LegacyAdminRecords,
			view.LegacyAdminResolutions,
		)
		return err
	})
	return result, err
}
