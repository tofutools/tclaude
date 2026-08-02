package agentd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestHumanMessageAttachmentMarkdownVerdict(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name        string
		filename    string
		contentType string
		data        []byte
		want        bool
	}{
		{
			name:        "notify-human's own text/markdown",
			filename:    "report.md",
			contentType: "text/markdown; charset=utf-8",
			data:        []byte("# Report\n\nAll green.\n"),
			want:        true,
		},
		{
			name:        "extension alone is enough",
			filename:    "notes.markdown",
			contentType: "application/octet-stream",
			data:        []byte("- one\n- two\n"),
			want:        true,
		},
		{
			name:        "media type alone is enough",
			filename:    "readme",
			contentType: "text/x-markdown",
			data:        []byte("plain enough\n"),
			want:        true,
		},
		{
			name:        "an empty document still renders",
			filename:    "empty.md",
			contentType: "text/markdown",
			data:        nil,
			want:        true,
		},
		{
			name:        "a plain text file is not offered as a document",
			filename:    "run.log",
			contentType: "text/plain; charset=utf-8",
			data:        []byte("started\n"),
			want:        false,
		},
		{
			name:        "an image is not offered as a document",
			filename:    "shot.png",
			contentType: "image/png",
			data:        []byte{0x89, 'P', 'N', 'G'},
			want:        false,
		},
		{
			name:        "binary bytes cannot claim .md",
			filename:    "trojan.md",
			contentType: "text/markdown",
			data:        []byte{'#', ' ', 'h', 0x00, 0x01, 0x02},
			want:        false,
		},
		{
			name:        "invalid UTF-8 cannot claim .md",
			filename:    "latin1.md",
			contentType: "text/markdown",
			data:        []byte{'#', ' ', 0xff, 0xfe, '\n'},
			want:        false,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Named by index: a case name can carry a media type's slash.
			path := filepath.Join(dir, strconv.Itoa(i)+"-"+tt.filename)
			require.NoError(t, os.WriteFile(path, tt.data, 0o600))
			assert.Equal(t, tt.want, humanMessageAttachmentMarkdown(&db.HumanMessageAttachment{
				Filename:    tt.filename,
				ContentType: tt.contentType,
				SizeBytes:   int64(len(tt.data)),
				StoragePath: path,
			}))
		})
	}
}

// A multibyte rune straddling the 512-byte sniff boundary is the sample's
// limitation, not evidence that the file is binary.
func TestHumanMessageAttachmentMarkdownAcceptsRuneAcrossSniffBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wide.md")
	body := strings.Repeat("a", 511) + "√ tail"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	assert.True(t, humanMessageAttachmentMarkdown(&db.HumanMessageAttachment{
		Filename:    "wide.md",
		ContentType: "text/markdown",
		SizeBytes:   int64(len(body)),
		StoragePath: path,
	}))
}

func TestHumanMessageAttachmentMarkdownRejectsOversizedAndMissing(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	require.NoError(t, os.WriteFile(big, []byte("# big\n"), 0o600))
	assert.False(t, humanMessageAttachmentMarkdown(&db.HumanMessageAttachment{
		Filename:    "big.md",
		ContentType: "text/markdown",
		SizeBytes:   maxMarkdownHumanAttachmentBytes + 1,
		StoragePath: big,
	}), "a document too large to lay out is download-only")

	assert.False(t, humanMessageAttachmentMarkdown(&db.HumanMessageAttachment{
		Filename:    "gone.md",
		ContentType: "text/markdown",
		StoragePath: filepath.Join(dir, "gone.md"),
	}), "a file that cannot be read is not claimed to be renderable")
}

// The probe is cached because the dashboard's 2-second poll would otherwise
// reopen every historical document. A cached positive deliberately survives
// file cleanup: the viewer then reaches the authenticated route and shows its
// own missing-file state, instead of the control vanishing from under the
// operator mid-read.
func TestHumanMessageAttachmentMarkdownCachesItsVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cached.md")
	require.NoError(t, os.WriteFile(path, []byte("# cached\n"), 0o600))
	a := &db.HumanMessageAttachment{
		Filename:    "cached.md",
		ContentType: "text/markdown",
		SizeBytes:   9,
		StoragePath: path,
	}
	require.True(t, humanMessageAttachmentMarkdown(a))

	require.NoError(t, os.Remove(path))
	assert.True(t, humanMessageAttachmentMarkdown(a),
		"a probed document stays renderable after cleanup removes the file")

	// A file never probed before cleanup is a different case: there is nothing
	// to serve and nothing cached, so it is not claimed to be renderable.
	assert.False(t, humanMessageAttachmentMarkdown(&db.HumanMessageAttachment{
		Filename:    "never-probed.md",
		ContentType: "text/markdown",
		StoragePath: filepath.Join(t.TempDir(), "never-probed.md"),
	}))
}

// The Markdown and image probes share one cache, so their keys must not be able
// to answer each other's question.
func TestHumanMessageAttachmentVerdictsDoNotShareCacheEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0o600))
	a := &db.HumanMessageAttachment{Filename: "shot.png", ContentType: "image/png", StoragePath: path}
	assert.True(t, humanMessageAttachmentPreviewable(a))
	assert.False(t, humanMessageAttachmentMarkdown(a))
}
