package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// groups_export.go is the daemon half of per-group export / import. It
// owns everything the DB layer (db/group_export.go) deliberately does
// not: locating and reading each agent's .jsonl, the on-disk zip
// container, conv-id collision detection + remap, cross-machine path
// rewriting, and the all-or-nothing staging dance around the DB
// transaction.
//
// Endpoints:
//
//	GET  /v1/groups/{name}/export    — download a group as a .zip (CLI)
//	POST /v1/groups/import           — import a .zip (CLI)
//	POST /v1/groups/import/inspect   — dry-run analyse a .zip (CLI)
//	GET  /api/groups/{name}/export   — download a group as a .zip (dashboard)
//	POST /api/groups/import          — import an uploaded .zip (dashboard)
//	POST /api/groups/import/inspect  — dry-run analyse an upload (dashboard)
//
// The /v1 surfaces take the raw .zip as the request body; the dashboard
// surfaces take a multipart/form-data upload (a browser cannot stream a
// raw body with query params). Both shapes funnel through the same
// permission-checked handlers — see readImportUpload.

// maxImportArchiveBytes caps an uploaded import archive. Conversations
// can be large, but a per-group export is not unbounded; the cap is a
// guard against a runaway upload, not a real-world limit.
const maxImportArchiveBytes = 512 << 20 // 512 MiB

// multipartParseMemoryBytes bounds how much of a multipart upload the
// stdlib parser keeps in RAM before spilling parts to temp files. The
// archive is read fully into memory afterwards regardless; this only
// caps the parser's transient buffer.
const multipartParseMemoryBytes = 32 << 20 // 32 MiB

// --- export ---

// handleGroupExport serves the group-export download. It is the single
// handler behind BOTH surfaces:
//
//   - GET /v1/groups/{name}/export — the CLI path. The caller is a real
//     SO_PEERCRED peer; requirePermission gates an agent on groups.export
//     and lets a human straight through.
//   - GET /api/groups/{name}/export — the dashboard button. The dashboard
//     route wraps the request with asDashboardHumanPeer first (the
//     cookie + Origin pin in checkDashboardAuth being the human-consent
//     layer), so requirePermission sees a permission-bypassing human.
//
// Routing both surfaces through this one permission-checked handler —
// rather than letting the dashboard path call serveGroupExport directly
// — keeps groups.export structurally enforced on every path and mirrors
// how the other shared dashboard/v1 handlers (e.g. handleGroupUpdate)
// are wired.
func handleGroupExport(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsExport); !ok {
		return
	}
	serveGroupExport(w, g.Name)
}

// serveGroupExport builds the archive and writes it as a file download.
func serveGroupExport(w http.ResponseWriter, groupName string) {
	archive, err := buildGroupExport(groupName)
	if err != nil {
		if errors.Is(err, db.ErrGroupNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "io", "export: "+err.Error())
		return
	}
	filename := fmt.Sprintf("group-%s-%s%s",
		sanitizeFilenamePart(groupName),
		time.Now().Format("20060102-150405"),
		groupexport.FileExtension)
	w.Header().Set("Content-Type", groupexport.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Tclaude-Export-Filename", filename)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(archive); err != nil {
		slog.Warn("group export: write response failed", "group", groupName, "error", err)
	}
}

// buildGroupExport collects every DB row for the group, reads each
// member's conversation .jsonl, and serializes the whole thing into a
// zip archive. A member whose .jsonl cannot be located is flagged
// Missing — its DB rows still export.
func buildGroupExport(groupName string) ([]byte, error) {
	exp, err := db.CollectGroupExport(groupName)
	if err != nil {
		return nil, err
	}

	for i := range exp.Convs {
		c := &exp.Convs[i]
		if c.Title == "" {
			c.Title = agent.FreshTitle(c.ConvID)
		}
		path, ok := findConvJSONL(c.ConvID)
		if !ok {
			c.Missing = true
			slog.Warn("group export: conv .jsonl not found", "group", groupName, "conv", c.ConvID)
			continue
		}
		content, err := os.ReadFile(path) //nolint:gosec // path is a daemon-resolved .jsonl under ~/.claude
		if err != nil {
			c.Missing = true
			slog.Warn("group export: read conv .jsonl failed",
				"group", groupName, "conv", c.ConvID, "error", err)
			continue
		}
		c.Content = content
		if c.SourceCwd == "" {
			c.SourceCwd = cwdFromJSONL(content)
		}
	}

	archive, err := groupexport.Marshal(exp)
	if err != nil {
		return nil, err
	}

	// Best-effort export audit row. An export mutates nothing else, so a
	// logging failure must never fail the export — log and move on.
	if _, err := db.InsertTransferLog(db.TransferLogEntry{
		Kind:          db.TransferKindExport,
		At:            time.Now().UTC(),
		FormatVersion: exp.FormatVersion,
		SourceGroup:   exp.SourceGroup,
		SourceHome:    exp.SourceHome,
		SourceOS:      exp.SourceOS,
		ResultGroup:   exp.SourceGroup,
		AgentCount:    len(exp.Members),
		MessageCount:  len(exp.Messages),
	}); err != nil {
		slog.Warn("group export: audit log write failed", "group", groupName, "error", err)
	}
	return archive, nil
}

