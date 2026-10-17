package agentd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Getting a finished capture out of the daemon, for sharing:
//
//	GET  /api/profiles/{id}/archive   every artifact as one zip download
//	POST /api/profiles/{id}/share     mail the artifacts to an agent
//
// Sharing rides the same durable operator-message path as the dashboard's
// message composer: the files are copied into agent-readable message storage
// and arrive as attachments, so the recipient can open them with its own
// tools (`go tool pprof`, reading summary.txt) without dashboard access.

// profileShareNoteMax bounds the operator's optional note on a share.
const profileShareNoteMax = 4096

// profileShareSummaryMax bounds how much of summary.txt is inlined into the
// shared message body; the full file is attached either way.
const profileShareSummaryMax = 8 << 10

var errProfileNotFinished = errors.New("capture has not finished")

// finishedProfileDir resolves a capture that is safe to read in full: it
// exists and nothing is still writing it.
func finishedProfileDir(id string) (string, *profileCapture, error) {
	dir, err := profileDir(id)
	if err != nil {
		return "", nil, errProfileNotFound
	}
	c, err := readProfileMeta(dir)
	if err != nil {
		return "", nil, errProfileNotFound
	}
	if id == runningProfileID() || (c.Status == profileStatusRunning && !profileCaptureStale(c)) {
		return "", nil, errProfileNotFinished
	}
	return dir, c, nil
}

// profileShareFiles lists the regular artifact files of a capture, meta.json
// included, in the profileFiles order.
func profileShareFiles(dir string) []profileFileInfo {
	files := profileFileInventory(dir)
	if fi, err := os.Stat(filepath.Join(dir, profileMetaFile)); err == nil && fi.Mode().IsRegular() {
		files = append(files, profileFileInfo{Name: profileMetaFile, Bytes: fi.Size()})
	}
	return files
}

func writeProfileLookupError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errProfileNotFinished) {
		http.Error(w, "capture is still running", http.StatusConflict)
		return
	}
	http.NotFound(w, r)
}

func handleProfileArchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir, _, err := finishedProfileDir(id)
	if err != nil {
		writeProfileLookupError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="agentd-profile-`+id+`.zip"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := writeProfileArchive(w, dir, "agentd-profile-"+id); err != nil {
		// Headers are already sent; the truncated zip fails to open, which
		// is the only signal left for the browser.
		slog.Warn("profile archive: write failed", "id", id, "error", err)
	}
}

