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
// and served with.
//
// A claim the bytes contradict never survives: when sniffing RECOGNISES a
// different type, that type wins, so a .png that is really HTML is recorded
// (and served) as HTML. A previewable claim must additionally be positively
// confirmed — an unrecognised payload calling itself image/png becomes opaque
// bytes rather than something the dashboard would display inline.
//
// Everything else keeps what the sender declared. That matters for honest
// formats Go cannot sniff (AVIF, HEIC, TIFF, SVG): they are simply not
// previewable, which the allowlist already ensures, so there is no reason to
// throw away their real type.
func resolveAttachmentContentType(declared string, head []byte) string {
	// Only an image claim is worth checking. Sniffing is coarse — it reports
	// markdown as text/plain — so letting it rewrite ordinary declarations
	// would lose real information for nothing.
	if !strings.HasPrefix(baseMediaType(declared), "image/") {
		return declared
	}
	sniffed := ""
	if len(head) > 0 {
		sniffed = http.DetectContentType(head)
	}
	switch {
	case baseMediaType(sniffed) == baseMediaType(declared):
		return declared
	// "application/octet-stream" is DetectContentType's "I don't recognise
	// this", not a positive verdict — it can demote a preview claim but must
	// not overwrite an honest type.
	case sniffed != "" && baseMediaType(sniffed) != "application/octet-stream":
		return sniffed
	case attachmentPreviewable(declared):
		return "application/octet-stream"
	}
	return declared
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
