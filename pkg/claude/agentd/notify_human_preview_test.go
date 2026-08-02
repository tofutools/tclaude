package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestHumanMessageAttachmentPreviewableRequiresRasterBytes(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name        string
		contentType string
		data        []byte
		want        bool
	}{
		{
			name:        "png",
			contentType: "image/png",
			data:        []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
			want:        true,
		},
		{
			name:        "svg is not previewable",
			contentType: "image/svg+xml",
			data:        []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
			want:        false,
		},
		{
			name:        "html cannot claim png",
			contentType: "image/png",
			data:        []byte(`<html><script>alert(1)</script></html>`),
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			require.NoError(t, os.WriteFile(path, tt.data, 0o600))
			assert.Equal(t, tt.want, humanMessageAttachmentPreviewable(&db.HumanMessageAttachment{
				ContentType: tt.contentType,
				StoragePath: path,
			}))
		})
	}
}

func TestHumanMessageAttachmentPreviewableRequiresExistingBytes(t *testing.T) {
	assert.False(t, humanMessageAttachmentPreviewable(&db.HumanMessageAttachment{
		ContentType: "image/png",
		StoragePath: filepath.Join(t.TempDir(), "missing.png"),
	}))
}
