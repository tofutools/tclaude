package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestTemplateAuthoringSnapshotSerializesSourceAndAuthorship(t *testing.T) {
	ctx := t.Context()
	fs, err := NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "snapshot-consistency",
		Start:      "begin",
		Nodes: map[string]model.Node{
			"begin": {Type: model.NodeTypeStart, Next: model.Next{"pass": "done"}},
			"done":  {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		tmpl.Start: {X: 10, Y: 20},
	}}
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	firstSource, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	firstParsed, err := model.Parse(firstSource)
	require.NoError(t, err)

	hookEntered := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	fs.templateAuthoringSnapshotHook = func() {
		close(hookEntered)
		<-releaseSnapshot
	}
	type snapshotResult struct {
		snapshot TemplateAuthoringSnapshot
		err      error
	}
	snapshotDone := make(chan snapshotResult, 1)
	go func() {
		snapshot, snapshotErr := fs.GetTemplateAuthoringSnapshot(ctx, first.Ref)
		snapshotDone <- snapshotResult{snapshot: snapshot, err: snapshotErr}
	}()
	<-hookEntered // source is read and the per-template lock is still held

	tmpl.Layout.Nodes[tmpl.Start] = model.LayoutNode{X: 30, Y: 40}
	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerStarted)
		_, writerErr := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, firstParsed.SourceHash, "agent:agt_second")
		writerDone <- writerErr
	}()
	<-writerStarted
	select {
	case writerErr := <-writerDone:
		t.Fatalf("writer interleaved with locked snapshot: %v", writerErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSnapshot)

	result := <-snapshotDone
	require.NoError(t, result.err)
	require.Len(t, result.snapshot.Authorship, 1)
	parsedSnapshot, err := model.Parse(result.snapshot.Source)
	require.NoError(t, err)
	assert.Equal(t, parsedSnapshot.SourceHash, result.snapshot.Authorship[0].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_first"), result.snapshot.Authorship[0].Actor)
	require.NoError(t, <-writerDone)

	fs.templateAuthoringSnapshotHook = nil
	after, err := fs.GetTemplateAuthoringSnapshot(ctx, first.Ref)
	require.NoError(t, err)
	require.Len(t, after.Authorship, 2)
	parsedAfter, err := model.Parse(after.Source)
	require.NoError(t, err)
	assert.Equal(t, parsedAfter.SourceHash, after.Authorship[1].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_second"), after.Authorship[1].Actor)
}
