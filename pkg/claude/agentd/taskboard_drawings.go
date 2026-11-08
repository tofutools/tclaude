package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/drawingboard"
)

// A drawing on a task (tk_413bdf5d): a drawing-board export rendered and
// attached in one call, as a PNG the task's viewer previews, with a
// reference back to the board so the viewer can open it there.
//
//	POST /v1/taskboard/tasks/{id}/drawings
//	{board, elements?, region?, scale?, name?, comment?, replace?}
//
// It needs read access to the board and the task's own attach rules (or,
// with a comment, its commenting rules). Exporting the same part of the same
// board to the task again updates that file in place rather than adding
// another; replace names the file to update explicitly. A comment posts the
// drawing as a new comment's file.

const taskboardDrawingBodyLimit = drawingBoardExportBodyLimit + 64<<10

// taskboardDrawingRef is a drawing file's reference back to its board, as
// stored (db GroupTaskAttachment.Drawing) and on the wire.
type taskboardDrawingRef struct {
	Board   string `json:"board"`
	Version int64  `json:"version"`
	// Elements is the ids the export picked, when there are at most
	// taskboardDrawingRefMaxElements of them; a bigger pick keeps only its
	// count and digest, since the reference rides on every board view.
	Elements     []string `json:"elements,omitempty"`
	ElementCount int      `json:"element_count,omitempty"`
	// Selection is a digest of the picked ids, sorted: what tells two
	// exports of the same part apart from others.
	Selection string            `json:"selection,omitempty"`
	Region    *drawingboard.Box `json:"region,omitempty"`
	// Crop is the part of the board the image shows, for opening the board
	// there.
	Crop drawingboard.Box `json:"crop"`
}

// taskboardDrawingRefMaxElements bounds the ids a stored reference lists.
const taskboardDrawingRefMaxElements = 200

// selectionDigest is a digest of a pick of element ids, in any order.
func selectionDigest(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:16])
}

// newTaskboardDrawingRef is the reference stored for an export of elements
// (or region) of board at version, showing crop.
func newTaskboardDrawingRef(board string, version int64, elements []string, region *drawingboard.Box, crop drawingboard.Box) taskboardDrawingRef {
	d := taskboardDrawingRef{Board: board, Version: version, Region: region, Crop: crop,
		ElementCount: len(elements), Selection: selectionDigest(elements)}
	if len(elements) <= taskboardDrawingRefMaxElements {
		d.Elements = elements
	}
	return d
}

// sameSelection says two references export the same part of the same board.
func (d taskboardDrawingRef) sameSelection(o taskboardDrawingRef) bool {
	if d.Board != o.Board || d.Selection != o.Selection {
		return false
	}
	switch {
	case d.Region == nil || o.Region == nil:
		return d.Region == nil && o.Region == nil
	default:
		return *d.Region == *o.Region
	}
}

func drawingRefOf(a db.GroupTaskAttachment) (taskboardDrawingRef, bool) {
	var d taskboardDrawingRef
	if a.Drawing == "" || json.Unmarshal([]byte(a.Drawing), &d) != nil || d.Board == "" {
		return taskboardDrawingRef{}, false
	}
	return d, true
}

type taskboardDrawingRequest struct {
	Board    string            `json:"board"`
	Elements []string          `json:"elements"`
	Region   *drawingboard.Box `json:"region"`
	Scale    float64           `json:"scale"`
	Name     string            `json:"name"`
	Comment  string            `json:"comment"`
	Replace  int64             `json:"replace"`
}

// drawingFileName is the file's name: the given one (a .png, or none), or
// the board's id.
func drawingFileName(name, board string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = board
	}
	switch ext := strings.ToLower(filepath.Ext(name)); ext {
	case ".png":
	case "":
		name += ".png"
	default:
		return "", fmt.Errorf("a drawing is attached as a .png, not %s", ext)
	}
	clean, _, err := fedAttachmentName(name)
	return clean, err
}

func registerTaskboardDrawingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/drawings", handleTaskboardTask(taskboardOwnGate, handleTaskboardDrawing))
}

