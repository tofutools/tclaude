package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestTemplatePutGetIsContentAddressed(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(record.Ref, "demo@sha256:") {
		t.Fatalf("unexpected ref %q", record.Ref)
	}
	again, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != record.Ref {
		t.Fatalf("ref changed: %q != %q", again.Ref, record.Ref)
	}
	tmpl, err := fs.GetTemplate(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.ID != "demo" || tmpl.Start != "implement" {
		t.Fatalf("template = %#v", tmpl)
	}
	records, err := fs.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Ref != record.Ref {
		t.Fatalf("template records = %#v", records)
	}
}

func TestListTemplateHeadsObservesPublishedGenerationAndMigratesLegacyPointer(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)

	alpha := storetest.Template()
	alpha.ID = "alpha"
	alphaRecord, err := fs.PutTemplate(ctx, alpha)
	require.NoError(t, err)
	alphaSource, err := fs.GetTemplateSource(ctx, alphaRecord.Ref)
	require.NoError(t, err)
	alphaParsed, err := model.Parse(alphaSource)
	require.NoError(t, err)
	beta := storetest.Template()
	beta.ID = "beta"
	betaRecord, err := fs.PutTemplate(ctx, beta)
	require.NoError(t, err)
	betaSource, err := fs.GetTemplateSource(ctx, betaRecord.Ref)
	require.NoError(t, err)
	betaParsed, err := model.Parse(betaSource)
	require.NoError(t, err)

	heads, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, []store.TemplateHead{
		{ID: "alpha", Ref: alphaRecord.Ref, SourceHash: alphaParsed.SourceHash},
		{ID: "beta", Ref: betaRecord.Ref, SourceHash: betaParsed.SourceHash},
	}, heads)

	alpha.Description = "new head"
	alphaNext, err := fs.PutTemplate(ctx, alpha)
	require.NoError(t, err)
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, alphaNext.Ref, heads[0].Ref)
	assert.NotEqual(t, alphaParsed.SourceHash, heads[0].SourceHash)
	assert.Equal(t, betaRecord.Ref, heads[1].Ref)
	assert.Equal(t, betaParsed.SourceHash, heads[1].SourceHash)

	// A ref-only legacy pointer is resolved under the lock and rewritten once
	// with its authoritative source generation.
	headPath := filepath.Join(root, "templates", "beta", "head")
	require.NoError(t, os.WriteFile(headPath, []byte(betaRecord.Ref+"\n"), 0o444))
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.TemplateHead{ID: "beta", Ref: betaRecord.Ref, SourceHash: betaParsed.SourceHash}, heads[1])
	migrated, err := os.ReadFile(headPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"ref":"`+betaRecord.Ref+`","sourceHash":"`+betaParsed.SourceHash+`"}`, string(migrated))

	// Structured v1 pointers remain readable and deliberately unattributed;
	// observing one does not scan history or invent a migration actor.
	require.NoError(t, os.WriteFile(headPath, []byte(`{"version":1,"ref":"`+betaRecord.Ref+`","sourceHash":"`+betaParsed.SourceHash+`"}`), 0o444))
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.TemplateHead{ID: "beta", Ref: betaRecord.Ref, SourceHash: betaParsed.SourceHash}, heads[1])

	// Attribution is optional index metadata. If it is malformed while the
	// generation fields remain valid, polling keeps the exact generation and
	// omits attribution rather than falling back to authorship history.
	attributionPath := filepath.Join(root, "templates", "beta", "head-attribution")
	require.NoError(t, os.WriteFile(
		attributionPath,
		[]byte(`{"version":1,"ref":"`+betaRecord.Ref+`","sourceHash":"`+betaParsed.SourceHash+`","actor":"invalid","authoredAt":"not-a-time"}`), 0o644,
	))
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.TemplateHead{ID: "beta", Ref: betaRecord.Ref, SourceHash: betaParsed.SourceHash}, heads[1])

	for name, sidecar := range map[string]string{
		"missing":   "",
		"stale":     `{"version":1,"ref":"beta@sha256:` + strings.Repeat("0", 64) + `","sourceHash":"` + betaParsed.SourceHash + `","actor":"agent:agt_stale","authoredAt":"2026-07-15T08:00:00Z"}`,
		"oversized": strings.Repeat("x", (4<<10)+1),
	} {
		t.Run("optional attribution "+name, func(t *testing.T) {
			if sidecar == "" {
				require.NoError(t, os.Remove(attributionPath))
			} else {
				require.NoError(t, os.WriteFile(attributionPath, []byte(sidecar), 0o644))
			}
			got, listErr := fs.ListTemplateHeads(ctx)
			require.NoError(t, listErr)
			assert.Equal(t, store.TemplateHead{ID: "beta", Ref: betaRecord.Ref, SourceHash: betaParsed.SourceHash}, got[1])
		})
	}

	// A failed first create can leave directories, but never a committed head
	// generation that churns the dashboard signature.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "templates", "orphan", "sha256-dead"), 0o755))
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Len(t, heads, 2)
	heads, err = fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Len(t, heads, 2, "an unchanged orphan never becomes a published generation")
}

func TestListTemplateHeadsDetectsSourceOnlyGeneration(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	tmpl := storetest.Template()
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{tmpl.Start: {X: 10, Y: 20}}}
	first, err := fs.PutTemplateEditorSource(ctx, tmpl, "")
	require.NoError(t, err)
	before, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, before, 1)

	tmpl.Layout.Nodes[tmpl.Start] = model.LayoutNode{X: 90, Y: 100}
	second, err := fs.PutTemplateEditorSource(ctx, tmpl, before[0].SourceHash)
	require.NoError(t, err)
	after, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, first.Ref, second.Ref, "layout-only authoring keeps semantic identity")
	assert.Equal(t, before[0].Ref, after[0].Ref)
	assert.NotEqual(t, before[0].SourceHash, after[0].SourceHash, "head generation follows the CAS authority")
}

func TestListTemplateHeadsBoundsPointerReads(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	tmpl := storetest.Template()
	_, err := fs.PutTemplate(ctx, tmpl)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "templates", tmpl.ID, "head"),
		[]byte(strings.Repeat("x", (4<<10)+1)), 0o644,
	))

	_, err = fs.ListTemplateHeads(ctx)
	require.ErrorContains(t, err, "template head exceeds 4096 bytes")
}

func TestTemplateSourcePreservesAndUpdatesEditorLayoutWithoutChangingRef(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	tmpl := storetest.Template()
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		tmpl.Start: {X: 12, Y: 34},
	}}
	record, err := fs.PutTemplate(ctx, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	source, err := fs.GetTemplateSource(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := model.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Template.Layout.Nodes[tmpl.Start].X; got != 12 {
		t.Fatalf("initial layout x = %v, want 12", got)
	}

	tmpl.Layout.Nodes[tmpl.Start] = model.LayoutNode{X: 98, Y: 76}
	updated, err := fs.PutTemplateEditorSource(ctx, tmpl, parsed.SourceHash)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Ref != record.Ref {
		t.Fatalf("layout-only save ref = %q, want %q", updated.Ref, record.Ref)
	}
	source, err = fs.GetTemplateSource(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = model.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Template.Layout.Nodes[tmpl.Start].X; got != 98 {
		t.Fatalf("updated layout x = %v, want 98", got)
	}
	pinned, err := fs.GetTemplate(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Layout != nil {
		t.Fatalf("run-pinned semantic template unexpectedly contains layout: %#v", pinned.Layout)
	}
}

func TestTemplateAuthorshipAppendsAcrossSourceOnlyEdits(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	now := time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC)
	restore := fs.SetNowForTest(func() time.Time { return now })
	t.Cleanup(restore)

	tmpl := storetest.Template()
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		tmpl.Start: {X: 10, Y: 20},
	}}
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	firstSource, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	firstParsed, err := model.Parse(firstSource)
	require.NoError(t, err)

	now = now.Add(time.Minute)
	tmpl.Layout.Nodes[tmpl.Start] = model.LayoutNode{X: 50, Y: 60}
	second, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, firstParsed.SourceHash, "agent:agt_second")
	require.NoError(t, err)
	require.Equal(t, first.Ref, second.Ref, "layout-only edits retain semantic identity")
	secondSource, err := fs.GetTemplateSource(ctx, second.Ref)
	require.NoError(t, err)
	secondParsed, err := model.Parse(secondSource)
	require.NoError(t, err)

	authorship, err := fs.ListTemplateAuthorship(ctx, first.Ref)
	require.NoError(t, err)
	require.Len(t, authorship, 2)
	assert.Equal(t, first.Ref, authorship[0].Ref)
	assert.Equal(t, firstParsed.SourceHash, authorship[0].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_first"), authorship[0].Actor)
	assert.Equal(t, time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC), authorship[0].AuthoredAt)
	assert.Equal(t, secondParsed.SourceHash, authorship[1].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_second"), authorship[1].Actor)
	assert.NotEqual(t, authorship[0].SourceHash, authorship[1].SourceHash)
	heads, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, heads, 1)
	assert.Equal(t, second.Ref, heads[0].Ref)
	assert.Equal(t, secondParsed.SourceHash, heads[0].SourceHash)
	assert.Equal(t, state.ActorRef("agent:agt_second"), heads[0].Actor)
	require.NotNil(t, heads[0].AuthoredAt)
	assert.Equal(t, authorship[1].AuthoredAt, *heads[0].AuthoredAt)
}

func TestTemplateAuthorshipIsOptionalForLegacyAndValidatesActor(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	require.NoError(t, err)

	authorship, err := fs.ListTemplateAuthorship(ctx, record.Ref)
	require.NoError(t, err)
	assert.Empty(t, authorship)

	_, err = fs.PutTemplateEditorSourceAttributed(ctx, storetest.Template(), "", "not-an-actor")
	require.ErrorContains(t, err, "invalid process template authoring actor")
}

func TestTemplateHeadWriteReadBoundAndOversizeRollback(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	now := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	restore := fs.SetNowForTest(func() time.Time { return now })
	t.Cleanup(restore)
	tmpl := storetest.Template()
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	parsed, err := model.Parse(source)
	require.NoError(t, err)

	type attributionWire struct {
		Version    int            `json:"version"`
		Ref        string         `json:"ref"`
		SourceHash string         `json:"sourceHash"`
		Actor      state.ActorRef `json:"actor"`
		AuthoredAt time.Time      `json:"authoredAt"`
	}
	var boundaryActor state.ActorRef
	for size := 1; size < 5<<10; size++ {
		actor := state.ActorRef("human:" + strings.Repeat("a", size))
		data, marshalErr := json.Marshal(attributionWire{
			Version: 1, Ref: parsed.Ref, SourceHash: parsed.SourceHash, Actor: actor, AuthoredAt: now,
		})
		require.NoError(t, marshalErr)
		if len(data)+1 == 4<<10 {
			boundaryActor = actor
			break
		}
	}
	require.NotEmpty(t, boundaryActor, "fixture must exactly fill the bounded head including newline")
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", boundaryActor)
	require.NoError(t, err)
	heads, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, heads, 1)
	assert.Equal(t, first.SourceHash, heads[0].SourceHash)
	assert.Equal(t, boundaryActor, heads[0].Actor)
	// A normal attributed write must leave the authoritative pointer readable
	// by the previous release, whose strict decoder knows only v1 fields.
	headData, err := os.ReadFile(filepath.Join(root, "templates", tmpl.ID, "head"))
	require.NoError(t, err)
	var legacy struct {
		Version    int    `json:"version"`
		Ref        string `json:"ref"`
		SourceHash string `json:"sourceHash"`
	}
	dec := json.NewDecoder(strings.NewReader(string(headData)))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&legacy))
	assert.Equal(t, 1, legacy.Version)
	assert.Equal(t, first.Ref, legacy.Ref)
	assert.Equal(t, first.SourceHash, legacy.SourceHash)

	now = now.Add(time.Minute)
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{tmpl.Start: {X: 40, Y: 50}}}
	_, err = fs.PutTemplateEditorSourceAttributed(ctx, tmpl, first.SourceHash, boundaryActor+"a")
	require.ErrorContains(t, err, "template head attribution exceeds 4096 bytes")
	after, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, heads[0], after[0], "oversized attribution must not advance the exact head")
	authorship, err := fs.ListTemplateAuthorship(ctx, first.Ref)
	require.NoError(t, err)
	require.Len(t, authorship, 1, "oversized publication must roll back its appended provenance")
}

func TestTemplateAuthorshipFailureRollsBackFirstCreate(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	tmpl := storetest.Template()
	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	require.NoError(t, os.MkdirAll(versionDir, 0o755))
	authorshipPath := filepath.Join(versionDir, "authorship.jsonl")
	require.NoError(t, os.WriteFile(authorshipPath, []byte("not-json\n"), 0o644))

	_, err = fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_writer")
	require.ErrorContains(t, err, "decode process template authorship")
	_, err = fs.GetTemplateHead(ctx, tmpl.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a failed first save must not become the no-head fallback")
	for _, name := range []string{"template.yaml", "template.json"} {
		_, statErr := os.Stat(filepath.Join(versionDir, name))
		require.ErrorIs(t, statErr, os.ErrNotExist, name)
	}
	corrupt, err := os.ReadFile(authorshipPath)
	require.NoError(t, err)
	assert.Equal(t, "not-json\n", string(corrupt), "rollback preserves the pre-save state")
}

func TestTemplateAuthorshipFailureRollsBackSourceOnlyEdit(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	tmpl := storetest.Template()
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	beforeSource, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	before, err := model.Parse(beforeSource)
	require.NoError(t, err)

	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	authorshipPath := filepath.Join(versionDir, "authorship.jsonl")
	require.NoError(t, os.WriteFile(authorshipPath, []byte("not-json\n"), 0o644))
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		tmpl.Start: {X: 90, Y: 100},
	}}

	_, err = fs.PutTemplateEditorSourceAttributed(ctx, tmpl, before.SourceHash, "agent:agt_second")
	require.ErrorContains(t, err, "decode process template authorship")
	afterSource, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	assert.Equal(t, beforeSource, afterSource, "failed attributed save must not change editor source")
	head, err := fs.GetTemplateHead(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, first.Ref, head.Ref)
}

func TestTemplateAuthorshipCrashIntentRecoversBeforeFirstCreateIsVisible(t *testing.T) {
	ctx := t.Context()
	physicalRoot := t.TempDir()
	rootAlias := filepath.Join(t.TempDir(), "store-alias")
	require.NoError(t, os.Symlink(physicalRoot, rootAlias))
	fs := newStoreAt(t, rootAlias)
	root := canonicalStoreTestRoot(t, rootAlias)
	require.NotEqual(t, rootAlias, root, "exercise the same path-alias class as macOS /var -> /private/var")
	tmpl := storetest.Template()
	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	paths := []string{
		filepath.Join(root, "templates", tmpl.ID, "head"),
		filepath.Join(versionDir, "template.yaml"),
		filepath.Join(versionDir, "template.json"),
		filepath.Join(versionDir, "authorship.jsonl"),
	}
	writeCrashedTemplateSaveIntent(t, root, tmpl.ID, semanticHash, paths)
	require.NoError(t, os.MkdirAll(versionDir, 0o755))
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	semantic, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(paths[1], source, 0o644))
	require.NoError(t, os.WriteFile(paths[2], semantic, 0o644))

	heads, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Empty(t, heads, "head observation must recover and skip a failed-first-create orphan")
	records, err := fs.ListTemplates(ctx)
	require.NoError(t, err)
	assert.Empty(t, records, "list must recover the intent before exposing a commit marker")
	_, err = fs.GetTemplateHead(ctx, tmpl.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	for _, path := range append(paths, filepath.Join(root, "templates", tmpl.ID, ".attributed-save-intent.json")) {
		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist, path)
	}
}

func TestListTemplateHeadsRecoversTransientMovedGeneration(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	root = canonicalStoreTestRoot(t, root)
	tmpl := storetest.Template()
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	before, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, before, 1)

	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	paths := []string{
		filepath.Join(root, "templates", tmpl.ID, "head"),
		filepath.Join(versionDir, "template.yaml"),
		filepath.Join(versionDir, "template.json"),
		filepath.Join(versionDir, "authorship.jsonl"),
	}
	writeCrashedTemplateSaveIntent(t, root, tmpl.ID, semanticHash, paths)
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{tmpl.Start: {X: 123, Y: 456}}}
	partialSource, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	partialParsed, err := model.Parse(partialSource)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(paths[1], partialSource, 0o644))
	transientHead, err := json.Marshal(map[string]any{
		"version": 1, "ref": first.Ref, "sourceHash": partialParsed.SourceHash,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(paths[0], append(transientHead, '\n'), 0o644))

	after, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after, "observation recovers the intent before publishing a transient generation")
	_, statErr := os.Stat(filepath.Join(root, "templates", tmpl.ID, ".attributed-save-intent.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestTemplateAuthorshipCrashIntentRestoresSourceBeforeRead(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	root = canonicalStoreTestRoot(t, root)
	tmpl := storetest.Template()
	first, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	before, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	paths := []string{
		filepath.Join(root, "templates", tmpl.ID, "head"),
		filepath.Join(versionDir, "template.yaml"),
		filepath.Join(versionDir, "template.json"),
		filepath.Join(versionDir, "authorship.jsonl"),
	}
	writeCrashedTemplateSaveIntent(t, root, tmpl.ID, semanticHash, paths)
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		tmpl.Start: {X: 120, Y: 140},
	}}
	partial, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(paths[1], partial, 0o644))

	after, err := fs.GetTemplateSource(ctx, first.Ref)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = os.Stat(filepath.Join(root, "templates", tmpl.ID, ".attributed-save-intent.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGetTemplateExactDoesNotRecoverPendingAttributedSave(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	tmpl := storetest.Template()
	record, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", tmpl.ID, "sha256-"+semanticHash)
	paths := []string{
		filepath.Join(root, "templates", tmpl.ID, "head"),
		filepath.Join(versionDir, "template.yaml"),
		filepath.Join(versionDir, "template.json"),
		filepath.Join(versionDir, "authorship.jsonl"),
	}
	writeCrashedTemplateSaveIntent(t, root, tmpl.ID, semanticHash, paths)
	before := directoryFingerprint(t, filepath.Join(root, "templates", tmpl.ID))

	_, err = fs.GetTemplateExact(ctx, record.Ref)
	require.ErrorIs(t, err, store.ErrTemplateSavePending)
	assert.Equal(t, before, directoryFingerprint(t, filepath.Join(root, "templates", tmpl.ID)))
}

func TestTemplateAuthorshipUnreadableCrashIntentFailsClosed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	tmpl := storetest.Template()
	record, err := fs.PutTemplateEditorSourceAttributed(ctx, tmpl, "", "agent:agt_first")
	require.NoError(t, err)
	intentPath := filepath.Join(root, "templates", tmpl.ID, ".attributed-save-intent.json")
	require.NoError(t, os.WriteFile(intentPath, []byte("not-json\n"), 0o600))

	_, err = fs.GetTemplateSource(ctx, record.Ref)
	require.ErrorContains(t, err, "decode attributed process template save intent")
	_, statErr := os.Stat(intentPath)
	require.NoError(t, statErr, "unrecoverable intent must remain for a later recovery attempt")
}

func writeCrashedTemplateSaveIntent(t *testing.T, root, id, semanticHash string, paths []string) {
	t.Helper()
	files := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			files = append(files, map[string]any{"path": path, "exists": false})
			continue
		}
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		files = append(files, map[string]any{
			"path": path, "data": data, "mode": info.Mode().Perm(), "modTime": info.ModTime(), "exists": true,
		})
	}
	intent, err := json.Marshal(map[string]any{
		"version": 1, "id": id, "semanticHash": semanticHash, "files": files,
	})
	require.NoError(t, err)
	intentPath := filepath.Join(root, "templates", id, ".attributed-save-intent.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(intentPath), 0o755))
	require.NoError(t, os.WriteFile(intentPath, append(intent, '\n'), 0o600))
}

func canonicalStoreTestRoot(t *testing.T, root string) string {
	t.Helper()
	// NewFS canonicalizes its root before any production intent path is built.
	// Handcrafted crash fixtures must do the same; using an unresolved alias in
	// an intent models corrupt/untrusted metadata, which recovery rejects.
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	return resolved
}

func TestTemplateEditorSourceCASRejectsConcurrentLayoutSave(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	first := newStoreAt(t, root)
	second := newStoreAt(t, root)
	base := storetest.Template()
	base.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		base.Start: {X: 1, Y: 2},
	}}
	record, err := first.PutTemplate(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	source, err := first.GetTemplateSource(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := model.Parse(source)
	if err != nil {
		t.Fatal(err)
	}

	left := storetest.Template()
	left.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{left.Start: {X: 10, Y: 20}}}
	right := storetest.Template()
	right.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{right.Start: {X: 30, Y: 40}}}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, save := range []struct {
		fs   *store.FS
		tmpl *model.Template
	}{{first, left}, {second, right}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, saveErr := save.fs.PutTemplateEditorSource(ctx, save.tmpl, parsed.SourceHash)
			results <- saveErr
		}()
	}
	wg.Wait()
	close(results)
	var saved, conflicted int
	for saveErr := range results {
		switch {
		case saveErr == nil:
			saved++
		case errors.Is(saveErr, store.ErrTemplateSourceConflict):
			conflicted++
		default:
			t.Fatalf("unexpected save error: %v", saveErr)
		}
	}
	if saved != 1 || conflicted != 1 {
		t.Fatalf("saved=%d conflicted=%d, want one of each", saved, conflicted)
	}
}

func TestPutTemplateVersionFreezesLegacyHeadBeforePublishing(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	latest := storetest.Template()
	latest.Description = "latest editor head"
	latestRecord, err := fs.PutTemplate(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	headPath := filepath.Join(root, "templates", latest.ID, "head")
	if err := os.Remove(headPath); err != nil {
		t.Fatal(err)
	}
	// A directory at the pointer path forces the pre-publication atomic head
	// write to fail while leaving version directories otherwise writable.
	if err := os.Mkdir(headPath, 0o755); err != nil {
		t.Fatal(err)
	}
	older := storetest.Template()
	older.Description = "older file-backed version"
	if _, err := fs.PutTemplateVersion(ctx, older); err == nil {
		t.Fatal("expected head-freeze failure")
	}
	records, err := fs.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Ref != latestRecord.Ref {
		t.Fatalf("failed head freeze published a version: %#v", records)
	}
	if err := os.Remove(headPath); err != nil {
		t.Fatal(err)
	}
	head, err := fs.GetTemplateHead(ctx, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if head.Ref != latestRecord.Ref {
		t.Fatalf("legacy fallback head = %s, want %s", head.Ref, latestRecord.Ref)
	}
	migrated, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	var pointer struct {
		Version    int    `json:"version"`
		Ref        string `json:"ref"`
		SourceHash string `json:"sourceHash"`
	}
	if err := json.Unmarshal(migrated, &pointer); err != nil {
		t.Fatal(err)
	}
	if pointer.Version != 1 || pointer.Ref != latestRecord.Ref || pointer.SourceHash == "" {
		t.Fatalf("legacy fallback was not materialized as a bounded generation pointer: %#v", pointer)
	}
}

func TestTemplateGetRejectsTamperedContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := splitTemplateRef(record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "templates", "demo", "sha256-"+hash, "template.json")
	tampered := storetest.Template()
	tampered.Nodes["extra"] = model.Node{Type: model.NodeTypeTask}
	data, err := model.CanonicalSemanticJSON(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetTemplate(ctx, record.Ref); !errors.Is(err, store.ErrContentMismatch) {
		t.Fatalf("expected content mismatch, got %v", err)
	}
}

func TestAppendCASConflict(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.LogEntry(runID, "implement", 0)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, conflicts int
	for err := range results {
		switch {
		case err == nil:
			wins++
		case store.IsConflict(err):
			conflicts++
		default:
			t.Fatalf("unexpected append error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestCreateRunConflictIsSerialized(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_race"

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
			_, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, failures int
	for err := range results {
		if err == nil {
			wins++
		} else {
			if !errors.Is(err, store.ErrRunExists) {
				t.Errorf("conflict error = %v, want ErrRunExists", err)
			}
			failures++
		}
	}
	if wins != 1 || failures != 1 {
		t.Fatalf("wins=%d failures=%d", wins, failures)
	}
}

func TestListRuns(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run_b", "run_a"} {
		st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
		if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref, Params: map[string]string{"name": runID}}, st); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := fs.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run_a" || runs[1].ID != "run_b" || runs[0].Params["name"] != "run_a" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestSetProgramsAllowedRequiresDurableAdminAudit(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_programs"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref, AllowPrograms: true}, st); err == nil || !strings.Contains(err.Error(), "admin audit") {
		t.Fatalf("expected create-time opt-in refusal, got %v", err)
	}
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(ctx, runID); err == nil || !strings.Contains(err.Error(), "no admin") {
		t.Fatalf("expected unaudited opt-in refusal, got %v", err)
	}
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	_, err = fs.Append(ctx, runID, 0, []evidence.LogEntry{{
		At:    at,
		Scope: evidence.Scope{Kind: evidence.ScopeRun},
		Kind:  evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminProgramsAllowed,
			At:     at,
			Actor:  "human:test",
			Reason: "explicit test opt-in",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fs.SetProgramsAllowed(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.AllowPrograms {
		t.Fatal("audited program opt-in was not persisted")
	}
}

func TestSetProgramsAllowedRejectsUncommittedAuditLogTail(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_programs_tail"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	entry := evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		Seq:           1,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeRun},
		Kind:          evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminProgramsAllowed,
			Seq:    1,
			At:     at,
			Actor:  "human:test",
			Reason: "uncommitted test opt-in",
		},
	}
	path := filepath.Join(root, "runs", runID, "run", "log.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.AppendLogEntry(file, entry); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(ctx, runID); !errors.Is(err, store.ErrRunInconsistent) {
		t.Fatalf("expected inconsistent audit refusal, got %v", err)
	}
	run, err := fs.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AllowPrograms {
		t.Fatal("uncommitted audit enabled program execution")
	}
}

func TestListRunsToleratesBadRunJSON(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	st := state.New("run_good", record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: "run_good", TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(root, "runs", "run_bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "run.json"), []byte(`{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := fs.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run_bad" || runs[1].ID != "run_good" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestCreateRunStateOnlyLeftoverIsRetriable(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_half_created"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	data, err := state.Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetRun(ctx, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("state-only run should be invisible, got %v", err)
	}
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.LoadRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRoundTripVerifiesEvidenceAndStateAnchors(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.LogEntry(runID, "implement", 0)
	entry.At = time.Date(2026, 7, 9, 16, 30, 15, 120000000, time.FixedZone("TST", 90*60))
	entry.Event.At = entry.At

	result, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	if result.Entries[0].Seq != 1 || result.Entries[0].Event.Seq != 1 {
		t.Fatalf("entry seqs not assigned: %#v", result.Entries[0])
	}

	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
	if diagnostics := evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest); diagnostics.HasErrors() {
		t.Fatalf("state anchor diagnostics: %#v", diagnostics)
	}
	if snapshot.State.LastLogSeq != 1 || snapshot.State.LogChecksum != snapshot.Manifest[0].Checksum {
		t.Fatalf("state anchors = seq %d checksum %q", snapshot.State.LastLogSeq, snapshot.State.LogChecksum)
	}
}

