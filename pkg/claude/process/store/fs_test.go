package store_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
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
		t.Fatalf("semantic template unexpectedly contains layout: %#v", pinned.Layout)
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
	assert.Equal(t, store.ActorRef("agent:agt_first"), authorship[0].Actor)
	assert.Equal(t, time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC), authorship[0].AuthoredAt)
	assert.Equal(t, secondParsed.SourceHash, authorship[1].SourceHash)
	assert.Equal(t, store.ActorRef("agent:agt_second"), authorship[1].Actor)
	assert.NotEqual(t, authorship[0].SourceHash, authorship[1].SourceHash)
	heads, err := fs.ListTemplateHeads(ctx)
	require.NoError(t, err)
	require.Len(t, heads, 1)
	assert.Equal(t, second.Ref, heads[0].Ref)
	assert.Equal(t, secondParsed.SourceHash, heads[0].SourceHash)
	assert.Equal(t, store.ActorRef("agent:agt_second"), heads[0].Actor)
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
		Actor      store.ActorRef `json:"actor"`
		AuthoredAt time.Time      `json:"authoredAt"`
	}
	var boundaryActor store.ActorRef
	for size := 1; size < 5<<10; size++ {
		actor := store.ActorRef("human:" + strings.Repeat("a", size))
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