func handleTaskboardDrawing(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var req taskboardDrawingRequest
	if !decodeTaskboardBodyLimit(w, r, &req, taskboardDrawingBodyLimit) {
		return
	}
	var comment string
	if strings.TrimSpace(req.Comment) != "" {
		if req.Replace != 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "a comment posts a new file; replace updates a task's file")
			return
		}
		text, err := validateTaskComment(req.Comment)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		comment = text
		if !requireTaskboardCommenter(w, r, g) {
			return
		}
	} else if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	if !drawingBoardIDPattern.MatchString(req.Board) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "board must be a drawing board id")
		return
	}
	name, err := drawingFileName(req.Name, req.Board)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if _, ok := requireDrawingBoardAccess(w, r, req.Board, false); !ok {
		return
	}
	img, version, ok := renderDrawingBoardExport(w, r, req.Board, drawingBoardExportRequest{
		Format: drawingboard.FormatPNG, Elements: req.Elements, Region: req.Region, Scale: req.Scale,
	})
	if !ok {
		return
	}
	if len(img.Data) > taskAttachmentMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("the drawing is larger than %d MiB as a PNG; export a part of it, or at a smaller --scale", taskAttachmentMaxBytes>>20))
		return
	}
	ref := newTaskboardDrawingRef(req.Board, version, req.Elements, req.Region, img.Crop)
	refJSON, _ := json.Marshal(ref)
	actor := taskboardActor(r)
	if comment != "" {
		actor = taskboardCommentAuthor(r)
	}
	sha, err := storeTaskBlob(img.Data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "store attachment: "+err.Error())
		return
	}
	row := db.GroupTaskAttachment{Name: name, SHA256: sha, SizeBytes: int64(len(img.Data)), ContentType: img.ContentType,
		AddedBy: actor, Drawing: string(refJSON)}

	if comment != "" {
		c, _, err := db.AddGroupTaskComment(db.NewGroupTaskComment{TaskID: t.ID, Actor: actor, Body: comment, Attachments: []db.GroupTaskAttachment{row}})
		if err != nil {
			writeTaskboardCommentError(w, err)
			return
		}
		files, err := db.ListGroupTaskCommentAttachments(t.ID)
		if err != nil || len(files[c.ID]) == 0 {
			writeError(w, http.StatusInternalServerError, "db", "read the comment's file back")
			return
		}
		setAuditDetail(r, fmt.Sprintf("commented on %s (#%d) with drawing board %s", t.ID, c.ID, req.Board))
		mailTaskboardComment(r, g, t, c)
		writeJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "group": g.Name, "comment": c.ID,
			"attachment": taskboardAttachmentViews(files[c.ID][:1])[0]})
		return
	}

	target := req.Replace
	if target != 0 {
		a, err := db.GetGroupTaskAttachment(t.ID, target)
		if errors.Is(err, db.ErrGroupTaskAttachmentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("task %s has no attachment %d", t.ID, target))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		if _, ok := drawingRefOf(*a); !ok || a.CommentID != 0 {
			writeError(w, http.StatusConflict, "not_a_drawing", fmt.Sprintf("attachment %d is not a drawing on the task itself", target))
			return
		}
	} else {
		own, err := db.ListGroupTaskAttachments(t.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		for _, a := range own {
			if d, ok := drawingRefOf(a); ok && d.sameSelection(ref) {
				target = a.ID
				break
			}
		}
	}
	if target != 0 {
		_, updated, err := db.ReplaceGroupTaskAttachmentFile(t.ID, target, row, actor)
		if err != nil {
			writeTaskboardError(w, err)
			return
		}
		setAuditDetail(r, fmt.Sprintf("updated drawing %s on %s from drawing board %s", updated.Name, t.ID, req.Board))
		writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "group": g.Name, "replaced": true,
			"attachment": taskboardAttachmentViews([]db.GroupTaskAttachment{*updated})[0]})
		return
	}
	_, added, err := db.AddGroupTaskAttachments(t.ID, []db.GroupTaskAttachment{row}, actor)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("attached drawing %s to %s from drawing board %s", added[0].Name, t.ID, req.Board))
	writeJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "group": g.Name,
		"attachment": taskboardAttachmentViews(added)[0]})
}
