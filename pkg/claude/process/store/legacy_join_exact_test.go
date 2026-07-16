package store_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestPinnedTemplateReadsNeverPromoteLegacyMetadataJoin(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "legacy-pinned", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "merge", "right": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Metadata: model.Metadata{"join": "any"}},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)

	for _, load := range []func() (*model.Template, error){
		func() (*model.Template, error) { return fs.GetTemplate(t.Context(), record.Ref) },
		func() (*model.Template, error) { return fs.GetTemplateExact(t.Context(), record.Ref) },
	} {
		pinned, loadErr := load()
		require.NoError(t, loadErr)
		require.Empty(t, pinned.Nodes["merge"].Join)
		require.Equal(t, "any", pinned.Nodes["merge"].Metadata["join"])
		hash, hashErr := model.SemanticHash(pinned)
		require.NoError(t, hashErr)
		require.Equal(t, record.SemanticHash, hash)
	}

	source, err := fs.GetTemplateSource(t.Context(), record.Ref)
	require.NoError(t, err)
	authored, err := model.ParseAuthoring(source)
	require.NoError(t, err)
	require.Equal(t, model.JoinAny, authored.Template.Nodes["merge"].Join)
	require.NotEqual(t, record.SemanticHash, authored.SemanticHash)
}