func TestEmptyAppendReturnsCurrentStateAndValidatesRun(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	result, err := fs.Append(ctx, runID, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.RunID != runID {
		t.Fatalf("empty append state = %#v", result.State)
	}
	if _, err := fs.Append(ctx, "../x", 0, nil); err == nil {
		t.Fatal("expected invalid run id error")
	}
}

func TestAppendRejectsStateBehindManifest(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
	entry := storetest.AdminLogEntry(fixture.RunID, "implement", 0)
	_, err := fixture.Store.Append(t.Context(), fixture.RunID, 1, []evidence.LogEntry{entry})
	if !errors.Is(err, store.ErrRunInconsistent) {
		t.Fatalf("expected inconsistent run error, got %v", err)
	}

	snapshot, loadErr := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(snapshot.Manifest) != 1 || snapshot.State.LastLogSeq != 0 {
		t.Fatalf("append mutated stale run: manifest=%d stateSeq=%d", len(snapshot.Manifest), snapshot.State.LastLogSeq)
	}
}

func TestBatchedAppendValidationFailureDoesNotPartiallyCommit(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	valid := storetest.AdminLogEntry(runID, "implement", 0)
	invalid := storetest.AdminLogEntry(runID, "implement", 0)
	invalid.Event.Type = state.EventNodeAttemptSettled

	_, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{valid, invalid})
	if err == nil {
		t.Fatal("expected invalid second event to fail")
	}
	manifest, err := fs.ReadManifest(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	nodeLog, err := fs.ReadNodeLog(ctx, runID, "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 0 || len(nodeLog) != 0 {
		t.Fatalf("batch partially committed: manifest=%d nodeLog=%d", len(manifest), len(nodeLog))
	}
}

func TestAppendTimestampRequirementsInventoryLegacyAdminWrites(t *testing.T) {
	for _, eventType := range []state.EventType{
		state.EventAdminRepairRecorded,
		state.EventAdminProgramsAllowed,
	} {
		t.Run(string(eventType), func(t *testing.T) {
			fs, runID := initializedRun(t)
			entry := storetest.AdminLogEntry(runID, "implement", 0)
			entry.Scope = evidence.Scope{Kind: evidence.ScopeRun}
			entry.Event.Type = eventType
			entry.Event.At = time.Time{}
			// The envelope timestamp cannot substitute for the authority timestamp
			// that the legacy reducer persists into AdminRecords.
			require.False(t, entry.At.IsZero())

			_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{entry})
			require.ErrorContains(t, err, string(eventType)+" requires a timestamp for new writes")
			manifest, readErr := fs.ReadManifest(t.Context(), runID)
			require.NoError(t, readErr)
			runLog, readErr := fs.ReadRunLog(t.Context(), runID)
			require.NoError(t, readErr)
			checkpoint, readErr := fs.LoadRunState(t.Context(), runID)
			require.NoError(t, readErr)
			assert.Empty(t, manifest)
			assert.Empty(t, runLog)
			assert.Empty(t, checkpoint.AdminRecords)
		})
	}

	t.Run("block resolution remains strict", func(t *testing.T) {
		fs, runID := initializedRun(t)
		entry := storetest.AdminLogEntry(runID, "implement", 0)
		entry.Event = &state.Event{Type: state.EventBlockResolutionRecorded, Resolution: &state.BlockResolution{
			NodeID: "implement", BlockedAttempt: 1, Decision: state.BlockDecisionSkip,
			Actor: "human:operator", Reason: "waived", EvidenceRef: "ticket:TCL-523",
		}}
		_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{entry})
		require.ErrorContains(t, err, "block resolution requires timestamp")
		manifest, readErr := fs.ReadManifest(t.Context(), runID)
		require.NoError(t, readErr)
		assert.Empty(t, manifest)
	})
}

