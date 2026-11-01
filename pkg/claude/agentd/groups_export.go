package agentd

import (
	"bytes"
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
	"github.com/tofutools/tclaude/pkg/claude/common/reflink"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/pwf"
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
	// A group export writes PAF unless asked for the legacy .zip, which an
	// older tclaude reads.
	format := r.URL.Query().Get("format")
	if format == "" {
		format = groupArchivePAF
	}
	if format != groupArchiveZip && format != groupArchivePAF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "format must be paf (the default) or zip")
		return
	}
	carried := carriedWorkflowsFromQuery(r.URL.Query())
	if carried.any() && format != groupArchivePAF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "only a PAF can carry workflows: drop --format zip")
		return
	}
	workflows, ok := loadCarriedWorkflows(w, r, carried, groupOwnWorkflows(g.ID))
	if !ok {
		return
	}
	serveGroupExport(w, g.Name, format, r.URL.Query().Get("allow_flagged") == "true", classify(peerFromContext(r.Context())) == classHuman, workflows)
}

// Archive formats a group export can be written in.
const (
	groupArchiveZip = "zip"
	groupArchivePAF = "paf"
)

// serveGroupExport builds the archive and writes it as a file download.
func serveGroupExport(w http.ResponseWriter, groupName, format string, allowFlagged, byHuman bool, workflows []pwf.Bundle) {
	archive, err := buildGroupExportAs(groupName, format, allowFlagged, byHuman, workflows)
	if err != nil {
		if errors.Is(err, db.ErrGroupNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		var flagged *flaggedGroupExportError
		if errors.As(err, &flagged) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": "flagged_credentials", "findings": flagged.findings})
			return
		}
		writeError(w, http.StatusInternalServerError, "io", "export: "+err.Error())
		return
	}
	ext, contentType := groupexport.FileExtension, groupexport.ContentType
	if format == groupArchivePAF {
		ext, contentType = ".paf", paf.MediaType
	}
	filename := fmt.Sprintf("group-%s-%s%s",
		sanitizeFilenamePart(groupName),
		time.Now().Format("20060102-150405"),
		ext)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Tclaude-Export-Filename", filename)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(archive); err != nil {
		slog.Warn("group export: write response failed", "group", groupName, "error", err)
	}
}

