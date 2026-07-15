package view

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// ProjectCurrentPathV1ViewerV2 is the closed-gate schema-7 viewer entrypoint.
// Its only routing authority is the exact validated current checkpoint; it has
// no evidence input and therefore cannot reconstruct or fall back to history.
func ProjectCurrentPathV1ViewerV2(ctx context.Context, checkpointJSON, templateSource []byte) (ViewerV2, error) {
	if _, err := pathv1.VerifyExclusiveInput(ctx, checkpointJSON, templateSource); err != nil {
		return ViewerV2{}, err
	}
	checkpoint, err := pathv1.DecodeCheckpointV7(checkpointJSON)
	if err != nil {
		return ViewerV2{}, err
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		return ViewerV2{}, err
	}
	parsed, err := model.Parse(templateSource)
	if err != nil {
		return ViewerV2{}, err
	}
	view := aggregate.View()
	result := ProjectViewerV2(ViewerV2Input{
		RunID: view.RunID, StateSchemaVersion: pathv1.CheckpointStateSchemaVersion,
		ExactTemplateRef: checkpoint.Initialize.UpgradeNeeded.TemplateRef, ExactTemplate: parsed.Template,
		TemplateSourceHash: parsed.SourceHash, Aggregate: &view,
	})
	return result, nil
}