func TestRunScopeLogRoundTrip(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.AdminLogEntry(runID, "implement", 0)
	entry.Scope = evidence.Scope{Kind: evidence.ScopeRun}

	if _, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry}); err != nil {
		t.Fatal(err)
	}
	runLog, err := fs.ReadRunLog(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runLog) != 1 || runLog[0].Scope.Kind != evidence.ScopeRun {
		t.Fatalf("run log = %#v", runLog)
	}
	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
}

func TestCrashFixturesAreDetectable(t *testing.T) {
	t.Run("log ahead of manifest", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterLogBeforeManifest)
		snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasDiagnostic(evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs), "log_ahead_of_manifest") {
			t.Fatalf("expected log_ahead_of_manifest")
		}
	})
	t.Run("manifest ahead of state", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
		snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasDiagnostic(evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest), "state_behind_manifest") {
			t.Fatalf("expected state_behind_manifest")
		}
	})
	t.Run("torn final log line", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashTornFinalLogLine)
		_, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		var readErr *evidence.ReadError
		if !errors.As(err, &readErr) || readErr.Kind != evidence.ReadErrorTornTail {
			t.Fatalf("expected torn-tail read error, got %#v", err)
		}
		if readErr.File != "nodes/implement/log.jsonl" {
			t.Fatalf("read error file = %q", readErr.File)
		}
	})
}

