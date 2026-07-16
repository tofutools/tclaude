package engine

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// MigrationReadinessAuthority is the schema-7 pre-planning release seam.
// UpgradeNeeded returns a detached candidate; before any
// decision, ConfirmUpgradeNeeded must rederive authority from a coherent
// source view and require an exact match with that candidate. Hosts opt into
// this capability explicitly so generic v6 embedders remain compatible.
type MigrationReadinessAuthority interface {
	UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error)
	ConfirmUpgradeNeeded(context.Context, string, pathv1.UpgradeNeeded) error
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
	if err := authority.ConfirmUpgradeNeeded(ctx, runID, needed); err != nil {
		return PrePlanningDecision{}, fmt.Errorf("migration readiness authority confirmation failed: %w", err)
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
