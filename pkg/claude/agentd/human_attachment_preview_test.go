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
	assert.Equal(t, "application/octet-stream",
		resolveAttachmentContentType("image/png", []byte("\x00\x01unrecognised payload")),
		"a previewable claim sniffing to nothing recognisable is demoted")
	// Go cannot sniff AVIF/HEIC/TIFF/SVG, and none of them are previewable, so
	// an honest file keeps the type that describes it.
	for _, declared := range []string{"image/avif", "image/heic", "image/tiff", "image/svg+xml"} {
		assert.Equal(t, declared, resolveAttachmentContentType(declared, []byte("\x00\x01ftypavif")),
			"an honest non-previewable image keeps its type")
	}
	assert.Equal(t, "text/markdown; charset=utf-8",
		resolveAttachmentContentType("text/markdown; charset=utf-8", []byte("# Notes\n")),
		"sniffing is coarse, so a non-image declaration is left alone")
	assert.Equal(t, "text/html; charset=utf-8",
		resolveAttachmentContentType("image/svg+xml", []byte("<html><body>hi</body></html>")),
		"an image claim contradicted by recognised bytes still loses")
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
