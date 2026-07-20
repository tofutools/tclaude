package view

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

// ProjectCurrentPathV1ViewerV2 is the schema-7 viewer entrypoint.
// Its only routing authority is the exact validated current checkpoint; it has
// no evidence input and therefore cannot reconstruct or fall back to history.
func ProjectCurrentPathV1ViewerV2(ctx context.Context, checkpointJSON, templateSource []byte) (ViewerV2, error) {
	return ProjectCurrentPathV1ViewerV2Page(ctx, checkpointJSON, templateSource, RoutingPageRequestV2{})
}

// ProjectCurrentPathV1ViewerV2Page retains the same checkpoint/template-only
// authority seam while selecting a bounded window for each rich detail table.
func ProjectCurrentPathV1ViewerV2Page(ctx context.Context, checkpointJSON, templateSource []byte, page RoutingPageRequestV2) (ViewerV2, error) {
	if _, err := pathv1.VerifyExecutionInput(ctx, checkpointJSON, templateSource); err != nil {
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
	parsed, err := model.ParseExactSource(templateSource)
	if err != nil {
		return ViewerV2{}, err
	}
	view := aggregate.View()
	result := ProjectViewerV2(ViewerV2Input{
		RunID: view.RunID, StateSchemaVersion: pathv1.CheckpointStateSchemaVersion,
		ExactTemplateRef: checkpoint.Initialize.UpgradeNeeded.TemplateRef, ExactTemplate: parsed.Template,
		TemplateSourceHash: parsed.SourceHash, Aggregate: &view, Page: page,
	})
	return result, nil
}

// BuildCurrentPathV1Envelope creates the additive live viewer response for a
// verified schema-7 store snapshot. Migrated legacy evidence may populate the
// historical Report only; routing authority comes only from the exact current
// checkpoint and template source.
func BuildCurrentPathV1Envelope(ctx context.Context, snapshot store.PathV1RunSnapshot) (Envelope, error) {
	return BuildCurrentPathV1EnvelopePage(ctx, snapshot, RoutingPageRequestV2{})
}

// BuildCurrentPathV1EnvelopePage builds the live envelope with one explicitly
// bounded detail window. Graph topology and aggregate state counts are stable
// across pages.
func BuildCurrentPathV1EnvelopePage(ctx context.Context, snapshot store.PathV1RunSnapshot, page RoutingPageRequestV2) (Envelope, error) {
	if snapshot.Checkpoint == nil {
		return Envelope{}, fmt.Errorf("schema-7 checkpoint is required")
	}
	viewer, err := ProjectCurrentPathV1ViewerV2Page(ctx, snapshot.CheckpointJSON, snapshot.TemplateSource, page)
	if err != nil {
		return Envelope{}, err
	}
	status := state.RunStatus(pathv1.CurrentRunStatus(snapshot.Checkpoint))
	if !status.IsValid() {
		return Envelope{}, fmt.Errorf("schema-7 checkpoint has invalid status %q", status)
	}
	verification, historical, historicalTemplate := processverify.PathV1History(ctx, snapshot)
	envelope := NewEnvelope(snapshot.Run.ID, verification)
	envelope.Run.TemplateRef = safeTemplateRef(snapshot.Run.TemplateRef)
	envelope.Run.StoredStatus = status
	envelope.Run.EffectiveStatus = status
	envelope.Run.CreatedAt = snapshot.Run.CreatedAt
	envelope.Run.UpdatedAt = snapshot.Run.UpdatedAt
	envelope.ViewerV2 = viewer
	if historical != nil {
		envelope.Report = Build(*historical, historicalTemplate, verification).Report
	}
	return envelope, nil
}