func TestArtifactsAndLeases(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	artifact, err := fs.PutArtifact(ctx, runID, "note.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := fs.GetArtifact(ctx, runID, artifact.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("artifact data = %q", data)
	}

	if _, err := fs.AcquireRunLease(ctx, runID, "agent-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.AcquireRunLease(ctx, runID, "agent-b", time.Minute); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("expected held lease, got %v", err)
	}
	if err := fs.ReleaseRunLease(ctx, runID, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.AcquireRunLease(ctx, runID, "agent-b", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactGetRejectsTamperedContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	artifact, err := fs.PutArtifact(ctx, runID, "note.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runs", runID, "artifacts", artifact.SHA256), []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := fs.GetArtifact(ctx, runID, artifact.Ref)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if !errors.Is(readErr, store.ErrContentMismatch) && !errors.Is(closeErr, store.ErrContentMismatch) {
		t.Fatalf("expected content mismatch, data=%q readErr=%v closeErr=%v", data, readErr, closeErr)
	}
}

func TestAcquireRunLeaseRequiresExistingRun(t *testing.T) {
	fs := newStore(t)
	_, err := fs.AcquireRunLease(t.Context(), "missing_run", "agent-a", time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected missing run, got %v", err)
	}
}

func TestAcquireRunLeaseExpiredHolderCanBeReplaced(t *testing.T) {
	fs, runID := initializedRun(t)
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
	if _, err := fs.AcquireRunLease(t.Context(), runID, "agent-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	lease, err := fs.AcquireRunLease(t.Context(), runID, "agent-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "agent-b" {
		t.Fatalf("lease holder = %q", lease.Holder)
	}
	if err := fs.ReleaseRunLease(t.Context(), runID, "agent-a"); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("stale holder release = %v", err)
	}
}

func TestRunLockHonorsContextWhileFlockHeld(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	lockPath := filepath.Join(root, ".locks", runID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fl.Unlock() }()

	lockCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	_, err := fs.AcquireRunLease(lockCtx, runID, "agent-a", time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestNewFSAnchorsRelativeRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	fs, runID := initializedRunAt(t, "relative-store")
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetRun(t.Context(), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "relative-store", "runs", runID, "run.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRootFailsClosedWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if root := store.DefaultRoot(); root != "" {
		t.Fatalf("default root without home = %q", root)
	}
}

func TestPublicRunMethodsRejectUnsafeRunIDs(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	for name, call := range map[string]func() error{
		"get run":       func() error { _, err := fs.GetRun(ctx, "../x"); return err },
		"read manifest": func() error { _, err := fs.ReadManifest(ctx, "../x"); return err },
		"read node log": func() error { _, err := fs.ReadNodeLog(ctx, "../x", "node"); return err },
		"read run log":  func() error { _, err := fs.ReadRunLog(ctx, "../x"); return err },
		"put artifact":  func() error { _, err := fs.PutArtifact(ctx, "../x", "a", strings.NewReader("x")); return err },
		"get artifact": func() error {
			_, err := fs.GetArtifact(ctx, "../x", "artifact:sha256:0000000000000000000000000000000000000000000000000000000000000000")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatalf("expected unsafe run id rejection")
			}
		})
	}
}

func TestConcurrentAppendHammer(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	const writers = 24

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				snapshot, err := fs.LoadRun(ctx, runID)
				if err != nil {
					t.Errorf("load run: %v", err)
					return
				}
				entry := storetest.AdminLogEntry(runID, "implement", 0)
				entry.Event.Reason = "concurrent append probe " + string(rune('a'+i))
				_, err = fs.Append(ctx, runID, snapshot.State.LastLogSeq, []evidence.LogEntry{entry})
				if err == nil {
					return
				}
				if store.IsConflict(err) {
					continue
				}
				t.Errorf("append: %v", err)
				return
			}
		}(i)
	}
	wg.Wait()

	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Manifest) != writers {
		t.Fatalf("manifest len = %d, want %d", len(snapshot.Manifest), writers)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
	if diagnostics := evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest); diagnostics.HasErrors() {
		t.Fatalf("state diagnostics: %#v", diagnostics)
	}
}

