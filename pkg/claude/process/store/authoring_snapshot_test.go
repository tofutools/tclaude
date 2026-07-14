package store

import (
	"errors"
	"testing"

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
	writerContended := make(chan struct{})
	fs.templateLockContendedHook = func() { close(writerContended) }
	writerDone := make(chan error, 1)
	go func() {
		_, writerErr := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, firstParsed.SourceHash, "agent:agt_second")
		writerDone <- writerErr
	}()
	<-writerContended
	close(releaseSnapshot)

	result := <-snapshotDone
	require.NoError(t, result.err)
	require.Len(t, result.snapshot.Authorship, 1)
	parsedSnapshot, err := model.Parse(result.snapshot.Source)
	require.NoError(t, err)
	assert.Equal(t, parsedSnapshot.SourceHash, result.snapshot.Authorship[0].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_first"), result.snapshot.Authorship[0].Actor)
	require.NoError(t, <-writerDone)

	fs.templateLockContendedHook = nil
	fs.templateAuthoringSnapshotHook = nil
	after, err := fs.GetTemplateAuthoringSnapshot(ctx, first.Ref)
	require.NoError(t, err)
	require.Len(t, after.Authorship, 2)
	parsedAfter, err := model.Parse(after.Source)
	require.NoError(t, err)
	assert.Equal(t, parsedAfter.SourceHash, after.Authorship[1].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_second"), after.Authorship[1].Actor)
}

func TestTemplateAuthoringCommitIsExactWithQueuedLayoutWriter(t *testing.T) {
	ctx := t.Context()
	fs, err := NewFS(t.TempDir())
	require.NoError(t, err)
	base := authoringCommitTestTemplate(10)
	baseCommit, err := fs.PutTemplateEditorSourceAttributed(ctx, base, "", "agent:agt_base")
	require.NoError(t, err)

	writerA := authoringCommitTestTemplate(20)
	sourceA, err := model.CanonicalYAML(writerA)
	require.NoError(t, err)
	hashA := sourceHash(sourceA)
	writerB := authoringCommitTestTemplate(30)
	sourceB, err := model.CanonicalYAML(writerB)
	require.NoError(t, err)
	hashB := sourceHash(sourceB)

	aCommitted := make(chan struct{})
	releaseA := make(chan struct{})
	fs.templateAuthoringCommitHook = func(commit TemplateAuthoringCommit) {
		if commit.Actor == state.ActorRef("agent:agt_a") {
			close(aCommitted)
			<-releaseA
		}
	}
	type commitResult struct {
		commit TemplateAuthoringCommit
		err    error
	}
	aDone := make(chan commitResult, 1)
	go func() {
		commit, commitErr := fs.PutTemplateEditorSourceAttributed(ctx, writerA, baseCommit.SourceHash, "agent:agt_a")
		aDone <- commitResult{commit: commit, err: commitErr}
	}()
	<-aCommitted // A's exact result is fixed, but its write lock remains held.

	bContended := make(chan struct{})
	fs.templateLockContendedHook = func() { close(bContended) }
	bDone := make(chan commitResult, 1)
	go func() {
		commit, commitErr := fs.PutTemplateEditorSourceAttributed(ctx, writerB, hashA, "agent:agt_b")
		bDone <- commitResult{commit: commit, err: commitErr}
	}()
	<-bContended
	close(releaseA)

	resultA := <-aDone
	require.NoError(t, resultA.err)
	resultB := <-bDone
	require.NoError(t, resultB.err)
	assert.Equal(t, hashA, resultA.commit.SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_a"), resultA.commit.Actor)
	assert.Equal(t, hashB, resultB.commit.SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_b"), resultB.commit.Actor)

	fs.templateLockContendedHook = nil
	fs.templateAuthoringCommitHook = nil
	staleEdit := authoringCommitTestTemplate(40)
	_, err = fs.PutTemplateEditorSourceAttributed(ctx, staleEdit, resultA.commit.SourceHash, "agent:agt_a")
	var conflict *TemplateSourceConflictError
	require.True(t, errors.As(err, &conflict), "stale A client must conflict after B commits: %v", err)
	assert.Equal(t, resultB.commit.SourceHash, conflict.CurrentSourceHash)
}

func authoringCommitTestTemplate(x float64) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "commit-consistency",
		Start:      "begin",
		Nodes: map[string]model.Node{
			"begin": {Type: model.NodeTypeStart, Next: model.Next{"pass": "done"}},
			"done":  {Type: model.NodeTypeEnd, Result: "success"},
		},
		Layout: &model.Layout{Nodes: map[string]model.LayoutNode{
			"begin": {X: x, Y: 20},
		}},
	}
}
