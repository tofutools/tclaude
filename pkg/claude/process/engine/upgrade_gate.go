package engine

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// MigrationReadinessAuthority is the dormant pre-planning seam for the later
// v7 release gate. The live v6 Host deliberately does not hold or call this
// capability in TCL-502.
type MigrationReadinessAuthority interface {
	UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error)
}

type PrePlanningAction string

const (
	PrePlanningDrainLegacy PrePlanningAction = "drain_legacy"
	PrePlanningUpgrade     PrePlanningAction = "upgrade_required"
)

type PrePlanningDecision struct {
	Action        PrePlanningAction
	UpgradeNeeded pathv1.UpgradeNeeded
}

// DecideBeforePlanning consumes only the typed migration-readiness authority.
// It neither plans legacy work nor initializes, reduces, or executes path-v1.
func DecideBeforePlanning(ctx context.Context, authority MigrationReadinessAuthority, runID string) (PrePlanningDecision, error) {
	if authority == nil {
		return PrePlanningDecision{}, fmt.Errorf("migration readiness authority is required")
	}
	needed, err := authority.UpgradeNeeded(ctx, runID)
	if err != nil {
		return PrePlanningDecision{}, err
	}
	if err := pathv1.ValidateUpgradeNeeded(needed); err != nil {
		return PrePlanningDecision{}, fmt.Errorf("invalid migration readiness authority: %w", err)
	}
	if needed.RunID != runID {
		return PrePlanningDecision{}, fmt.Errorf("migration readiness authority returned an invalid binding")
	}
	action := PrePlanningUpgrade
	if len(needed.ActiveLegacyIDs) > 0 {
		action = PrePlanningDrainLegacy
	}
	if needed.Reason == pathv1.UpgradeLegacyDrainRequired && action != PrePlanningDrainLegacy ||
		needed.Reason == pathv1.UpgradeMigrationRequired && action != PrePlanningUpgrade {
		return PrePlanningDecision{}, fmt.Errorf("migration readiness authority returned an inconsistent reason")
	}
	return PrePlanningDecision{Action: action, UpgradeNeeded: needed}, nil
}
