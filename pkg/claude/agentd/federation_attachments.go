package agentd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Remote mail attachments.
//
// Files travel inline in the encrypted mail payload. The receiver accepts
// them only for a recipient in a group exported to the sender with both
// `mail` and `attachments`, re-derives every name and content type itself,
// accepts only the types below, and bounds what one peer may keep on disk.

// fedAttachmentTypes is the receive (and send) allow-list, by extension.
// Deliberately no HTML, SVG, archives or executables.
var fedAttachmentTypes = map[string]string{
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".txt":   "text/plain; charset=utf-8",
	".log":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".csv":   "text/csv; charset=utf-8",
	".json":  "application/json",
	".yaml":  "text/yaml; charset=utf-8",
	".yml":   "text/yaml; charset=utf-8",
	".diff":  "text/x-diff; charset=utf-8",
	".patch": "text/x-diff; charset=utf-8",
	".pdf":   "application/pdf",
}

// fedAttachmentPeerQuota bounds the attachment bytes one peer may have
// stored here at once.
const fedAttachmentPeerQuota = 64 << 20

// fedAttachmentName gates a file name crossing the federation boundary
// and returns it with its content type.
func fedAttachmentName(name string) (string, string, error) {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if ext := filepath.Ext(base); len(base) > proto.MaxNameLen && len(ext) < 10 {
		base = base[:proto.MaxNameLen-len(ext)] + ext
	}
	clean := proto.SafeName(base, false)
	if clean == "unknown" || strings.HasPrefix(clean, ".") {
		return "", "", fmt.Errorf("attachment name %q is not usable", name)
	}
	ct, ok := fedAttachmentTypes[strings.ToLower(filepath.Ext(clean))]
	if !ok {
		return "", "", fmt.Errorf("attachment %q: type not allowed (allowed: images, text, markdown, csv, json, yaml, diff/patch, pdf)", clean)
	}
	return clean, ct, nil
}

// validateFedAttachments checks count, size and names.
func validateFedAttachments(atts []proto.AttachmentPayload) error {
	if len(atts) > proto.MaxAttachments {
		return fmt.Errorf("at most %d attachments per remote message", proto.MaxAttachments)
	}
	total := 0
	for _, a := range atts {
		if len(a.Data) == 0 {
			return fmt.Errorf("attachment %q is empty", a.Name)
		}
		total += len(a.Data)
		if _, _, err := fedAttachmentName(a.Name); err != nil {
			return err
		}
	}
	if total > proto.MaxAttachmentBytes {
		return fmt.Errorf("attachments total %d bytes, limit %d", total, proto.MaxAttachmentBytes)
	}
	return nil
}

// fedAttachmentsAllowed reports whether conv is in a live group exported to
// peer with both mail and attachments.
func fedAttachmentsAllowed(peer, conv string) bool {
	groups, err := db.ListGroupsForConv(conv)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if fedPeerAllows(peer, g.ID, PermMessageDirect) && fedPeerAllows(peer, g.ID, PermMessageAttachments) {
			return true
		}
	}
	return false
}

func fedAttachmentPeerDir(instance string) string {
	return filepath.Join(operatorMessageAttachmentsBase, "federation", instance)
}

// fedAttachmentUsage sums the bytes stored for one peer.
func fedAttachmentUsage(instance string) int64 {
	var n int64
	_ = filepath.WalkDir(fedAttachmentPeerDir(instance), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// storeFedAttachments writes validated attachments into a fresh directory
// under the peer's attachment root. The caller removes dir if the message
// is not committed; the attachment reconciler sweeps it otherwise.
func storeFedAttachments(instance string, atts []proto.AttachmentPayload) (rows []db.AgentMessageAttachment, dir string, err error) {
	dir = filepath.Join(fedAttachmentPeerDir(instance), convops.GenerateUUID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	used := map[string]bool{}
	for i, a := range atts {
		name, ct, err := fedAttachmentName(a.Name)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, "", err
		}
		// Case-folded: macOS filesystems are case-insensitive.
		if used[strings.ToLower(name)] {
			name = fmt.Sprintf("%d-%s", i+1, name)
		}
		used[strings.ToLower(name)] = true
		path := filepath.Join(dir, name)
		if err := writeNewFile(path, a.Data); err != nil {
			_ = os.RemoveAll(dir)
			return nil, "", err
		}
		rows = append(rows, db.AgentMessageAttachment{Filename: name, ContentType: ct, SizeBytes: int64(len(a.Data)), StoragePath: path})
	}
	return rows, dir, nil
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
