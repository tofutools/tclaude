package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestResolveAttachmentContentType(t *testing.T) {
	assert.Equal(t, "image/png", resolveAttachmentContentType("image/png", pngBytes),
		"honest image keeps its declared type")
	assert.Equal(t, "text/html; charset=utf-8",
		resolveAttachmentContentType("image/png", []byte("<html><body>hi</body></html>")),
		"a mislabeled payload is recorded as what its bytes say")
	assert.Equal(t, "application/octet-stream", resolveAttachmentContentType("image/png", nil),
		"an empty file claiming to be an image is stored as opaque bytes")
	assert.Equal(t, "application/zip", resolveAttachmentContentType("application/zip", pngBytes),
		"non-image declarations are left alone — only inline display needs proof")
}

func TestAttachmentPreviewable(t *testing.T) {
	for contentType, want := range map[string]bool{
		"image/png":                true,
		"image/jpeg":               true,
		"image/gif":                true,
		"image/webp":               true,
		"IMAGE/PNG":                true,
		"image/png; charset=utf-8": true,
		"image/svg+xml":            false, // an active document, not a picture
		"text/html; charset=utf-8": false,
		"application/zip":          false,
		"":                         false,
	} {
		assert.Equalf(t, want, attachmentPreviewable(contentType), "content type %q", contentType)
	}
}

func TestSniffBufferKeepsOnlyTheHead(t *testing.T) {
	var buf sniffBuffer
	n, err := buf.Write(make([]byte, attachmentSniffBytes*3))
	assert.NoError(t, err)
	assert.Equal(t, attachmentSniffBytes*3, n, "the tee must report every byte written")
	assert.Len(t, buf.Bytes(), attachmentSniffBytes)
}