func TestLoadRunViewSerializesWithAppend(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	readLocked := make(chan struct{})
	releaseRead := make(chan struct{})
	restore := fs.SetViewerReadHookForTest(func() {
		close(readLocked)
		<-releaseRead
	})

	viewResult := make(chan store.Snapshot, 1)
	viewErr := make(chan error, 1)
	go func() {
		snapshot, err := fs.LoadRunView(ctx, runID)
		viewResult <- snapshot
		viewErr <- err
	}()
	<-readLocked

	appendDone := make(chan error, 1)
	go func() {
		_, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
		appendDone <- err
	}()
	select {
	case err := <-appendDone:
		t.Fatalf("append completed while viewer held the run lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseRead)
	require.NoError(t, <-viewErr)
	assert.Equal(t, int64(0), (<-viewResult).State.LastLogSeq)
	require.NoError(t, <-appendDone)

	restore()
	after, err := fs.LoadRunView(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), after.State.LastLogSeq)
	require.Len(t, after.Manifest, 1)
}

func TestLoadRunViewRejectsIDsBeforeLockSideEffects(t *testing.T) {
	root := t.TempDir()
	fs := newStoreAt(t, root)
	outside := filepath.Join(root, "outside.lock")
	_, err := fs.LoadRunView(t.Context(), "../outside")
	require.Error(t, err)
	_, statErr := os.Stat(outside)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(root, ".locks", "..", "outside.lock"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	_, err = fs.LoadRunView(t.Context(), "safe-missing")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, statErr = os.Stat(filepath.Join(root, ".locks", "safe-missing.lock"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "missing viewer reads must not persist lock files")

	_, err = fs.GetTemplateExact(t.Context(), "../outside@sha256:"+strings.Repeat("a", 64))
	require.ErrorIs(t, err, store.ErrContentMismatch)
	_, statErr = os.Stat(filepath.Join(root, ".locks", "template-..", "outside.lock"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestLoadRunViewResourceBudgets(t *testing.T) {
	t.Run("oversized and growing file", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := initializedRunAt(t, root)
		statePath := filepath.Join(root, "runs", runID, "state.json")
		snapshot, err := fs.LoadRun(t.Context(), runID)
		require.NoError(t, err)
		stateData, err := state.Encode(snapshot.State)
		require.NoError(t, err)
		maxFile := int64(len(stateData) + 64)
		if info, statErr := os.Stat(filepath.Join(root, "runs", runID, "run.json")); statErr == nil && info.Size() > maxFile {
			maxFile = info.Size() + 64
		}
		restoreLimits := fs.SetViewerResourceLimitsForTest(maxFile, maxFile*10, 100, 100)
		defer restoreLimits()
		require.NoError(t, os.Truncate(statePath, maxFile+1))
		_, err = fs.LoadRunView(t.Context(), runID)
		require.ErrorIs(t, err, store.ErrViewerResourceLimit)

		// Restore valid state, then grow it after stat/first read. limit+1 must
		// catch the race without allocating the grown size.
		require.NoError(t, os.WriteFile(statePath, stateData, 0o644))
		var once sync.Once
		restoreHooks := fs.SetViewerIOHooksForTest(func(name string, _ int64) {
			if name == "state.json" {
				once.Do(func() { require.NoError(t, os.Truncate(statePath, maxFile+1)) })
			}
		}, nil)
		defer restoreHooks()
		_, err = fs.LoadRunView(t.Context(), runID)
		require.ErrorIs(t, err, store.ErrViewerResourceLimit)
	})

	t.Run("cumulative bytes records and directory entries", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := initializedRunAt(t, root)
		runDir := filepath.Join(root, "runs", runID)
		nodesDir := filepath.Join(runDir, "nodes")
		for _, id := range []string{"a", "b", "c"} {
			require.NoError(t, os.MkdirAll(filepath.Join(nodesDir, id), 0o755))
		}
		restore := fs.SetViewerResourceLimitsForTest(1<<20, 1<<20, 100, 2)
		_, err := fs.LoadRunView(t.Context(), runID)
		restore()
		require.ErrorIs(t, err, store.ErrViewerResourceLimit)
		for _, id := range []string{"a", "b"} {
			require.NoError(t, os.WriteFile(filepath.Join(nodesDir, id, "log.jsonl"), []byte(strings.Repeat(" \n", 32)), 0o644))
		}

		entry := storetest.AdminLogEntry(runID, "implement", 0)
		_, err = fs.Append(t.Context(), runID, 0, []evidence.LogEntry{entry})
		require.NoError(t, err)
		restore = fs.SetViewerResourceLimitsForTest(1<<20, 1<<20, 1, 100)
		_, err = fs.LoadRunView(t.Context(), runID)
		restore()
		require.ErrorIs(t, err, store.ErrViewerResourceLimit)

		var total int64
		for _, path := range []string{
			filepath.Join(runDir, "run.json"), filepath.Join(runDir, "state.json"),
			filepath.Join(runDir, "manifest.jsonl"), filepath.Join(runDir, "nodes", "a", "log.jsonl"),
			filepath.Join(runDir, "nodes", "b", "log.jsonl"), filepath.Join(runDir, "nodes", "implement", "log.jsonl"),
		} {
			info, statErr := os.Stat(path)
			require.NoError(t, statErr)
			total += info.Size()
		}
		restore = fs.SetViewerResourceLimitsForTest(1<<20, total-1, 100, 100)
		_, err = fs.LoadRunView(t.Context(), runID)
		restore()
		require.ErrorIs(t, err, store.ErrViewerResourceLimit)
	})
}

func TestLoadRunViewCancellationReleasesWriterLock(t *testing.T) {
	assertWriterAcquires := func(t *testing.T, fs *store.FS, runID string) {
		t.Helper()
		appendDone := make(chan error, 1)
		go func() {
			_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
			appendDone <- err
		}()
		select {
		case err := <-appendDone:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("append could not acquire lock after viewer cancellation")
		}
	}

	t.Run("during read", func(t *testing.T) {
		fs, runID := initializedRun(t)
		ctx, cancel := context.WithCancel(t.Context())
		var once sync.Once
		restore := fs.SetViewerIOHooksForTest(func(name string, _ int64) {
			if name == "state.json" {
				once.Do(cancel)
			}
		}, nil)
		defer restore()
		_, err := fs.LoadRunView(ctx, runID)
		require.ErrorIs(t, err, context.Canceled)
		assertWriterAcquires(t, fs, runID)
	})

	t.Run("during decode", func(t *testing.T) {
		fs, runID := initializedRun(t)
		ctx, cancel := context.WithCancel(t.Context())
		decodeStarted := make(chan struct{})
		decodeFinished := make(chan struct{})
		var active atomic.Int32
		var startOnce sync.Once
		restore := fs.SetViewerIOHooksForTest(nil, func(component string) {
			if component == "state" {
				active.Add(1)
				defer active.Add(-1)
				startOnce.Do(func() { close(decodeStarted) })
				<-ctx.Done()
				close(decodeFinished)
			}
		})
		defer restore()
		loadDone := make(chan error, 1)
		go func() { _, err := fs.LoadRunView(ctx, runID); loadDone <- err }()
		<-decodeStarted
		cancel()
		select {
		case err := <-loadDone:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("canceled viewer decode did not promptly release")
		}
		select {
		case <-decodeFinished:
		default:
			t.Fatal("viewer returned while decode work was still active")
		}
		assert.Zero(t, active.Load(), "no decode work may outlive LoadRunView")
		assertWriterAcquires(t, fs, runID)
	})
}

func TestViewerDescriptorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, *store.FS, string)
	}{
		{"node directory symlink", func(t *testing.T, root string, _ *store.FS, runID string) {
			target := t.TempDir()
			nodes := filepath.Join(root, "runs", runID, "nodes")
			require.NoError(t, os.MkdirAll(nodes, 0o755))
			require.NoError(t, os.Symlink(target, filepath.Join(nodes, "linked")))
		}},
		{"node log symlink", func(t *testing.T, root string, fs *store.FS, runID string) {
			_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
			require.NoError(t, err)
			path := filepath.Join(root, "runs", runID, "nodes", "implement", "log.jsonl")
			require.NoError(t, os.Remove(path))
			target := filepath.Join(t.TempDir(), "target")
			require.NoError(t, os.WriteFile(target, []byte("TARGET_SECRET"), 0o644))
			require.NoError(t, os.Symlink(target, path))
		}},
		{"run log directory symlink", func(t *testing.T, root string, _ *store.FS, runID string) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "runs", runID, "run")))
		}},
		{"run log symlink", func(t *testing.T, root string, _ *store.FS, runID string) {
			runLogDir := filepath.Join(root, "runs", runID, "run")
			require.NoError(t, os.MkdirAll(runLogDir, 0o755))
			target := filepath.Join(t.TempDir(), "target")
			require.NoError(t, os.WriteFile(target, []byte("TARGET_SECRET"), 0o644))
			require.NoError(t, os.Symlink(target, filepath.Join(runLogDir, "log.jsonl")))
		}},
		{"viewer lock symlink", func(t *testing.T, root string, _ *store.FS, runID string) {
			path := filepath.Join(root, ".locks", runID+".lock")
			require.NoError(t, os.Remove(path))
			target := filepath.Join(t.TempDir(), "lock-target")
			require.NoError(t, os.WriteFile(target, nil, 0o600))
			require.NoError(t, os.Symlink(target, path))
		}},
		{"state non-regular object", func(t *testing.T, root string, _ *store.FS, runID string) {
			path := filepath.Join(root, "runs", runID, "state.json")
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0o755))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID := initializedRunAt(t, root)
			tc.mutate(t, root, fs, runID)
			_, err := fs.LoadRunView(t.Context(), runID)
			require.ErrorIs(t, err, store.ErrUnsafeRunPath)
		})
	}

	t.Run("template intent symlink", func(t *testing.T) {
		root := t.TempDir()
		fs := newStoreAt(t, root)
		record, err := fs.PutTemplate(t.Context(), storetest.Template())
		require.NoError(t, err)
		intent := filepath.Join(root, "templates", "demo", ".attributed-save-intent.json")
		target := filepath.Join(t.TempDir(), "intent-target")
		require.NoError(t, os.WriteFile(target, []byte("TARGET_SECRET"), 0o600))
		require.NoError(t, os.Symlink(target, intent))
		_, err = fs.GetTemplateExact(t.Context(), record.Ref)
		require.ErrorIs(t, err, store.ErrUnsafeRunPath)
	})
}

func initializedRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	return initializedRunAt(t, t.TempDir())
}

func initializedRunAt(t *testing.T, root string) (*store.FS, string) {
	t.Helper()
	ctx := t.Context()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_test"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	return fs, runID
}

func newStore(t *testing.T) *store.FS {
	t.Helper()
	return newStoreAt(t, t.TempDir())
}

func newStoreAt(t *testing.T, root string) *store.FS {
	t.Helper()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func splitTemplateRef(ref string) (string, string, error) {
	id, hash, ok := strings.Cut(ref, "@sha256:")
	if !ok {
		return "", "", errors.New("invalid template ref")
	}
	return id, hash, nil
}

func directoryFingerprint(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[rel] = sha256.Sum256(data)
		return nil
	}))
	return result
}

func hasDiagnostic(diagnostics evidence.Diagnostics, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
