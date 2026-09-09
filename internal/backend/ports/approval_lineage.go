package ports

import "github.com/tofutools/tclaude/internal/backend/model"

// ApprovalLineageProvider interprets native approval modes without preparing a
// process or reading operator settings. The app combines this separate approval
// boundary with sandbox lineage before admitting any delegated launch.
type ApprovalLineageProvider interface {
	ApprovalPosture(model.DesiredConfiguration) model.ApprovalPosture
}
