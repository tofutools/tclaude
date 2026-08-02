package agentd

import (
	"net/http"
	"strings"
)

// The dashboard previews an agent-published image inline instead of making the
// operator download it — the reason `notify-human --attach a.png --attach b.png`
// delivers separate files rather than one zip. Two rules keep that safe and
// honest, both decided HERE on the daemon side (the sender's declared type is
// only a hint):
//
//   - the stored content type is confirmed by sniffing the file's first bytes,
//     so a mislabeled payload cannot enter the browser as an image;
//   - only raster formats are previewable. SVG is deliberately excluded: it is
//     an active document, not a picture.
const attachmentSniffBytes = 512

// previewableImageTypes is the raster allowlist the dashboard may render inline.
var previewableImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// sniffBuffer captures the first attachmentSniffBytes bytes of a stream while
// it is copied to disk, so content sniffing costs no extra read.
type sniffBuffer struct {
	head []byte
}

func (b *sniffBuffer) Write(p []byte) (int, error) {
	if room := attachmentSniffBytes - len(b.head); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.head = append(b.head, p[:room]...)
	}
	return len(p), nil
}

func (b *sniffBuffer) Bytes() []byte { return b.head }

// resolveAttachmentContentType decides the content type an attachment is stored
// and served with. A declared image type survives only when the bytes agree
// with it; anything else falls back to the sniffed type, so a .png that is
// really HTML is recorded (and served) as what it is.
func resolveAttachmentContentType(declared string, head []byte) string {
	if !strings.HasPrefix(strings.ToLower(declared), "image/") {
		return declared
	}
	if len(head) == 0 {
		return "application/octet-stream"
	}
	sniffed := http.DetectContentType(head)
	if baseMediaType(sniffed) == baseMediaType(declared) {
		return declared
	}
	return sniffed
}

func baseMediaType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// attachmentPreviewable reports whether the dashboard may render this
// attachment inline as an image.
func attachmentPreviewable(contentType string) bool {
	return previewableImageTypes[baseMediaType(contentType)]
}