// writeProfileArchive streams the capture's artifacts as a zip with every
// entry under prefix/, so it unpacks into one directory.
func writeProfileArchive(out io.Writer, dir, prefix string) error {
	zw := zip.NewWriter(out)
	for _, f := range profileShareFiles(dir) {
		if err := addProfileArchiveEntry(zw, filepath.Join(dir, f.Name), prefix+"/"+f.Name); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addProfileArchiveEntry(zw *zip.Writer, path, name string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name = name
	// .pprof files are already gzip-compressed; deflating them again only
	// burns CPU.
	if strings.HasSuffix(name, ".pprof") {
		hdr.Method = zip.Store
	} else {
		hdr.Method = zip.Deflate
	}
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

func handleProfileShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		To   string `json:"to"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 6*profileShareNoteMax+1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	req.To = strings.TrimSpace(req.To)
	req.Note = strings.TrimSpace(req.Note)
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "to is required")
		return
	}
	if len(req.Note) > profileShareNoteMax {
		writeError(w, http.StatusBadRequest, "too_large", "note is too long")
		return
	}
	dir, capture, err := finishedProfileDir(id)
	switch {
	case errors.Is(err, errProfileNotFinished):
		writeError(w, http.StatusConflict, "busy", "capture is still running")
		return
	case err != nil:
		writeError(w, http.StatusNotFound, "not_found", "profile not found")
		return
	}
	target, matches, err := agent.ResolveSelector(req.To)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "target matches multiple conversations", "code": "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
		return
	}

	attachments, skipped, destDir, err := copyProfileAttachments(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	subject := "agentd profile " + id
	if capture.Label != "" {
		subject += ": " + capture.Label
	}
	if len([]rune(subject)) > operatorMessageMaxSubject {
		subject = string([]rune(subject)[:operatorMessageMaxSubject])
	}
	msgID, pending, err := queueRegularAgentMessageWithAttachments(&db.AgentMessage{
		FromConv: "", ToConv: target.ConvID, Subject: subject,
		Body:         profileShareBody(dir, capture, req.Note, skipped),
		ToRecipients: []string{target.ConvID}, OperatorAuthored: true,
	}, attachments)
	if err != nil {
		_ = os.RemoveAll(destDir)
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, target.ConvID, full)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	setAuditTargetLabel(r, agent.TitleFor(target.ConvID))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": msgID, "queued": true, "pending": pending, "to": target.ConvID,
		"title": agent.TitleFor(target.ConvID), "files": len(attachments), "skipped": skipped,
	})
}

// copyProfileAttachments copies a capture's artifacts into durable message
// storage. Files over the per-attachment cap (in practice only a long
// execution trace) are left out and reported, not failed on.
func copyProfileAttachments(dir string) ([]db.AgentMessageAttachment, []string, string, error) {
	destDir := filepath.Join(operatorMessageAttachmentsBase, convops.GenerateUUID())
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, nil, "", fmt.Errorf("create attachment directory: %w", err)
	}
	var (
		out     []db.AgentMessageAttachment
		skipped []string
		total   int64
	)
	for _, f := range profileShareFiles(dir) {
		if f.Bytes > spawnAttachmentMaxFileBytes || total+f.Bytes > spawnAttachmentMaxTotalBytes {
			skipped = append(skipped, f.Name)
			continue
		}
		dst := filepath.Join(destDir, f.Name)
		if err := copyOperatorAttachment(filepath.Join(dir, f.Name), dst); err != nil {
			_ = os.RemoveAll(destDir)
			return nil, nil, "", err
		}
		fi, err := os.Stat(dst)
		if err != nil {
			_ = os.RemoveAll(destDir)
			return nil, nil, "", fmt.Errorf("stat attachment: %w", err)
		}
		total += fi.Size()
		ct := mime.TypeByExtension(filepath.Ext(f.Name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, db.AgentMessageAttachment{
			Filename: f.Name, ContentType: ct, SizeBytes: fi.Size(), StoragePath: dst,
		})
	}
	return out, skipped, destDir, nil
}

// profileShareBody is the message text: the operator's note, what the
// attachments are and how to read them, and the capture summary inline so
// the headline numbers are visible without opening anything.
func profileShareBody(dir string, c *profileCapture, note string, skipped []string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Shared agentd profile capture %s (%s, %ds CPU profile", c.ID, c.Status, c.Seconds)
	if c.TraceSeconds > 0 {
		fmt.Fprintf(&b, " + %ds execution trace", c.TraceSeconds)
	}
	b.WriteString("). The capture files are attached:\n")
	b.WriteString("- summary.txt: CPU split (agentd vs child processes), GC rate, subprocess table\n")
	b.WriteString("- *.pprof: Go profiles; inspect with `go tool pprof -top <file>` (cpu, heap, allocs, goroutine, block, mutex)\n")
	b.WriteString("- trace.out: execution trace; `go tool trace trace.out`\n")
	b.WriteString("- goroutines.txt, stats-*.json, meta.json: stack dump, counter snapshots, capture metadata\n")
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "Not attached (over the attachment size limit): %s. Download them from the dashboard's Activity › Performance.\n", strings.Join(skipped, ", "))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, profileSummaryFile)); err == nil && len(raw) > 0 {
		summary := string(raw)
		if len(summary) > profileShareSummaryMax {
			cut := profileShareSummaryMax
			for cut > 0 && !utf8.RuneStart(summary[cut]) {
				cut--
			}
			summary = summary[:cut] + "\n… (truncated; see summary.txt)"
		}
		b.WriteString("\n--- summary.txt ---\n")
		b.WriteString(strings.TrimRight(summary, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