// buildGroupExportAs collects every DB row for the group, reads each
// member's conversation .jsonl, and serializes the whole thing as the v5
// zip or a group PAF carrying the same data. A member whose .jsonl cannot
// be located is flagged Missing — its DB rows still export.
// byHuman records allowFlagged as a human's allowance in privacy.json;
// an agent's allowance passes the export but is not recorded as one.
func buildGroupExportAs(groupName, format string, allowFlagged, byHuman bool, workflows []pwf.Bundle) ([]byte, error) {
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
		if !ok && format == groupArchivePAF {
			// A group PAF records each conversation's harness, so a member
			// on another harness carries its own native history.
			exported, err := exportMemberHistory(c)
			if err != nil {
				slog.Warn("group export: read conv history failed", "group", groupName, "conv", c.ConvID, "error", err)
			}
			if exported {
				continue
			}
		}
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

	var archive []byte
	if format == groupArchivePAF {
		// A group PAF records a credential scan in privacy.json and, like an
		// agent export, refuses credential findings unless allowed.
		findings, scanErr := scanGroupExport(exp)
		if scanErr != nil {
			return nil, scanErr
		}
		findings = append(findings, scanWorkflowBundles(workflows)...)
		if privacy.Blocking(findings) && !allowFlagged {
			return nil, &flaggedGroupExportError{findings: findings}
		}
		archive, err = groupExportPAF(exp, findings, byHuman && allowFlagged && privacy.Blocking(findings), workflows)
	} else {
		archive, err = groupexport.Marshal(exp)
	}
	if err != nil {
		return nil, err
	}

	// Best-effort export audit row. An export mutates nothing else, so a
	// logging failure must never fail the export — log and move on.
	if _, err := db.InsertTransferLog(db.TransferLogEntry{
		Kind:          db.TransferKindExport,
		At:            time.Now().UTC(),
		FormatVersion: groupexport.FormatVersion,
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

// groupExportPAF writes exp as a group PAF, signed when this node already
// has a federation identity (exporting never creates one).
func groupExportPAF(exp *groupexport.Export, findings []paf.Finding, allowed bool, workflows []pwf.Bundle) ([]byte, error) {
	// A PAF bounds its uncompressed size; the PAF default (2 GiB) leaves the
	// same headroom the zip's 512 MiB compressed upload cap does.
	opts := legacy.GroupWriteOptions{Producer: paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}, Findings: findings, AllowedFindings: allowed}
	if id, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		opts.Keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	if len(workflows) > 0 {
		opts.AddContent = func(w *paf.Writer, agents []string) error {
			return writeCarriedWorkflows(w, workflows, agents, false)
		}
	}
	opts.EarlierLosses = map[string]*paf.Losses{}
	for _, c := range exp.Convs {
		if l := storedConvLosses(c.ConvID); l != nil {
			opts.EarlierLosses[c.ConvID] = l
		}
	}
	var buf bytes.Buffer
	if err := legacy.WriteGroupExport(exp, &buf, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flaggedGroupExportError refuses a group PAF export with credential
// findings no human allowed.
type flaggedGroupExportError struct{ findings []paf.Finding }

func (e *flaggedGroupExportError) Error() string {
	return "suspected credentials: use --allow-flagged to export anyway"
}

// scanGroupExport scans a group's conversations and its tclaude state
// (messages, cron bodies, group context) for suspected credentials.
func scanGroupExport(exp *groupexport.Export) ([]paf.Finding, error) {
	c := privacy.NewCollector()
	for _, conv := range exp.Convs {
		if conv.Missing || conv.Content == nil {
			continue
		}
		name := conv.Title
		if name == "" {
			name = conv.ConvID
		}
		if err := c.JSONLines(bytes.NewReader(conv.Content), name+" history", agentRecordLimit()); err != nil {
			return nil, err
		}
	}
	state := *exp
	state.Convs = nil
	raw, err := json.Marshal(&state)
	if err != nil {
		return nil, err
	}
	c.JSON(raw, "group")
	return c.Findings(), nil
}

// decodedGroupArchive is an import archive read into the group export
// model. Archive is the verified group PAF, nil for a zip; Source is
// paf.Format or legacy.SourceGroupExport.
type decodedGroupArchive struct {
	Export  *groupexport.Export
	Archive *paf.Archive
	Source  string
}

func (d *decodedGroupArchive) workflows() []paf.WorkflowItem {
	if d.Archive == nil {
		return nil
	}
	return d.Archive.Manifest.Workflows
}

func (d *decodedGroupArchive) signatures() []pwfSignatureView {
	if d.Archive == nil {
		return nil
	}
	return pwfSignatureViews(d.Archive.Signatures)
}

// decodeGroupArchive reads an import archive: a group PAF, verified and
// read into the group export model, or a group export zip.
func decodeGroupArchive(archive []byte) (*decodedGroupArchive, error) {
	r := bytes.NewReader(archive)
	if !legacy.IsPAF(r, int64(len(archive))) {
		exp, err := groupexport.Unmarshal(archive)
		if err != nil {
			return nil, err
		}
		return &decodedGroupArchive{Export: exp, Source: legacy.SourceGroupExport}, nil
	}
	a, err := paf.Open(r, int64(len(archive)), paf.Options{Purpose: paf.PurposeImport})
	if err != nil {
		return nil, err
	}
	exp, err := legacy.ToGroupExport(a, paf.DefaultLimits().MaxArchiveBytes)
	if err != nil {
		return nil, err
	}
	return &decodedGroupArchive{Export: exp, Archive: a, Source: paf.Format}, nil
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
	// Kept and Dropped list the file's elevated items by what became of them.
	Kept    []string `json:"kept,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
	// PromptWarnings reports what happened to the group's saved-prompt
	// defaults: prompts created here, local versions kept, names dropped.
	PromptWarnings []string `json:"prompt_warnings,omitempty"`
	// Landings says where each conversation landed.
	Landings []*groupMemberLanding `json:"landings,omitempty"`
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
	r = withApprovalFacts(r, func(body []byte) string { return groupImportApprovalFacts(r, body) })
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

	landing, err := parseGroupLandingOptions(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "import: "+err.Error())
		return
	}
	resp, status, err := runGroupImport(archive, into, asName, caller, landing, readImportKeep(r))
	if err != nil {
		code := "io"
		var cut *groupHistoryCutError
		var refused *groupPostureRefusal
		var keep *importKeepError
		switch {
		case errors.As(err, &keep):
			code = keep.code
		case errors.As(err, &cut):
			code = "history_confirmation_required"
		case errors.As(err, &refused):
			code = refused.kind
		case status == http.StatusBadRequest:
			code = "invalid_arg"
		case status == http.StatusConflict:
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
	// Manifest summary — what the archive declares it holds. Source is
	// "paf" for a group PAF, "group-export" for a zip.
	Source      string `json:"source"`
	SourceGroup string `json:"source_group"`
	// CarriedWorkflows are workflows the file also holds. Importing the group
	// stores none of them; a workflow import of the same file reviews each.
	CarriedWorkflows []paf.WorkflowItem `json:"carried_workflows,omitempty"`
	// Signatures are a group PAF's checked signatures.
	Signatures    []pwfSignatureView `json:"signatures,omitempty"`
	FormatVersion int                `json:"format_version"`
	SchemaVersion int                `json:"schema_version,omitempty"`
	SourceHome    string             `json:"source_home,omitempty"`
	SourceOS      string             `json:"source_os,omitempty"`
	ExportedAt    string             `json:"exported_at,omitempty"`
	AgentCount    int                `json:"agent_count"`
	MessageCount  int                `json:"message_count"`
	ConvCount     int                `json:"conv_count"`
	MissingConvs  int                `json:"missing_convs"`

	// Target + collision analysis against THIS machine.
	TargetName      string          `json:"target_name"`
	TargetNameValid bool            `json:"target_name_valid"`
	TargetNameError string          `json:"target_name_error,omitempty"`
	GroupNameTaken  bool            `json:"group_name_taken"`
	ConvCollisions  []convCollision `json:"conv_collisions"`
	// Landings says where each conversation lands: its harness and model
	// and the rung its history takes.
	Landings []*groupMemberLanding `json:"landings"`
	// FileWarnings says what the file asks for and will not get.
	FileWarnings []string `json:"file_warnings,omitempty"`
	// Confirm lists what a human may keep of that.
	Confirm *importConfirmation `json:"confirm"`
	// Findings are this node's own credential scan of the archive.
	Findings []paf.Finding `json:"findings"`
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
func inspectGroupImport(archive []byte, asName, into string, landing groupLandingOptions, keep importKeep) (*importInspection, int, error) {
	decoded, err := decodeGroupArchive(archive)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	exp, source := decoded.Export, decoded.Source
	drops := fileGroupDrops(exp)
	settled := settleFileDrops(drops, keep.preview(), nil)
	if err := canonicalizeImportedPermissionScopes(exp); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	attachmentURL, attachmentLabel, err := reflink.NormalizeOptional(
		exp.Group.AttachmentURL, exp.Group.AttachmentLabel)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: invalid group attachment: %w", err)
	}
	exp.Group.AttachmentURL, exp.Group.AttachmentLabel = attachmentURL, attachmentLabel
	if err := validateImportedConvIDs(exp); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}

	insp := &importInspection{
		CarriedWorkflows: decoded.workflows(),
		Signatures:       decoded.signatures(),
		Source:           source,
		SourceGroup:      exp.SourceGroup,
		FormatVersion:    exp.FormatVersion,
		SchemaVersion:    exp.SchemaVersion,
		SourceHome:       exp.SourceHome,
		SourceOS:         exp.SourceOS,
		ExportedAt:       exp.ExportedAt,
		AgentCount:       len(exp.Members),
		MessageCount:     len(exp.Messages),
		ConvCount:        len(exp.Convs),
		ConvCollisions:   []convCollision{},
	}
	cwd := ""
	if into != "" {
		cwd, _ = filepath.Abs(expandTilde(into))
	}
	insp.FileWarnings = settled.warnings
	insp.Confirm = newImportConfirmation(archiveDigest(archive), drops, keep.human, settled.warnings)
	landings, err := planGroupLandings(exp, decoded.Archive, landing, cwd)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	closeGroupLandings(landings)
	if err := planGroupPostures(landings, exp, cwd); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: %w", err)
	}
	insp.Landings = landings
	if decoded.Archive != nil {
		insp.Findings, err = privacy.ScanArchive(decoded.Archive, agentRecordLimit())
	} else {
		insp.Findings, err = scanGroupExport(exp)
	}
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: scan: %w", err)
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

	// Conv-id collisions: surface every file-bearing Conv plus historical
	// succession endpoints, matching the importer's identity remap set. The
	// latter may legitimately be absent from current membership/Conv stubs.
	seenImportIDs := make(map[string]bool, len(exp.Convs))
	for i := range exp.Convs {
		c := &exp.Convs[i]
		seenImportIDs[c.ConvID] = true
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
	for _, convID := range importedIdentityConvIDs(exp) {
		if seenImportIDs[convID] || !convExistsLocally(convID) {
			continue
		}
		seenImportIDs[convID] = true
		insp.ConvCollisions = append(insp.ConvCollisions, convCollision{
			ConvID: convID,
			Title:  agent.FreshTitle(convID),
		})
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
	if !reviewingForApproval(r) {
		r = withApprovalFacts(r, func(body []byte) string { return groupImportApprovalFacts(r, body) })
		if _, ok := requirePermission(w, r, PermGroupsImport); !ok {
			return
		}
		giveBackSudoUses(r)
	}
	archive, into, asName, ok := readImportUpload(w, r)
	if !ok {
		return
	}
	landing, err := parseGroupLandingOptions(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "import: "+err.Error())
		return
	}
	insp, status, err := inspectGroupImport(archive, asName, into, landing, readImportKeep(r))
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
// directory first, then placed with O_EXCL so a post-preflight local file can
// never be overwritten. Missing archive conversations reserve their actual
// destination path through commit. db.ImportGroup then writes every DB row and
// the audit entry in one transaction. A failure removes only paths this import
// created and leaves no group, rows, files, or log entry.
var groupImportAfterCollisionCheckForTest func()

// SetGroupImportAfterCollisionCheckForTest installs a deterministic file-race hook.
func SetGroupImportAfterCollisionCheckForTest(fn func()) func() {
	old := groupImportAfterCollisionCheckForTest
	groupImportAfterCollisionCheckForTest = fn
	return func() { groupImportAfterCollisionCheckForTest = old }
}

func canonicalizeImportedPermissionScopes(exp *groupexport.Export) error {
	// A pre-v208 database could contain both a legacy slug and its canonical
	// replacement because they were distinct primary-key values at the time.
	// Collapse those archive rows before import, matching the database/config
	// migration rule: an originally canonical row wins over its legacy alias.
	groupPermissions := make([]groupexport.GroupPermission, 0, len(exp.Group.Permissions))
	groupPermissionIndex := make(map[string]int, len(exp.Group.Permissions))
	groupPermissionCanonical := make(map[string]bool, len(exp.Group.Permissions))
	for _, permission := range exp.Group.Permissions {
		originalSlug := permission.Slug
		permission.Slug = db.CanonicalPermissionSlug(originalSlug)
		idx, exists := groupPermissionIndex[permission.Slug]
		originallyCanonical := originalSlug == permission.Slug
		if exists {
			if originallyCanonical && !groupPermissionCanonical[permission.Slug] {
				groupPermissions[idx] = permission
				groupPermissionCanonical[permission.Slug] = true
			}
			continue
		}
		groupPermissionIndex[permission.Slug] = len(groupPermissions)
		groupPermissionCanonical[permission.Slug] = originallyCanonical
		groupPermissions = append(groupPermissions, permission)
	}
	exp.Group.Permissions = groupPermissions
	for i := range exp.Group.Permissions {
		permission := &exp.Group.Permissions[i]
		canonical, err := canonicalImportedPermissionScopeForSlug(permission.Slug, permission.ScopeJSON)
		if err != nil {
			return fmt.Errorf("group permission %q has invalid scope: %w", permission.Slug, err)
		}
		permission.ScopeJSON = canonical
	}

	permissions := make([]groupexport.Permission, 0, len(exp.Permissions))
	type permissionKey struct{ convID, slug string }
	permissionIndex := make(map[permissionKey]int, len(exp.Permissions))
	permissionCanonical := make(map[permissionKey]bool, len(exp.Permissions))
	for _, permission := range exp.Permissions {
		originalSlug := permission.Slug
		permission.Slug = db.CanonicalPermissionSlug(originalSlug)
		key := permissionKey{convID: permission.ConvID, slug: permission.Slug}
		idx, exists := permissionIndex[key]
		originallyCanonical := originalSlug == permission.Slug
		if exists {
			if originallyCanonical && !permissionCanonical[key] {
				permissions[idx] = permission
				permissionCanonical[key] = true
			}
			continue
		}
		permissionIndex[key] = len(permissions)
		permissionCanonical[key] = originallyCanonical
		permissions = append(permissions, permission)
	}
	exp.Permissions = permissions
	for i := range exp.Permissions {
		permission := &exp.Permissions[i]
		canonical, err := canonicalImportedPermissionScopeForSlug(permission.Slug, permission.ScopeJSON)
		if err != nil {
			return fmt.Errorf("permission %s/%s has invalid scope: %w", permission.ConvID, permission.Slug, err)
		}
		if permission.Effect == db.PermEffectDeny && canonical != "" {
			return fmt.Errorf("deny permission %s/%s cannot carry a scope", permission.ConvID, permission.Slug)
		}
		permission.ScopeJSON = canonical
	}
	return nil
}

// canonicalImportedPermissionScopeForSlug rejects exact stable actor IDs.
// Group archives currently carry conversation IDs but no stable-actor mapping,
// so copying an exact target_agent matcher could accidentally authorize the
// original actor still running on this daemon. Relational selectors remain
// safe: an archive does not import lineage, so they naturally fail closed until
// new post-import descendants are spawned.
func canonicalImportedPermissionScopeForSlug(slug, raw string) (string, error) {
	canonical, err := canonicalPermissionScopeForSlug(slug, raw)
	if err != nil || canonical == "" {
		return canonical, err
	}
	scope, _, err := parsePermissionScope(json.RawMessage(canonical))
	if err != nil {
		return "", err
	}
	for _, matcher := range scope[ScopeDimTargetAgent] {
		if !strings.HasPrefix(matcher, "@") {
			return "", fmt.Errorf("exact target_agent matcher %q cannot be imported without a stable-actor mapping", matcher)
		}
	}
	return canonical, nil
}

func runGroupImport(archive []byte, into, asName, caller string, landing groupLandingOptions, keep importKeep) (resp *importResponse, status int, err error) {
	decoded, err := decodeGroupArchive(archive)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	exp := decoded.Export
	drops := fileGroupDrops(exp)
	if err := keep.check(archiveDigest(archive), fileImportItems(drops)); err != nil {
		var refused *importKeepError
		errors.As(err, &refused)
		return nil, refused.status, err
	}
	settled := settleFileDrops(drops, keep, nil)
	if err := canonicalizeImportedPermissionScopes(exp); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	attachmentURL, attachmentLabel, err := reflink.NormalizeOptional(
		exp.Group.AttachmentURL, exp.Group.AttachmentLabel)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: invalid group attachment: %w", err)
	}
	exp.Group.AttachmentURL, exp.Group.AttachmentLabel = attachmentURL, attachmentLabel
	if err := validateImportedConvIDs(exp); err != nil {
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
	// Succession edges are global selector redirects, and legitimate exports
	// may carry a historical endpoint that is not a current member/Conv stub.
	// Give every such endpoint the same collision/remap treatment as Convs so
	// an archive cannot graft an imported edge onto an unrelated local identity.
	for _, convID := range importedIdentityConvIDs(exp) {
		if _, resolved := convRemap[convID]; resolved {
			continue
		}
		if convExistsLocally(convID) {
			convRemap[convID] = uuid.NewString()
		} else {
			convRemap[convID] = convID
		}
	}
	// Members landing through a harness's own history import get the
	// conversation id that import mints; the rest take the file path below.
	fileWarnings := settled.warnings
	landings, err := planGroupLandings(exp, decoded.Archive, landing, targetCwd)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
	}
	defer closeGroupLandings(landings)
	if err := planGroupPostures(landings, exp, targetCwd); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: %w", err)
	}
	if refused := refusedPostures(landings); refused != nil {
		return nil, refused.status, refused
	}
	if err := unconfirmedCuts(landings, landing.ConfirmHistoryLoss); err != nil {
		return nil, http.StatusConflict, err
	}
	installed := map[string]bool{}
	var uninstall []func()
	defer func() {
		if err != nil {
			for _, undo := range uninstall {
				undo()
			}
		}
	}()
	harnessLanded := map[string]bool{}
	for i, l := range landings {
		c := &exp.Convs[i]
		if l.fileLanding {
			continue
		}
		harnessLanded[c.ConvID] = true
		if prev := convRemap[c.ConvID]; prev != c.ConvID {
			delete(retitled, prev)
		}
		if l.history == nil {
			// Configuration only: the member's rows import without history,
			// and its conv_index row records the landing harness so it
			// resumes there.
			c.Missing, c.Content = true, nil
			finalID, title := c.ConvID, c.Title
			if convExistsLocally(c.ConvID) {
				finalID = uuid.NewString()
				title = uniqueImportTitle(c.Title, usedTitles)
				usedTitles[title] = true
				retitled[finalID] = title
			}
			convRemap[c.ConvID] = finalID
			if err := db.SetConvIndexCustomTitle(finalID, title, l.Harness); err != nil {
				return nil, http.StatusInternalServerError, fmt.Errorf("import: index %s: %w", c.Title, err)
			}
			uninstall = append(uninstall, func() { _ = db.DeleteConvIndex(finalID) })
			installed[finalID] = true
			continue
		}
		newID, undo, installErr := installGroupLanding(l, targetCwd)
		if installErr != nil {
			var refused *groupPostureRefusal
			if errors.As(installErr, &refused) {
				return nil, refused.status, refused
			}
			return nil, http.StatusInternalServerError, fmt.Errorf("import: land %s on %s: %w", c.Title, l.Harness, installErr)
		}
		uninstall = append(uninstall, undo)
		installed[newID] = true
		convRemap[c.ConvID] = newID
		title := c.Title
		if convExistsLocally(c.ConvID) {
			title = uniqueImportTitle(c.Title, usedTitles)
			usedTitles[title] = true
			retitled[newID] = title
		}
		if title != "" {
			if err := db.SetConvIndexCustomTitle(newID, title, l.Harness); err != nil {
				return nil, http.StatusInternalServerError, fmt.Errorf("import: title %s: %w", c.Title, err)
			}
		}
	}
	if groupImportAfterCollisionCheckForTest != nil {
		groupImportAfterCollisionCheckForTest()
	}
	targetProjectDir := convops.GetClaudeProjectPath(targetCwd)
	destinationPaths := make(map[string]string, len(exp.Convs))
	for i := range exp.Convs {
		if harnessLanded[exp.Convs[i].ConvID] && !exp.Convs[i].Missing {
			continue
		}
		finalID := convRemap[exp.Convs[i].ConvID]
		dst, err := importConversationPath(targetProjectDir, finalID)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("import: %w", err)
		}
		destinationPaths[finalID] = dst
	}

	// --- stage the transformed .jsonl files ---
	stagingDir := filepath.Join(targetHome, ".claude", ".tclaude-import-staging-"+uuid.NewString())
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("import: create staging dir: %w", err)
	}
	// Any early return from here on must wipe the staging dir.
	staged := make(map[string]string) // final conv-id -> staged file path
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
		if harnessLanded[c.ConvID] {
			continue
		}
		// The member's cost-state is its source's spend: kept, Claude Code
		// would resume the landed conversation with it on its meter and this
		// node would record that spend again.
		content := transformConvJSONL(convops.StripClaudeCostState(c.Content), exp.SourceHome, targetHome,
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

	// Reserve/place every destination with no-clobber semantics before the DB
	// transaction. This closes the preflight/file-placement TOCTOU: a conv file
	// that appeared meanwhile aborts the import instead of being overwritten or
	// silently attached to the imported actor. Any later DB failure removes only
	// files this import created.
	if err := os.MkdirAll(targetProjectDir, 0o755); err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, http.StatusInternalServerError, fmt.Errorf("import: create target project dir: %w", err)
	}
	placed := make([]string, 0, len(staged))
	reservedMissing := make([]string, 0)
	claimedConversationPaths := make(map[string]string, len(exp.Convs))
	cleanupPlaced := func() {
		for _, path := range placed {
			_ = os.Remove(path)
		}
		for _, path := range reservedMissing {
			_ = os.Remove(path)
		}
	}
	for finalID, stagedPath := range staged {
		dst := destinationPaths[finalID]
		if err := moveFile(stagedPath, dst); err != nil {
			cleanupPlaced()
			_ = os.RemoveAll(stagingDir)
			return nil, http.StatusConflict,
				fmt.Errorf("import: reserve conversation %s: %w", finalID, err)
		}
		placed = append(placed, dst)
		claimedConversationPaths[finalID] = dst
	}
	for i := range exp.Convs {
		c := &exp.Convs[i]
		if !c.Missing {
			continue
		}
		finalID := convRemap[c.ConvID]
		dst := destinationPaths[finalID]
		if err := reserveFile(dst); err != nil {
			cleanupPlaced()
			_ = os.RemoveAll(stagingDir)
			return nil, http.StatusConflict,
				fmt.Errorf("import: reserve missing conversation %s: %w", finalID, err)
		}
		reservedMissing = append(reservedMissing, dst)
		claimedConversationPaths[finalID] = dst
	}

	// Embedded saved prompts face the library's own name/body rules; one that
	// fails them is left out, so its reference drops instead of storing it.
	promptWarnings := []string{}
	validPrompts := exp.SavedPrompts[:0]
	for _, raw := range exp.SavedPrompts {
		p, err := normalizeSavedPrompt(db.SavedPrompt{Name: raw.Name, Body: raw.Body})
		if err != nil {
			promptWarnings = append(promptWarnings, fmt.Sprintf("embedded saved prompt %q is invalid here (%s) — skipped", raw.Name, err))
			continue
		}
		validPrompts = append(validPrompts, groupexport.SavedPrompt{Name: p.Name, Body: p.Body})
	}
	exp.SavedPrompts = validPrompts

	// --- the transactional DB write ---
	result, err := db.ImportGroup(db.GroupImportPlan{
		Export:                      exp,
		TargetName:                  targetName,
		TargetCwd:                   targetCwd,
		ConvRemap:                   convRemap,
		ClaimedConversationPaths:    claimedConversationPaths,
		InstalledConversations:      installed,
		MemberPostures:              memberPostures(landings, exp, convRemap),
		ByConv:                      caller,
		CanonicalizePermissionScope: canonicalImportedPermissionScopeForSlug,
	})
	if err != nil {
		cleanupPlaced()
		_ = os.RemoveAll(stagingDir)
		if errors.Is(err, db.ErrGroupNameTaken) || errors.Is(err, db.ErrImportConversationCollision) {
			return nil, http.StatusConflict, fmt.Errorf("import: %w", err)
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("import: %w", err)
	}
	for _, path := range reservedMissing {
		if err := os.Remove(path); err != nil {
			fileWarnings = append(fileWarnings,
				fmt.Sprintf("%s: missing-conversation reservation could not be removed: %v", filepath.Base(path), err))
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
	for i, l := range landings {
		if l.Rung == landingNone {
			continue
		}
		c := &exp.Convs[i]
		var earlier *paf.Losses
		if decoded.Archive != nil && c.AgentKey != "" {
			earlier, _ = decoded.Archive.Losses(c.AgentKey)
		}
		recordConvLosses(convRemap[c.ConvID], earlier, l.Losses)
	}
	saveCutMemberHistories(landings, exp, convRemap, result.GroupName)
	fileWarnings = append(fileWarnings, recordOpenCodeLandings(landings, exp, convRemap, targetCwd)...)
	logKeptImportItems(settled.kept)
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
		FileWarnings:   append(fileWarnings, landingWarnings(landings)...),
		Kept:           settled.kept,
		Dropped:        settled.dropped,
		PromptWarnings: append(promptWarnings, groupImportPromptWarnings(result)...),
		Landings:       landings,
	}, http.StatusOK, nil
}

func groupImportPromptWarnings(result *db.GroupImportResult) []string {
	out := []string{}
	for _, name := range result.PromptsImported {
		out = append(out, fmt.Sprintf("imported saved prompt %q", name))
	}
	for _, name := range result.PromptsKeptLocal {
		out = append(out, fmt.Sprintf("saved prompt %q already exists here with different text — kept the local version", name))
	}
	for _, name := range result.PromptsDropped {
		out = append(out, fmt.Sprintf("default prompt %q is neither in the archive nor here — dropped", name))
	}
	return out
}

func validateImportedConvIDs(exp *groupexport.Export) error {
	for _, convID := range importedIdentityConvIDs(exp) {
		if harness.ValidOpenCodeConvID(convID) {
			continue
		}
		parsed, err := uuid.Parse(convID)
		if err != nil || parsed == uuid.Nil || parsed.String() != convID {
			return fmt.Errorf("conversation id %q is not a canonical UUID or OpenCode session id", convID)
		}
	}
	return nil
}

func importedIdentityConvIDs(exp *groupexport.Export) []string {
	seen := make(map[string]bool, len(exp.Convs)+2*len(exp.Successions))
	ids := make([]string, 0, len(exp.Convs)+2*len(exp.Successions))
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, conv := range exp.Convs {
		add(conv.ConvID)
	}
	for _, succession := range exp.Successions {
		add(succession.OldConvID)
		add(succession.NewConvID)
	}
	return ids
}

func importConversationPath(projectDir, convID string) (string, error) {
	dst := filepath.Join(projectDir, convID+".jsonl")
	rel, err := filepath.Rel(projectDir, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("conversation id %q escapes the target project directory", convID)
	}
	return dst, nil
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
	row, err := db.GetConvIndex(convID)
	if err == nil && row != nil && row.Harness != "" && row.Harness != harness.DefaultName {
		// Another harness's transcript (a Codex rollout is .jsonl too) is
		// not a Claude Code conversation.
		return "", false
	}
	if err == nil && row != nil && row.FullPath != "" {
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

// moveFile copies src into a newly-created destination and never replaces an
// existing path. O_EXCL is the filesystem-side collision guard for the window
// after import preflight.
func moveFile(src, dst string) error {
	return moveFileWithRemove(src, dst, os.Remove)
}

func moveFileWithRemove(src, dst string, remove func(string) error) error {
	in, err := os.Open(src) //nolint:gosec // src is a daemon-staged file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is derived from a server-minted UUID
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := remove(src); err != nil {
		return err
	}
	ok = true
	return nil
}

func reserveFile(dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is a validated imported conv id
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
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
