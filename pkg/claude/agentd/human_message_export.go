package agentd

import (
	"archive/zip"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Message export: the Messages tab's "⤓ download" turns one human
// notification into something the operator can hand to someone else.
//
//	/api/human-messages/{id}/export                 the message as Markdown (.md)
//	/api/human-messages/{id}/export?attachments=1   a .zip of message.md plus
//	                                                attachments/<files>
//
// A message without attachments is always served as plain Markdown, even when
// attachments were asked for — a zip holding one file is just friction. The
// Markdown is built here rather than in the browser so the zip's message.md
// and the plain download (and the dashboard's "Copy as Markdown") can never
// drift apart.

// parseHumanMessageExportPath recognises /api/human-messages/{id}/export.
func parseHumanMessageExportPath(p string) (int64, bool) {
	rest, ok := strings.CutPrefix(p, "/api/human-messages/")
	if !ok {
		return 0, false
	}
	idPart, ok := strings.CutSuffix(rest, "/export")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func serveHumanMessageExport(w http.ResponseWriter, r *http.Request, messageID int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	m, err := db.GetHumanMessage(messageID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m == nil {
		http.NotFound(w, r)
		return
	}
	base := humanMessageExportBasename(m)
	withFiles := r.URL.Query().Get("attachments") == "1" && len(m.Attachments) > 0
	if !withFiles {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".md"}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(humanMessageMarkdown(m, humanMessageSenderLabel(m), nil)))
		return
	}

	// Open every file before the first byte goes out: a file reclaimed from
	// disk is then reported in message.md instead of silently missing from
	// the archive, and nothing is committed to the response yet.
	names := humanMessageExportNames(m.Attachments)
	files := make([]*os.File, len(m.Attachments))
	defer func() {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	for i, a := range m.Attachments {
		f, openErr := os.Open(a.StoragePath)
		if openErr != nil {
			names[i] = ""
			continue
		}
		files[i] = f
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".zip"}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	zw := zip.NewWriter(w)
	modified := m.CreatedAt
	if modified.IsZero() {
		modified = time.Now()
	}
	if err := writeZipEntry(zw, "message.md", modified, strings.NewReader(humanMessageMarkdown(m, humanMessageSenderLabel(m), names))); err != nil {
		slog.Warn("dashboard: message export failed", "message", messageID, "error", err)
		return
	}
	for i, f := range files {
		if f == nil {
			continue
		}
		if err := writeZipEntry(zw, "attachments/"+names[i], modified, f); err != nil {
			slog.Warn("dashboard: message export failed", "message", messageID, "error", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		slog.Warn("dashboard: message export failed", "message", messageID, "error", err)
	}
}

func writeZipEntry(zw *zip.Writer, name string, modified time.Time, src io.Reader) error {
	dst, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified})
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

// humanMessageSenderLabel names the sender the way the reader's From row
// does: the agent's title, with its stable id alongside when known.
func humanMessageSenderLabel(m *db.HumanMessage) string {
	agentID := m.FromAgent
	if agentID == "" && m.FromConv != "" {
		agentID, _ = db.AgentIDForConv(m.FromConv)
	}
	title := m.FromTitle
	if title == "" && agentID != "" {
		if names, err := db.PendingNamesByAgent([]string{agentID}); err == nil {
			title = names[agentID]
		}
	}
	id := agentID
	if id == "" {
		id = m.FromConv
	}
	switch {
	case title != "" && id != "":
		return fmt.Sprintf("%s (%s)", title, id)
	case title != "":
		return title
	case id != "":
		return id
	}
	return "unknown sender"
}

// humanMessageMarkdown renders the message as a standalone Markdown document.
// zipNames is nil for a plain .md export, where attachments are listed by name
// only; for a zip it carries each file's archive name ("" for a file that was
// no longer on disk), and the list links into attachments/.
func humanMessageMarkdown(m *db.HumanMessage, sender string, zipNames []string) string {
	var b strings.Builder
	subject := strings.TrimSpace(m.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	fmt.Fprintf(&b, "# %s\n\n", subject)
	fmt.Fprintf(&b, "- **From:** %s\n", sender)
	b.WriteString("- **To:** human operator\n")
	if m.GroupName != "" {
		fmt.Fprintf(&b, "- **Group:** %s\n", m.GroupName)
	}
	if !m.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "- **Date:** %s\n", m.CreatedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "- **Message:** #%d\n\n---\n\n", m.ID)
	if body := strings.TrimSpace(m.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	} else if len(m.Attachments) > 0 {
		b.WriteString("_No message body — the attached file is the notification._\n")
	} else {
		b.WriteString("_Empty message._\n")
	}
	if len(m.Attachments) == 0 {
		return b.String()
	}
	b.WriteString("\n---\n\n## Attachments\n\n")
	for i, a := range m.Attachments {
		size := humanExportSize(a.SizeBytes)
		switch {
		case zipNames == nil:
			fmt.Fprintf(&b, "- %s (%s)\n", a.Filename, size)
		case zipNames[i] == "":
			fmt.Fprintf(&b, "- %s (%s) — no longer available, not included\n", a.Filename, size)
		default:
			fmt.Fprintf(&b, "- [%s](<attachments/%s>) (%s)\n", zipNames[i], zipNames[i], size)
		}
	}
	return b.String()
}

// humanMessageExportNames gives each attachment a safe, unique archive name.
// Stored filenames are already sender-sanitised, but the archive is unpacked on
// someone else's machine, so path separators are stripped again here and
// duplicate names get a " (2)" suffix instead of overwriting each other.
func humanMessageExportNames(attachments []*db.HumanMessageAttachment) []string {
	names := make([]string, len(attachments))
	seen := make(map[string]bool, len(attachments))
	for i, a := range attachments {
		name := path.Base(strings.ReplaceAll(a.Filename, "\\", "/"))
		if name == "." || name == "/" || name == ".." || name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		candidate := name
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for n := 2; seen[strings.ToLower(candidate)]; n++ {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		seen[strings.ToLower(candidate)] = true
		names[i] = candidate
	}
	return names
}

var exportSlugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// humanMessageExportBasename is "msg-<id>-<subject-slug>", capped so the
// download name stays reasonable.
func humanMessageExportBasename(m *db.HumanMessage) string {
	slug := strings.Trim(exportSlugUnsafe.ReplaceAllString(strings.ToLower(m.Subject), "-"), "-")
	if len(slug) > 48 {
		slug = strings.TrimRight(slug[:48], "-")
	}
	if slug == "" {
		return fmt.Sprintf("msg-%d", m.ID)
	}
	return fmt.Sprintf("msg-%d-%s", m.ID, slug)
}

func humanExportSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
}