// --- import ---

// importResponse is the JSON body returned by a successful import.
type importResponse struct {
	Group          string            `json:"group"`
	GroupID        int64             `json:"group_id"`
	TargetDir      string            `json:"target_dir"`
	AgentCount     int               `json:"agent_count"`
	MessageCount   int               `json:"message_count"`
	ConvRemaps     map[string]string `json:"conv_remaps,omitempty"`
	Retitled       map[string]string `json:"retitled,omitempty"`
	SkippedAliases []string          `json:"skipped_head_aliases,omitempty"`
	FileWarnings   []string          `json:"file_warnings,omitempty"`
}

// readImportUpload extracts the .zip archive and the into / as
// parameters from an import request, transparently handling both request
// shapes the import endpoints accept:
//
//   - multipart/form-data — the dashboard upload: an "archive" file part
//     plus "into" / "as" form fields. A browser cannot stream a raw body
//     with query params the way the CLI does, so it posts a form.
//   - any other content type — the CLI path: the raw .zip IS the request
//     body and into / as ride in the query string.
//
// The archive is capped at maxImportArchiveBytes either way. On any read
// failure it writes the error response itself and returns ok=false; the
// caller just returns.
func readImportUpload(w http.ResponseWriter, r *http.Request) (archive []byte, into, as string, ok bool) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		// Cap the whole request body before the multipart parser reads it.
		r.Body = http.MaxBytesReader(w, r.Body, maxImportArchiveBytes)
		if err := r.ParseMultipartForm(multipartParseMemoryBytes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"import: read multipart upload: "+err.Error())
			return nil, "", "", false
		}
		// Parts large enough to spill to temp files are no longer needed
		// once the archive is in memory — drop them before returning.
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		into = strings.TrimSpace(r.FormValue("into"))
		as = strings.TrimSpace(r.FormValue("as"))
		file, _, err := r.FormFile("archive")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"import: multipart upload has no 'archive' file part: "+err.Error())
			return nil, "", "", false
		}
		defer func() { _ = file.Close() }()
		archive, err = io.ReadAll(file)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"import: read uploaded archive: "+err.Error())
			return nil, "", "", false
		}
		return archive, into, as, true
	}

	// CLI path: the raw .zip is the request body verbatim.
	into = strings.TrimSpace(r.URL.Query().Get("into"))
	as = strings.TrimSpace(r.URL.Query().Get("as"))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportArchiveBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "import: read archive: "+err.Error())
		return nil, "", "", false
	}
	return body, into, as, true
}

