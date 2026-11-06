package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

// The template readers addressed by one ref share a prologue: the context,
// the ref, the per-template lock, then recovery of an interrupted attributed
// save, all before the read itself.
var lockedTemplateReaders = []struct {
	name string
	// read reports whether the reader got anything back.
	read func(ctx context.Context, fs *store.FS, ref string) (bool, error)
}{
	{"GetTemplate", func(ctx context.Context, fs *store.FS, ref string) (bool, error) {
		got, err := fs.GetTemplate(ctx, ref)
		return got != nil, err
	}},
	{"GetTemplateSource", func(ctx context.Context, fs *store.FS, ref string) (bool, error) {
		got, err := fs.GetTemplateSource(ctx, ref)
		return len(got) > 0, err
	}},
	{"ListTemplateAuthorship", func(ctx context.Context, fs *store.FS, ref string) (bool, error) {
		got, err := fs.ListTemplateAuthorship(ctx, ref)
		return len(got) > 0, err
	}},
	{"GetTemplateAuthoringSnapshot", func(ctx context.Context, fs *store.FS, ref string) (bool, error) {
		got, err := fs.GetTemplateAuthoringSnapshot(ctx, ref)
		return len(got.Source) > 0 || len(got.Authorship) > 0, err
	}},
}

func TestLockedTemplateReadsRefuseACancelledContextAndAMalformedRef(t *testing.T) {
	fs := newStoreAt(t, t.TempDir())
	rec, err := fs.PutTemplateEditorSourceAttributed(t.Context(), storetest.Template(), "", "agent:agt_one")
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, r := range lockedTemplateReaders {
		t.Run(r.name, func(t *testing.T) {
			_, err := r.read(cancelled, fs, rec.Ref)
			assert.ErrorIs(t, err, context.Canceled)
			_, err = r.read(t.Context(), fs, "no-hash")
			assert.EqualError(t, err, `invalid template ref "no-hash"`)
			got, err := r.read(t.Context(), fs, rec.Ref)
			assert.NoError(t, err)
			assert.True(t, got, "the stored version reads")
		})
	}
}

// An interrupted first save leaves its files behind with an intent; each
// reader rolls it back before reading, so the half-written version is never
// read.
func TestLockedTemplateReadsRecoverAnInterruptedSaveFirst(t *testing.T) {
	for _, r := range lockedTemplateReaders {
		t.Run(r.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newStoreAt(t, root)
			root = canonicalStoreTestRoot(t, root)
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
			// A real authorship record, from the same template saved elsewhere.
			donorRoot := t.TempDir()
			donor := newStoreAt(t, donorRoot)
			_, err = donor.PutTemplateEditorSourceAttributed(t.Context(), storetest.Template(), "", "agent:agt_one")
			require.NoError(t, err)
			authorship, err := os.ReadFile(filepath.Join(canonicalStoreTestRoot(t, donorRoot), "templates", tmpl.ID, "sha256-"+semanticHash, "authorship.jsonl"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(paths[3], authorship, 0o644))

			got, err := r.read(t.Context(), fs, tmpl.ID+"@sha256:"+semanticHash)
			assert.False(t, got, "the half-written version is rolled back, not read")
			if r.name == "ListTemplateAuthorship" {
				// A version with no authorship reads as none (legacy versions).
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, store.ErrNotFound)
			}
			for _, path := range paths {
				_, statErr := os.Stat(path)
				assert.ErrorIs(t, statErr, os.ErrNotExist, "rolled back: %s", path)
			}
			_, statErr := os.Stat(filepath.Join(root, "templates", tmpl.ID, ".attributed-save-intent.json"))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the intent is consumed")
		})
	}
}