// handleGroupImport imports a group from a .zip archive. It is the
// single permission-checked handler behind BOTH surfaces:
//
//   - POST /v1/groups/import?into=<path>&as=<name> — the CLI path; the
//     raw .zip is the request body. requirePermission gates an agent on
//     groups.import and lets a human straight through.
//   - POST /api/groups/import — the dashboard upload; a multipart form
//     carrying the .zip plus into / as fields. The dashboard route wraps
//     the request with asDashboardHumanPeer first, so requirePermission
//     sees a permission-bypassing human.
//
// Routing both surfaces through this one handler keeps groups.import
// structurally enforced on every path and mirrors handleGroupExport.
//
// Permission slug: groups.import (default human-only).
func handleGroupImport(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermGroupsImport)
	if !ok {
		return
	}

	archive, into, asName, ok := readImportUpload(w, r)
	if !ok {
		return
	}
	if into == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"import: target directory is required (the 'into' query parameter, or the 'into' form field on an upload)")
		return
	}

	resp, status, err := runGroupImport(archive, into, asName, caller)
	if err != nil {
		code := "io"
		switch status {
		case http.StatusBadRequest:
			code = "invalid_arg"
		case http.StatusConflict:
			code = "exists"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- import: dry-run inspection ---

// importInspection is the dry-run analysis of an import archive: the
// manifest summary plus a local-collision report. The dashboard preview
// panel renders it the moment a .zip is picked — so an import is never a
// blind action — and `tclaude agent groups import --dry-run` prints it.
// Producing it writes nothing.
type importInspection struct {
	// Manifest summary — what the archive declares it holds.
	SourceGroup   string `json:"source_group"`
	FormatVersion int    `json:"format_version"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	SourceHome    string `json:"source_home,omitempty"`
	SourceOS      string `json:"source_os,omitempty"`
	ExportedAt    string `json:"exported_at,omitempty"`
	AgentCount    int    `json:"agent_count"`
	MessageCount  int    `json:"message_count"`
	ConvCount     int    `json:"conv_count"`
	MissingConvs  int    `json:"missing_convs"`

	// Target + collision analysis against THIS machine.
	TargetName      string          `json:"target_name"`
	TargetNameValid bool            `json:"target_name_valid"`
	TargetNameError string          `json:"target_name_error,omitempty"`
	GroupNameTaken  bool            `json:"group_name_taken"`
	ConvCollisions  []convCollision `json:"conv_collisions"`
}

// convCollision is one imported conversation whose conv-id already
// exists locally — on import it is remapped to a fresh id and its agent
// retitled "-i-N" (see uniqueImportTitle).
type convCollision struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
}

// inspectGroupImport unmarshals an archive and analyses it against the
// local machine WITHOUT importing anything. It returns:
//
//   - (inspection, 200, nil) — the archive is a well-formed export. The
//     inspection carries the manifest summary and the collision report.
//     A group-name clash or an invalid --as is reported as a FIELD on
//     the inspection, not an error, so the preview can still show the
//     human what the archive holds while flagging the name to fix.
//   - (nil, 400, err) — the archive itself is unusable: not a zip, no
//     manifest, malformed, or a format version this tclaude predates.
//     There is nothing safe to preview; the caller blocks the import.
func inspectGroupImport(archive []byte, asName string) (*importInspection, int, error) {
	exp, err := groupexport.Unmarshal(archive)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}

	insp := &importInspection{
		SourceGroup:    exp.SourceGroup,
		FormatVersion:  exp.FormatVersion,
		SchemaVersion:  exp.SchemaVersion,
		SourceHome:     exp.SourceHome,
		SourceOS:       exp.SourceOS,
		ExportedAt:     exp.ExportedAt,
		AgentCount:     len(exp.Members),
		MessageCount:   len(exp.Messages),
		ConvCount:      len(exp.Convs),
		ConvCollisions: []convCollision{},
	}

	// The effective target name: --as when given, else the exported name.
	targetName := asName
	if targetName == "" {
		targetName = exp.SourceGroup
	}
	insp.TargetName = targetName
	if err := validateGroupName(targetName); err != nil {
		insp.TargetNameError = err.Error()
	} else {
		insp.TargetNameValid = true
		// A group-name collision is not auto-resolved on import — the
		// human must pass --as. Surface it so the preview can say so.
		if g, err := db.GetAgentGroupByName(targetName); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("import: check group name: %w", err)
		} else if g != nil {
			insp.GroupNameTaken = true
		}
	}

	// Conv-id collisions: the same per-conv check runGroupImport applies
	// when it builds convRemap. Walk Convs (not Members) so a conv with
	// no membership row is still surfaced — identical iteration to the
	// importer's remap loop.
	for i := range exp.Convs {
		c := &exp.Convs[i]
		if c.Missing {
			insp.MissingConvs++
		}
		if convExistsLocally(c.ConvID) {
			title := c.Title
			if title == "" {
				title = agent.FreshTitle(c.ConvID)
			}
			insp.ConvCollisions = append(insp.ConvCollisions, convCollision{
				ConvID: c.ConvID,
				Title:  title,
			})
		}
	}
	return insp, http.StatusOK, nil
}

// handleGroupImportInspect serves the import dry-run. It is the single
// permission-checked handler behind BOTH surfaces, mirroring
// handleGroupImport / handleGroupExport:
//
//   - POST /v1/groups/import/inspect — the CLI path (groups import
//     --dry-run); the raw .zip is the request body.
//   - POST /api/groups/import/inspect — the dashboard preview; a
//     multipart upload. The dashboard route wraps the request with
//     asDashboardHumanPeer first.
//
// It accepts the same upload shapes as handleGroupImport and writes
// nothing — the dashboard calls it as soon as a .zip is picked so the
// human sees the manifest summary + collision report before committing.
// Gated on groups.import: inspecting an archive reveals its full
// manifest, so the same slug that gates the import gates the preview.
func handleGroupImportInspect(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermGroupsImport); !ok {
		return
	}
	archive, _, asName, ok := readImportUpload(w, r)
	if !ok {
		return
	}
	insp, status, err := inspectGroupImport(archive, asName)
	if err != nil {
		code := "io"
		if status == http.StatusBadRequest {
			code = "invalid_arg"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, insp)
}

// runGroupImport performs the whole import. It returns (response, 200,
// nil) on success or (nil, status, err) on failure, where status is the
// HTTP code the failure should map to.
//
// Atomicity: the transformed .jsonl files are written to a staging
// directory first; then db.ImportGroup runs the entire DB write — every
// row plus the audit-log entry — in one transaction. Only after that
// transaction commits are the staged files moved into ~/.claude/projects.
// A failure before or during the transaction wipes the staging directory
// and leaves the system exactly as it was: no group, no rows, no files,
// no log entry.
func runGroupImport(archive []byte, into, asName, caller string) (*importResponse, int, error) {
	exp, err := groupexport.Unmarshal(archive)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}

	targetName := asName
	if targetName == "" {
		targetName = exp.SourceGroup
	}
	if err := validateGroupName(targetName); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: invalid group name %q: %w", targetName, err)
	}

	// expandTilde first so a human-typed "~/git/imported" resolves to the
	// home directory rather than a literal "~" segment under the daemon's
	// cwd — matching the spawn / group-default / worktree-picker paths,
	// which all expand tilde before use.
	targetCwd, err := filepath.Abs(expandTilde(into))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: resolve target dir %q: %w", into, err)
	}

	// Group-name collision is NOT auto-resolved: a group name is a
	// human-meaningful identity, so the human must choose it. Conv-ids,
	// by contrast, are mechanical and get silently remapped below.
	if g, err := db.GetAgentGroupByName(targetName); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: check group name: %w", err)
	} else if g != nil {
		return nil, http.StatusConflict, fmt.Errorf(
			"import: a group named %q already exists — pass --as to import under a different name",
			targetName)
	}

	targetHome, err := os.UserHomeDir()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: resolve home dir: %w", err)
	}

	// --- conv-id collision detection + remap ---
	// Every member conv-id maps to a final id: itself when nothing
	// locally collides, a freshly minted UUID when something does. A
	// remapped agent also gets a "-i-N" title suffix so the human can
	// tell the imported copy from the original.
	convRemap := make(map[string]string, len(exp.Convs))
	retitled := make(map[string]string)
	usedTitles := make(map[string]bool)
	for i := range exp.Convs {
		c := &exp.Convs[i]
		if convExistsLocally(c.ConvID) {
			freshID := uuid.NewString()
			convRemap[c.ConvID] = freshID
			newTitle := uniqueImportTitle(c.Title, usedTitles)
			usedTitles[newTitle] = true
			retitled[freshID] = newTitle
		} else {
			convRemap[c.ConvID] = c.ConvID
		}
	}

	// --- stage the transformed .jsonl files ---
	stagingDir := filepath.Join(targetHome, ".claude", ".tclaude-import-staging-"+uuid.NewString())
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: create staging dir: %w", err)
	}
	// Any early return from here on must wipe the staging dir.
	staged := make(map[string]string) // final conv-id -> staged file path
	fileWarnings := []string{}
	for i := range exp.Convs {
		c := &exp.Convs[i]
		finalID := convRemap[c.ConvID]
		// A Missing conv had no .jsonl at export time — its DB rows still
		// import, but there is no file to stage. An empty-but-present
		// conv is staged normally (a 0-byte .jsonl is a valid, if
		// degenerate, conversation).
		if c.Missing {
			fileWarnings = append(fileWarnings,
				fmt.Sprintf("%s: no conversation .jsonl in archive", finalID))
			continue
		}
		content := transformConvJSONL(c.Content, exp.SourceHome, targetHome,
			c.SourceCwd, targetCwd, convRemap)
		if newTitle, remapped := retitled[finalID]; remapped {
			content = appendCustomTitleTurn(content, finalID, newTitle)
		}
		stagedPath := filepath.Join(stagingDir, finalID+".jsonl")
		if err := os.WriteFile(stagedPath, content, 0o600); err != nil {
			_ = os.RemoveAll(stagingDir)
			return nil, http.StatusInternalServerError,
				fmt.Errorf("import: stage conv %s: %w", finalID, err)
		}
		staged[finalID] = stagedPath
	}

	// --- the transactional DB write ---
	result, err := db.ImportGroup(db.GroupImportPlan{
		Export:     exp,
		TargetName: targetName,
		TargetCwd:  targetCwd,
		ConvRemap:  convRemap,
		ByConv:     caller,
	})
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		if errors.Is(err, db.ErrGroupNameTaken) {
			return nil, http.StatusConflict, fmt.Errorf("import: %w", err)
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("import: %w", err)
	}

	// --- move staged files into place (post-commit) ---
	// The DB transaction has committed; the import has logically
	// succeeded. Moving the .jsonl files is a set of same-filesystem
	// renames into a pre-created directory — about as reliable as a
	// filesystem op gets. A rare per-file failure is reported as a
	// warning rather than failing the whole import, since the group and
	// its rows are already durably in place.
	targetProjectDir := convops.GetClaudeProjectPath(targetCwd)
	if err := os.MkdirAll(targetProjectDir, 0o755); err != nil {
		slog.Error("import: create target project dir failed",
			"dir", targetProjectDir, "error", err)
		fileWarnings = append(fileWarnings, "target project directory could not be created: "+err.Error())
	} else {
		for finalID, stagedPath := range staged {
			dst := filepath.Join(targetProjectDir, finalID+".jsonl")
			if err := moveFile(stagedPath, dst); err != nil {
				slog.Error("import: move staged conv failed",
					"conv", finalID, "error", err)
				fileWarnings = append(fileWarnings,
					fmt.Sprintf("%s: conversation file could not be placed: %v", finalID, err))
			}
		}
	}
	_ = os.RemoveAll(stagingDir)

	// Best-effort: refresh conv_index for the target project dir so the
	// imported agents show up (offline) in listings and the dashboard
	// immediately, rather than only after the next scan.
	if _, err := convops.LoadSessionsIndex(targetProjectDir); err != nil {
		slog.Warn("import: conv_index refresh failed", "dir", targetProjectDir, "error", err)
	}

	collidedOnly := make(map[string]string)
	for old, fresh := range convRemap {
		if old != fresh {
			collidedOnly[old] = fresh
		}
	}
	slog.Info("group import complete",
		"group", result.GroupName, "agents", result.AgentCount,
		"messages", result.MessageCount, "remapped", len(collidedOnly),
		"target", targetCwd)

	return &importResponse{
		Group:          result.GroupName,
		GroupID:        result.GroupID,
		TargetDir:      targetCwd,
		AgentCount:     result.AgentCount,
		MessageCount:   result.MessageCount,
		ConvRemaps:     collidedOnly,
		Retitled:       retitled,
		SkippedAliases: result.HeadAliasesSkipped,
		FileWarnings:   fileWarnings,
	}, http.StatusOK, nil
}

// --- transfer log ---

// transferLogView is the JSON shape of one agent_transfer_log row for
// the GET /v1/groups/transfers listing.
type transferLogView struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	At            string `json:"at"`
	FormatVersion int    `json:"format_version"`
	SourceGroup   string `json:"source_group"`
	SourceHome    string `json:"source_home,omitempty"`
	SourceOS      string `json:"source_os,omitempty"`
	ResultGroup   string `json:"result_group,omitempty"`
	TargetDir     string `json:"target_dir,omitempty"`
	ConvRemaps    string `json:"conv_remaps,omitempty"`
	AgentCount    int    `json:"agent_count"`
	MessageCount  int    `json:"message_count"`
	// ByAgent is the stable agent_id of the caller who ran the export/import
	// (the actor); ByConv is the conv-id snapshot kept for forensics. ByAgent
	// is "" for a human/un-enrolled caller (then only ByConv is meaningful).
	ByAgent string `json:"by_agent,omitempty"`
	ByConv  string `json:"by_conv,omitempty"`
}

// handleGroupTransfers serves GET /v1/groups/transfers — the export /
// import audit log. Read-only, so it is open to any caller, the same
// policy as `groups ls` / `groups members`.
func handleGroupTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	entries, err := db.ListTransferLog(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := make([]transferLogView, 0, len(entries))
	for _, e := range entries {
		out = append(out, transferLogView{
			ID:            e.ID,
			Kind:          e.Kind,
			At:            e.At.Format(time.RFC3339),
			FormatVersion: e.FormatVersion,
			SourceGroup:   e.SourceGroup,
			SourceHome:    e.SourceHome,
			SourceOS:      e.SourceOS,
			ResultGroup:   e.ResultGroup,
			TargetDir:     e.TargetDir,
			ConvRemaps:    e.ConvRemaps,
			AgentCount:    e.AgentCount,
			MessageCount:  e.MessageCount,
			ByAgent:       e.ByAgent,
			ByConv:        e.ByConv,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ---

// findConvJSONL locates a conversation's .jsonl on disk. It prefers the
// path recorded in conv_index, falling back to a glob over every project
// directory (the conv-id is a unique UUID, so a glob match is exact).
func findConvJSONL(convID string) (string, bool) {
	if row, err := db.GetConvIndex(convID); err == nil && row != nil && row.FullPath != "" {
		if fi, statErr := os.Stat(row.FullPath); statErr == nil && !fi.IsDir() {
			return row.FullPath, true
		}
	}
	matches, err := filepath.Glob(filepath.Join(convops.ClaudeProjectsDir(), "*", convID+".jsonl"))
	if err == nil && len(matches) > 0 {
		return matches[0], true
	}
	return "", false
}

// convExistsLocally reports whether a conv-id is already known on this
// machine — as an agent, a conv_index row, a group member, or a .jsonl on
// disk. Any hit means an import must remap that conv-id rather than collide
// with the existing conversation.
func convExistsLocally(convID string) bool {
	if a, err := db.GetAgentByConv(convID); err == nil && a != nil {
		return true
	}
	if row, err := db.GetConvIndex(convID); err == nil && row != nil {
		return true
	}
	if groups, err := db.ListGroupsForConv(convID); err == nil && len(groups) > 0 {
		return true
	}
	if _, ok := findConvJSONL(convID); ok {
		return true
	}
	return false
}

// importSuffixRegex matches a trailing import suffix in either the short
// form `-i-<digits>` or the long form `-import-<digits>`, so a
// re-imported agent's title bumps N rather than nesting. Sibling of
// reincarnateSuffixRegex (`-r-`) and cloneSuffixRegex (`-c-`).
var importSuffixRegex = regexp.MustCompile(`^(.*?)-(?:i|import)-\d+$`)

// uniqueImportTitle picks an imported (and conv-id-remapped) agent's new
// title in the pattern `<base>-i-<N>` — the import sibling of the `-r-N`
// reincarnate and `-c-N` clone conventions. base is the source title
// with any existing `-i-<digits>` / `-import-<digits>` stripped, so a
// re-import bumps N instead of nesting. N is the smallest free slot not
// already present in conv_index and not already handed out in this same
// import (alsoUsed).
func uniqueImportTitle(sourceTitle string, alsoUsed map[string]bool) string {
	base := sourceTitle
	if m := importSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
	}
	prefix := "i-"
	if base != "" {
		prefix = base + "-i-"
	}
	used := scanImportSuffixes(prefix)
	for n := 1; ; n++ {
		cand := prefix + strconv.Itoa(n)
		if !used[n] && !alsoUsed[cand] {
			return cand
		}
	}
}

// scanImportSuffixes walks conv_index and returns the set of integers N
// for which some custom_title equals `<prefix><N>`.
func scanImportSuffixes(prefix string) map[int]bool {
	used := map[int]bool{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return used
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.CustomTitle, prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(r.CustomTitle, prefix)); err == nil {
			used[n] = true
		}
	}
	return used
}

// transformConvJSONL rewrites a conversation .jsonl for the import
// target machine. Two independent rewrites are applied to the raw bytes:
//
//  1. conv-id remap — every (source-id → final-id) pair in convRemap is
//     substituted. conv-ids are 36-char UUIDs, so a plain substring
//     replace cannot mis-hit; this catches the embedded sessionId and
//     any cross-references to other remapped convs.
//  2. path rewrite — the source cwd prefix is rewritten to the target
//     dir, then the source home prefix to the local home. cwd is done
//     first because it is the more specific prefix (it usually sits
//     under home). This covers the per-turn cwd field and absolute paths
//     embedded in tool-call records alike.
//
// Known limitation: the path rewrite is a POSIX prefix substitution. An
// archive exported on Windows and imported on a POSIX host (or vice
// versa) will not have its .jsonl-internal backslash-separated paths
// translated — the structural import (DB rows, projects-dir placement)
// still works cross-OS because those always use the local encoding.
// Linux<->macOS, the supported cross-machine case, is fully handled.
func transformConvJSONL(content []byte, srcHome, dstHome, srcCwd, dstCwd string, convRemap map[string]string) []byte {
	s := string(content)
	for old, fresh := range convRemap {
		if old != fresh && old != "" {
			s = strings.ReplaceAll(s, old, fresh)
		}
	}
	s = rewritePathPrefix(s, srcCwd, dstCwd)
	s = rewritePathPrefix(s, srcHome, dstHome)
	return []byte(s)
}

// rewritePathPrefix replaces every occurrence of oldPrefix with
// newPrefix, but only where the match is a discrete path token — the
// characters on BOTH sides of it are not path-name characters. The
// right-boundary check stops `/home/A` from corrupting `/home/Alice`
// (the `l` after `/home/A` is a name char); the left-boundary check
// stops a match buried mid-token, e.g. the `/home/A` inside
// `keep/home/A`, from being rewritten into a broken `keep<newPrefix>`.
// `/home/A/`, `/home/A"` and a bare trailing `/home/A` still rewrite.
func rewritePathPrefix(s, oldPrefix, newPrefix string) string {
	if oldPrefix == "" || oldPrefix == newPrefix {
		return s
	}
	var b strings.Builder
	atStart := true   // still at the very start of the original input?
	var prevByte byte // last original byte consumed so far
	for {
		i := strings.Index(s, oldPrefix)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		end := i + len(oldPrefix)
		// Left boundary: the byte immediately before the match in the
		// ORIGINAL input. When the match is mid-slice it is s[i-1]; when
		// it is at the slice start it is the last byte of the previous
		// chunk (prevByte), or "start of input" on the first iteration.
		var leftOK bool
		if i > 0 {
			leftOK = !isPathNameByte(s[i-1])
		} else {
			leftOK = atStart || !isPathNameByte(prevByte)
		}
		rightOK := end >= len(s) || !isPathNameByte(s[end])
		if leftOK && rightOK {
			b.WriteString(newPrefix)
		} else {
			b.WriteString(oldPrefix)
		}
		prevByte = s[end-1]
		atStart = false
		s = s[end:]
	}
	return b.String()
}

// isPathNameByte reports whether b can be part of a path-component name.
// A path prefix that is followed by such a byte has NOT ended — it is a
// prefix of a longer, different name.
func isPathNameByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '_', b == '-':
		return true
	default:
		return false
	}
}

// appendCustomTitleTurn appends a custom-title turn to a .jsonl, the same
// shape Claude Code writes for a /rename. A conv_index scan then resolves
// the imported agent's title to the "-i-N"-suffixed name.
func appendCustomTitleTurn(content []byte, sessionID, title string) []byte {
	line, err := json.Marshal(map[string]any{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   sessionID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return content
	}
	out := content
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	return out
}

// cwdFromJSONL best-effort extracts a working directory from the first
// turn that carries a cwd field — used when conv_index has no
// project_path for an exported conv. Claude Code stamps cwd on user
// turns.
func cwdFromJSONL(content []byte) string {
	for i, line := range strings.SplitN(string(content), "\n", 64) {
		if i >= 63 || line == "" {
			continue
		}
		var turn struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal([]byte(line), &turn); err == nil && turn.Cwd != "" {
			return turn.Cwd
		}
	}
	return ""
}

// moveFile renames src to dst, falling back to a copy+remove when the
// two are on different filesystems (rename returns EXDEV).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src) //nolint:gosec // src is a daemon-staged file
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// sanitizeFilenamePart reduces a string to a safe filename fragment —
// alphanumerics, dash and underscore survive, everything else becomes a
// dash. Used for the export's download filename.
func sanitizeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "group"
	}
	return out
}
