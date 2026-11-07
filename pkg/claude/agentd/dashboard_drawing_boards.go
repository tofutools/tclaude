package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Collab drawing boards (tcl-4f86kp): a shared drawing board for the operator and
// agents. The operator draws in the dashboard drawing board editor (/api/drawing-boards);
// agents read and write the same drawing board through `tclaude agent drawing-board`
// (/v1/drawing-boards, drawing-boards.read / drawing-boards.write). Elements are a bounded subset of
// Excalidraw's element JSON, so a drawing board exports as `.excalidraw`.
//
//   - Unknown element keys are dropped; the rest are validated and bounded.
//   - Each element merges independently with Excalidraw's reconciliation
//     rule (higher version wins, a version tie goes to the lower nonce).
//     Deletion is a tombstone (isDeleted) that stays so it can sync.
//   - customData.author is always set by the server: "operator" on /api, the
//     calling agent's id on /v1.
//   - The latest PNG snapshot (rendered by the dashboard) is a file under
//     config.DataDir()/drawing-boards, private daemon state, stored with the drawing board
//     version it shows.
//   - Operator drawing board activity (POST /api/drawing-boards/{id}/events) becomes operator
//     mail to each online participant, at most once per drawing board per 3s.

const (
	PermDrawingBoardsRead  = "drawing-boards.read"
	PermDrawingBoardsWrite = "drawing-boards.write"
)

const (
	drawingBoardAuthorOperator   = "operator"
	drawingBoardMaxLiveElements  = 5000
	drawingBoardMaxStoredRows    = 20000 // live elements plus tombstones
	drawingBoardMaxDrawingBoards = 500
	drawingBoardMaxVersion       = 1<<53 - 1 // stays exact in JavaScript numbers
	drawingBoardMaxRequestBody   = 4 << 20
	// A dashboard edit also carries what its undo steps put back. Keep in
	// step with DRAWING_BOARD_MAX_STEPS_BODY in drawing-board-island.js.
	drawingBoardMaxRequestBodyWithSteps = 12 << 20
	drawingBoardMaxSnapshotBytes        = 8 << 20
	drawingBoardMaxCoordinate           = 1e6
	drawingBoardMaxStack                = 1e15
	drawingBoardMaxPoints               = 5000
	drawingBoardMaxTextBytes            = 4000
	drawingBoardMaxTitleBytes           = 200
	drawingBoardMaxEventBytes           = 2000
	drawingBoardEventInterval           = 3 * time.Second
)

var (
	drawingBoardIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	elementIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	drawingBoardColor     = regexp.MustCompile(`^(#[0-9A-Fa-f]{6}|#[0-9A-Fa-f]{3}|transparent)$`)
	pngMagic              = []byte("\x89PNG\r\n\x1a\n")
)

var (
	drawingBoardElementTypes = map[string]bool{"rectangle": true, "ellipse": true, "diamond": true, "arrow": true, "line": true, "freedraw": true, "text": true}
	drawingBoardLinearTypes  = map[string]bool{"arrow": true, "line": true, "freedraw": true}
	drawingBoardFillStyles   = map[string]bool{"solid": true, "hachure": true, "none": true}
	drawingBoardStrokeStyles = map[string]bool{"solid": true, "dashed": true, "dotted": true}
	drawingBoardTextAligns   = map[string]bool{"left": true, "center": true, "right": true}
	drawingBoardRoutings     = map[string]bool{"straight": true, "elbow": true, "curved": true}
)

// drawingBoardElement is the stored Excalidraw subset. Decoding into it drops every
// key it does not name; pointers keep absent optional fields absent.
type drawingBoardElement struct {
	ID              string                   `json:"id"`
	Type            string                   `json:"type,omitempty"`
	X               *float64                 `json:"x,omitempty"`
	Y               *float64                 `json:"y,omitempty"`
	Width           *float64                 `json:"width,omitempty"`
	Height          *float64                 `json:"height,omitempty"`
	Angle           *float64                 `json:"angle,omitempty"`
	StrokeColor     string                   `json:"strokeColor,omitempty"`
	BackgroundColor string                   `json:"backgroundColor,omitempty"`
	FillStyle       string                   `json:"fillStyle,omitempty"`
	StrokeWidth     *float64                 `json:"strokeWidth,omitempty"`
	StrokeStyle     string                   `json:"strokeStyle,omitempty"`
	Roughness       *float64                 `json:"roughness,omitempty"`
	Opacity         *float64                 `json:"opacity,omitempty"`
	Points          [][]float64              `json:"points,omitempty"`
	Text            *string                  `json:"text,omitempty"`
	FontSize        *float64                 `json:"fontSize,omitempty"`
	FontFamily      any                      `json:"fontFamily,omitempty"` // Excalidraw's: 1 hand, 2 sans, 3 mono
	FontWeight      any                      `json:"fontWeight,omitempty"` // bold
	FontStyle       any                      `json:"fontStyle,omitempty"`  // italic
	TextAlign       string                   `json:"textAlign,omitempty"`
	ContainerID     string                   `json:"containerId,omitempty"`
	StartBinding    *drawingBoardBinding     `json:"startBinding,omitempty"`
	EndBinding      *drawingBoardBinding     `json:"endBinding,omitempty"`
	Routing         string                   `json:"routing,omitempty"`
	Z               *float64                 `json:"z,omitempty"` // its place in the stack, see stackOrder in drawing-board-model.js
	Version         int64                    `json:"version"`
	VersionNonce    int64                    `json:"versionNonce"`
	IsDeleted       bool                     `json:"isDeleted"`
	Updated         int64                    `json:"updated,omitempty"`
	CustomData      *drawingBoardElementMeta `json:"customData,omitempty"`
}

// drawingBoardBinding attaches one end of a line to an element. FixedPoint is the
// snap point as Excalidraw stores it, [x, y] fractions of the element's box;
// without one the end meets the element wherever the line reaches it.
type drawingBoardBinding struct {
	ElementID  string    `json:"elementId"`
	FixedPoint []float64 `json:"fixedPoint,omitempty"`
}

type drawingBoardElementMeta struct {
	Author string `json:"author"`
}

// normalizeDrawingBoardElement decodes one element, drops unknown keys and checks
// every bound. author replaces whatever customData the client sent.
func normalizeDrawingBoardElement(raw json.RawMessage, author string) (db.CollabDrawingBoardElement, error) {
	var e drawingBoardElement
	if err := json.Unmarshal(raw, &e); err != nil {
		return db.CollabDrawingBoardElement{}, fmt.Errorf("invalid element: %v", err)
	}
	if !elementIDPattern.MatchString(e.ID) {
		return db.CollabDrawingBoardElement{}, errors.New("element id must be 1-64 characters of [A-Za-z0-9_-]")
	}
	fail := func(format string, args ...any) (db.CollabDrawingBoardElement, error) {
		return db.CollabDrawingBoardElement{}, fmt.Errorf("element %s: "+format, append([]any{e.ID}, args...)...)
	}
	if e.Version < 1 || e.Version > drawingBoardMaxVersion {
		return fail("version must be an integer from 1 to 2^53-1")
	}
	if e.VersionNonce < math.MinInt32 || e.VersionNonce > math.MaxInt32 {
		return fail("versionNonce must be a 32-bit integer")
	}
	if e.Type == "" && !e.IsDeleted {
		return fail("type is required")
	}
	if e.Type != "" && !drawingBoardElementTypes[e.Type] {
		return fail("type must be one of rectangle, ellipse, diamond, arrow, line, freedraw, text")
	}
	for name, v := range map[string]*float64{"x": e.X, "y": e.Y, "width": e.Width, "height": e.Height, "angle": e.Angle} {
		if v != nil && !drawingBoardFinite(*v, drawingBoardMaxCoordinate) {
			return fail("%s must be a finite number with |v| <= 1e6", name)
		}
	}
	for name, c := range map[string]string{"strokeColor": e.StrokeColor, "backgroundColor": e.BackgroundColor} {
		if c != "" && (len(c) > 32 || !drawingBoardColor.MatchString(c)) {
			return fail("%s must be #rrggbb, #rgb or transparent", name)
		}
	}
	if e.FillStyle != "" && !drawingBoardFillStyles[e.FillStyle] {
		return fail("fillStyle must be solid, hachure or none")
	}
	if e.StrokeStyle != "" && !drawingBoardStrokeStyles[e.StrokeStyle] {
		return fail("strokeStyle must be solid, dashed or dotted")
	}
	if e.TextAlign != "" && !drawingBoardTextAligns[e.TextAlign] {
		return fail("textAlign must be left, center or right")
	}
	if !drawingBoardInRange(e.StrokeWidth, 0.5, 20) {
		return fail("strokeWidth must be 0.5-20")
	}
	if !drawingBoardInRange(e.Roughness, 0, 2) {
		return fail("roughness must be 0-2")
	}
	if !drawingBoardInRange(e.Opacity, 0, 100) {
		return fail("opacity must be 0-100")
	}
	if !drawingBoardInRange(e.FontSize, 4, 200) {
		return fail("fontSize must be 4-200")
	}
	// What this drawing board cannot draw goes the way of any key it does not know,
	// whatever its type: a newer Excalidraw font number or a numeric weight must
	// not refuse a whole pasted drawing. Hence `any` on these three.
	if f, ok := e.FontFamily.(float64); !ok || (f != 1 && f != 2 && f != 3) {
		e.FontFamily = nil
	}
	if e.FontWeight != "bold" {
		e.FontWeight = nil
	}
	if e.FontStyle != "italic" {
		e.FontStyle = nil
	}
	if !drawingBoardLinearTypes[e.Type] {
		e.Points = nil
	}
	if len(e.Points) > drawingBoardMaxPoints {
		return fail("at most %d points", drawingBoardMaxPoints)
	}
	for _, p := range e.Points {
		if len(p) != 2 || !drawingBoardFinite(p[0], drawingBoardMaxCoordinate) || !drawingBoardFinite(p[1], drawingBoardMaxCoordinate) {
			return fail("points must be [x, y] pairs of finite numbers with |v| <= 1e6")
		}
	}
	if drawingBoardLinearTypes[e.Type] && !e.IsDeleted && len(e.Points) < 2 {
		return fail("%s needs at least 2 points", e.Type)
	}
	if e.Text != nil {
		if len(*e.Text) > drawingBoardMaxTextBytes {
			return fail("text is longer than %d bytes", drawingBoardMaxTextBytes)
		}
		if !drawingBoardPrintable(*e.Text, true) {
			return fail("text must be printable (newlines allowed)")
		}
	}
	if !drawingBoardLinearTypes[e.Type] {
		e.StartBinding, e.EndBinding = nil, nil
	}
	for _, b := range []*drawingBoardBinding{e.StartBinding, e.EndBinding} {
		if b == nil {
			continue
		}
		if !elementIDPattern.MatchString(b.ElementID) {
			return fail("binding elementId must be 1-64 characters of [A-Za-z0-9_-]")
		}
		if b.FixedPoint == nil {
			continue
		}
		if len(b.FixedPoint) != 2 || !drawingBoardFinite(b.FixedPoint[0], drawingBoardMaxCoordinate) || !drawingBoardFinite(b.FixedPoint[1], drawingBoardMaxCoordinate) {
			return fail("binding fixedPoint must be [x, y] fractions")
		}
		// Excalidraw can store a point just off the box; the editor clamps too.
		for i, v := range b.FixedPoint {
			b.FixedPoint[i] = min(1, max(0, v))
		}
	}
	if e.Type != "arrow" && e.Type != "line" {
		e.Routing = ""
	}
	if e.Routing != "" && !drawingBoardRoutings[e.Routing] {
		return fail("routing must be straight, elbow or curved")
	}
	if e.Type != "text" {
		e.ContainerID = ""
	}
	if e.Z != nil && !drawingBoardFinite(*e.Z, drawingBoardMaxStack) {
		return fail("z must be a finite number")
	}
	if e.ContainerID != "" && !elementIDPattern.MatchString(e.ContainerID) {
		return fail("containerId must be 1-64 characters of [A-Za-z0-9_-]")
	}
	if e.Updated < 0 || e.Updated > drawingBoardMaxVersion {
		return fail("updated must be a millisecond epoch")
	}
	if e.IsDeleted {
		// A tombstone only needs what the merge and sync read.
		e = drawingBoardElement{ID: e.ID, Type: e.Type, Version: e.Version, VersionNonce: e.VersionNonce, IsDeleted: true, Updated: e.Updated}
	}
	e.CustomData = &drawingBoardElementMeta{Author: author}
	body, err := json.Marshal(e)
	if err != nil {
		return db.CollabDrawingBoardElement{}, err
	}
	return db.CollabDrawingBoardElement{ID: e.ID, Version: e.Version, Nonce: e.VersionNonce, Deleted: e.IsDeleted, Body: body}, nil
}

func drawingBoardFinite(v, bound float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= bound
}

func drawingBoardInRange(v *float64, lo, hi float64) bool {
	return v == nil || (!math.IsNaN(*v) && *v >= lo && *v <= hi)
}

// drawingBoardPrintable accepts printable text; newlines (and tabs) only when
// multiline.
func drawingBoardPrintable(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// drawingBoardIDFromTitle derives a drawing board id from a title: lowercase, runs of other
// characters become one dash, clipped to the id length.
func drawingBoardIDFromTitle(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := b.String()
	if len(id) > 64 {
		id = id[:64]
	}
	id = strings.Trim(id, "-")
	if id == "" {
		return "drawing_board"
	}
	return id
}

// ---- JSON shapes ----

type drawingBoardSummaryJSON struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Version      int64     `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
	ElementCount int       `json:"element_count"`
	Participants []string  `json:"participants"`
	Group        string    `json:"group,omitempty"`
	Agents       int       `json:"agents"`           // on it, invited or through its group
	Active       bool      `json:"active,omitempty"` // an agent holds a session on it
	AgentsLocked bool      `json:"agents_locked,omitempty"`
}

type drawingBoardJSON struct {
	ID              string                    `json:"id"`
	Title           string                    `json:"title"`
	Version         int64                     `json:"version"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	UpdatedBy       string                    `json:"updated_by"`
	Participants    []string                  `json:"participants"`
	SnapshotVersion *int64                    `json:"snapshot_version,omitempty"`
	Elements        []json.RawMessage         `json:"elements"`
	Presence        *drawingBoardPresenceJSON `json:"presence,omitempty"`
	// Group is the group the drawing board belongs to; its active members are on the
	// drawing board (GroupMembers) without an invitation (tcl-4yhow7).
	Group        string   `json:"group,omitempty"`
	GroupMembers []string `json:"group_members,omitempty"`
	// AgentsLocked is the lock on the drawing board against agent changes
	// (group_resource_locks.go).
	AgentsLocked bool `json:"agents_locked,omitempty"`
}

// drawingBoardElementJSON stamps the stored body with its server_version.
func drawingBoardElementJSON(e db.CollabDrawingBoardElement) json.RawMessage {
	body := bytes.TrimSpace(e.Body)
	if len(body) < 2 || body[len(body)-1] != '}' {
		return e.Body
	}
	out := make([]byte, 0, len(body)+32)
	out = append(out, body[:len(body)-1]...)
	out = append(out, `,"server_version":`...)
	out = strconv.AppendInt(out, e.ServerVersion, 10)
	return append(out, '}')
}

func drawingBoardToJSON(b *db.CollabDrawingBoard, elems []db.CollabDrawingBoardElement) drawingBoardJSON {
	out := drawingBoardJSON{
		ID: b.ID, Title: b.Title, Version: b.Version, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		UpdatedBy: b.UpdatedBy, Participants: b.Participants, Elements: make([]json.RawMessage, 0, len(elems)),
		Group: b.GroupName, AgentsLocked: db.GroupResourceLocked(db.GroupResourceDrawingBoard, b.ID),
	}
	if members, err := db.CollabDrawingBoardGroupMembers(b); err == nil {
		out.GroupMembers = members
	}
	if b.HasSnapshot {
		v := b.SnapshotVersion
		out.SnapshotVersion = &v
	}
	for _, e := range elems {
		out.Elements = append(out.Elements, drawingBoardElementJSON(e))
	}
	return out
}

func writeDrawingBoardStoreError(w http.ResponseWriter, err error) {
	var tooMany *db.CollabDrawingBoardTooManyElementsError
	switch {
	case errors.Is(err, db.ErrCollabDrawingBoardNotFound):
		writeError(w, http.StatusNotFound, "not_found", "drawing board not found")
	case errors.Is(err, db.ErrCollabDrawingBoardExists):
		writeError(w, http.StatusConflict, "exists", "a drawing board with that id already exists")
	case errors.Is(err, db.ErrCollabDrawingBoardLimit):
		writeError(w, http.StatusConflict, "too_many_drawing_boards", fmt.Sprintf("at most %d drawing boards; delete one first", drawingBoardMaxDrawingBoards))
	case errors.As(err, &tooMany):
		writeError(w, http.StatusBadRequest, "too_many_elements", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	}
}

// drawingBoardPathID reads and checks the {id} path value.
func drawingBoardPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !drawingBoardIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "drawing board not found")
		return "", false
	}
	return id, true
}

// ---- shared handlers (both surfaces) ----

// serveDrawingBoardList lists the drawing boards; keep, when set, picks which.
func serveDrawingBoardList(w http.ResponseWriter, keep func(*db.CollabDrawingBoard) bool) {
	drawingBoards, err := db.ListCollabDrawingBoards()
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	sessions, err := db.ListCollabDrawingBoardSessions()
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	locked, err := db.GroupResourceLocks(db.GroupResourceDrawingBoard)
	if err != nil {
		locked = map[string]bool{}
	}
	active := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		active[s.DrawingBoardID] = true
	}
	out := make([]drawingBoardSummaryJSON, 0, len(drawingBoards))
	for _, b := range drawingBoards {
		if keep != nil && !keep(b) {
			continue
		}
		out = append(out, drawingBoardSummaryJSON{
			ID: b.ID, Title: b.Title, Version: b.Version, UpdatedAt: b.UpdatedAt,
			ElementCount: b.ElementCount, Participants: b.Participants, Group: b.GroupName,
			Agents: len(drawingBoardAudience(b)), Active: active[b.ID], AgentsLocked: locked[b.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"drawingBoards": out})
}

// drawingBoardAudience is everyone on the drawing board: its group's members are on it as well
// as those invited.
func drawingBoardAudience(b *db.CollabDrawingBoard) []string {
	audience := slices.Clone(b.Participants)
	if members, err := db.CollabDrawingBoardGroupMembers(b); err == nil {
		for _, m := range members {
			if !slices.Contains(audience, m) {
				audience = append(audience, m)
			}
		}
	}
	return audience
}

// resolveDrawingBoardGroup turns a group name into its id for linking a drawing board; ""
// is no group. An agent (agentID set) may only link a drawing board to a group it is
// in: the link mails the drawing board's activity to every member.
func resolveDrawingBoardGroup(w http.ResponseWriter, name, agentID string) (int64, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, true
	}
	g, err := db.GetAgentGroupByName(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return 0, false
	}
	if g == nil || g.IsArchived() {
		writeError(w, http.StatusNotFound, "not_found", "no active group "+guideText(name, 80))
		return 0, false
	}
	if agentID != "" {
		m, err := db.FindAgentMemberInGroup(g.ID, agentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return 0, false
		}
		if m == nil {
			writeError(w, http.StatusForbidden, "not_member", "you can only link a drawing board to a group you are a member of")
			return 0, false
		}
	}
	return g.ID, true
}

func serveDrawingBoardCreate(w http.ResponseWriter, r *http.Request, by, agentID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid drawing board: "+err.Error())
		return
	}
	b, ok := createDrawingBoard(w, req.ID, req.Title, req.Group, by, agentID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, drawingBoardToJSON(b, nil))
}

// createDrawingBoard makes a drawing board (id from title when blank) linked to group,
// writing the refusal to w when it cannot.
func createDrawingBoard(w http.ResponseWriter, rawID, rawTitle, group, by, agentID string) (*db.CollabDrawingBoard, bool) {
	id, title := strings.TrimSpace(rawID), strings.TrimSpace(rawTitle)
	if title == "" {
		title = id
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a drawing board needs a title or an id")
		return nil, false
	}
	if len(title) > drawingBoardMaxTitleBytes || !drawingBoardPrintable(title, false) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("title must be printable text of at most %d bytes", drawingBoardMaxTitleBytes))
		return nil, false
	}
	if id == "" {
		id = drawingBoardIDFromTitle(title)
	}
	if !drawingBoardIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "drawing board id must match [a-z0-9][a-z0-9-]{0,63}")
		return nil, false
	}
	groupID, ok := resolveDrawingBoardGroup(w, group, agentID)
	if !ok {
		return nil, false
	}
	b, err := db.CreateCollabDrawingBoard(id, title, by, drawingBoardMaxDrawingBoards)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return nil, false
	}
	if groupID > 0 {
		if err := db.SetCollabDrawingBoardGroup(id, groupID); err != nil {
			// Leave no half-made drawing board for the retry to collide with.
			if delErr := deleteDrawingBoardAndSnapshot(id); delErr != nil {
				slog.Warn("drawing boards: remove drawing board after failed group link", "drawing_board", id, "error", delErr)
			}
			writeDrawingBoardStoreError(w, err)
			return nil, false
		}
		if b, err = db.GetCollabDrawingBoard(id); err != nil {
			writeDrawingBoardStoreError(w, err)
			return nil, false
		}
	}
	return b, true
}

// handleDashboardDrawingBoardCopy copies a drawing board: a new drawing board, linked to group,
// holding the source's live elements. The Groups tab's Ctrl/Cmd-drag of a
// drawing board row onto another group's box makes one.
//
//	POST /api/drawing-boards/{id}/copy {id, title, group}
func handleDashboardDrawingBoardCopy(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	srcID, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid drawing board copy: "+err.Error())
		return
	}
	src, err := db.GetCollabDrawingBoard(srcID)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	elems, err := db.ListCollabDrawingBoardElements(srcID, -1)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	title := req.Title
	if strings.TrimSpace(title) == "" {
		title = src.Title + " (copy)"
	}
	b, ok := createDrawingBoard(w, req.ID, title, req.Group, drawingBoardAuthorOperator, "")
	if !ok {
		return
	}
	if len(elems) > 0 {
		if _, _, err := db.ApplyCollabDrawingBoardEdit(b.ID, drawingBoardAuthorOperator, "", elems, nil, drawingBoardMaxLiveElements, drawingBoardMaxStoredRows); err != nil {
			if delErr := deleteDrawingBoardAndSnapshot(b.ID); delErr != nil {
				slog.Warn("drawing boards: remove drawing board after failed copy", "drawing_board", b.ID, "error", delErr)
			}
			writeDrawingBoardStoreError(w, err)
			return
		}
		if b, err = db.GetCollabDrawingBoard(b.ID); err != nil {
			writeDrawingBoardStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, drawingBoardToJSON(b, nil))
}

// serveDrawingBoardRead returns the drawing board. Without ?since it carries the live
// elements; with ?since=N every element (tombstones included) whose
// server_version is above N.
func serveDrawingBoardRead(w http.ResponseWriter, r *http.Request, id string) {
	since := int64(-1)
	if s := r.URL.Query().Get("since"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since must be a drawing board version >= 0")
			return
		}
		since = n
	}
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	elems, err := db.ListCollabDrawingBoardElements(id, since)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	out := drawingBoardToJSON(b, elems)
	out.Presence = drawingBoardPresence.snapshot(id)
	writeJSON(w, http.StatusOK, out)
}

// serveDrawingBoardPatch merges elements authored by author; participant (an agent
// id, or empty) joins the drawing board in the same transaction.
//
// historyActor, when set, takes the request's undo steps into that actor's
// history. A step's before values are stored as sent and only normalized when
// an undo uses them, so a bad step costs that undo and never the edit.
func serveDrawingBoardPatch(w http.ResponseWriter, r *http.Request, id, author, participant, historyActor string) {
	limit := int64(drawingBoardMaxRequestBody)
	if historyActor != "" {
		limit = drawingBoardMaxRequestBodyWithSteps
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var req struct {
		Elements []json.RawMessage                    `json:"elements"`
		Steps    [][]db.CollabDrawingBoardStepElement `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body is larger than %d MiB", limit>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid patch: "+err.Error())
		return
	}
	elems := make([]db.CollabDrawingBoardElement, 0, len(req.Elements))
	for i, raw := range req.Elements {
		e, err := normalizeDrawingBoardElement(raw, author)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_element", fmt.Sprintf("elements[%d]: %v", i, err))
			return
		}
		elems = append(elems, e)
	}
	var history *db.CollabDrawingBoardHistory
	if historyActor != "" {
		history = &db.CollabDrawingBoardHistory{Actor: historyActor, Steps: drawingBoardStepsWithAfter(validDrawingBoardSteps(req.Steps), elems)}
	}
	version, accepted, err := db.ApplyCollabDrawingBoardEdit(id, author, participant, elems, history, drawingBoardMaxLiveElements, drawingBoardMaxStoredRows)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	if accepted > 0 {
		drawingBoardPresence.noteEdit(id, participant)
	}
	if accepted > 0 || participant != "" {
		drawingBoardLive.publish(id, drawingBoardClient(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "accepted": accepted})
}

func drawingBoardSnapshotPath(id string) string {
	return filepath.Join(config.DataDir(), "drawing-boards", id+".png")
}

func serveDrawingBoardSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	if region := r.URL.Query().Get("region"); region != "" {
		serveDrawingBoardRegion(w, id, region)
		return
	}
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	if !b.HasSnapshot {
		writeError(w, http.StatusNotFound, "no_snapshot", "the drawing board has no snapshot yet")
		return
	}
	data, err := os.ReadFile(drawingBoardSnapshotPath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_snapshot", "the drawing board has no snapshot yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read snapshot: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Drawing-board-Snapshot-Version", strconv.FormatInt(b.SnapshotVersion, 10))
	w.Header().Set("X-Drawing-board-Version", strconv.FormatInt(b.Version, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- dashboard surface (/api/drawing-boards) ----

func registerDashboardDrawingBoardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/drawing-boards", handleDashboardDrawingBoardList)
	mux.HandleFunc("POST /api/pages", handleDashboardPageCreate)
	mux.HandleFunc("DELETE /api/pages/{id}", handleDashboardPageClose)
	mux.HandleFunc("POST /api/pages/{id}/reopen", handleDashboardPageReopen)
	mux.HandleFunc("POST /api/pages/{id}/delete", handleDashboardPageDelete)
	mux.HandleFunc("POST /api/pages/{id}/reload", handleDashboardPageReload)
	mux.HandleFunc("POST /api/pages/{id}/title", handleDashboardPageTitle)
	mux.HandleFunc("POST /api/pages/{id}/group", handleDashboardPageGroup)
	mux.HandleFunc("GET /api/pages/{id}/live", handleDashboardPageLive)
	mux.HandleFunc("POST /api/pages/{id}/marks", handleDashboardPageMarks)
	mux.HandleFunc("POST /api/drawing-boards", handleDashboardDrawingBoardCreate)
	mux.HandleFunc("GET /api/drawing-boards/{id}", handleDashboardDrawingBoardRead)
	mux.HandleFunc("PATCH /api/drawing-boards/{id}", handleDashboardDrawingBoardPatch)
	mux.HandleFunc("DELETE /api/drawing-boards/{id}", handleDashboardDrawingBoardDelete)
	mux.HandleFunc("PUT /api/drawing-boards/{id}/snapshot", handleDashboardDrawingBoardSnapshotPut)
	mux.HandleFunc("GET /api/drawing-boards/{id}/snapshot", handleDashboardDrawingBoardSnapshotGet)
	mux.HandleFunc("POST /api/drawing-boards/{id}/participants", handleDashboardDrawingBoardParticipants)
	mux.HandleFunc("POST /api/drawing-boards/{id}/events", handleDashboardDrawingBoardEvent)
	mux.HandleFunc("POST /api/drawing-boards/{id}/view", handleDashboardDrawingBoardView)
	mux.HandleFunc("POST /api/drawing-boards/{id}/group", handleDashboardDrawingBoardGroup)
	mux.HandleFunc("POST /api/drawing-boards/{id}/copy", handleDashboardDrawingBoardCopy)
	mux.HandleFunc("POST /api/drawing-boards/{id}/title", handleDashboardDrawingBoardTitle)
	mux.HandleFunc("POST /api/resource-locks", handleDashboardResourceLock)
	mux.HandleFunc("GET /api/drawing-boards/{id}/live", handleDashboardDrawingBoardLive)
	mux.HandleFunc("POST /api/drawing-boards/{id}/undo", handleDashboardDrawingBoardUndo)
	mux.HandleFunc("DELETE /api/drawing-boards/{id}/sessions", handleDashboardDrawingBoardSessionsEnd)
}

// DELETE /api/drawing-boards/{id}/sessions — the operator ends every agent's session
// on the drawing board, its row in the Background terminals menu. The drawing board stays.
func handleDashboardDrawingBoardSessionsEnd(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if _, err := db.GetCollabDrawingBoard(id); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	ended, err := db.EndCollabDrawingBoardSessions(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	setAuditTargetLabel(r, "drawing board "+id)
	writeJSON(w, http.StatusOK, map[string]any{"ended": ended})
}

// POST /api/drawing-boards/{id}/group {group} — link the drawing board to a group ("" unlinks).
func handleDashboardDrawingBoardGroup(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid group: "+err.Error())
		return
	}
	groupID, ok := resolveDrawingBoardGroup(w, req.Group, "")
	if !ok {
		return
	}
	if err := db.SetCollabDrawingBoardGroup(id, groupID); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	drawingBoardLive.publish(id, "")
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	setAuditTargetLabel(r, "drawing board "+b.ID)
	out := drawingBoardToJSON(b, nil)
	writeJSON(w, http.StatusOK, map[string]any{"group": out.Group, "group_members": out.GroupMembers})
}

func handleDashboardDrawingBoardList(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveDrawingBoardList(w, nil)
}

func handleDashboardDrawingBoardCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveDrawingBoardCreate(w, r, drawingBoardAuthorOperator, "")
}

func handleDashboardDrawingBoardRead(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := drawingBoardPathID(w, r); ok {
		serveDrawingBoardRead(w, r, id)
	}
}

func handleDashboardDrawingBoardPatch(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := drawingBoardPathID(w, r); ok {
		serveDrawingBoardPatch(w, r, id, drawingBoardAuthorOperator, "", drawingBoardAuthorOperator)
	}
}

func handleDashboardDrawingBoardDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	// Who was on it can only be read before it goes.
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	audience := drawingBoardAudience(b)
	if err := deleteDrawingBoardAndSnapshot(id); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	drawingBoardEvents.forget(id)
	drawingBoardPresence.forget(id)
	drawingBoardLive.publish(id, "")
	tellDrawingBoardDeleted(id, audience)
	setAuditTargetLabel(r, "drawing board "+id)
	if err := removeDrawingBoardRegions(id); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "remove event images: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tellDrawingBoardDeleted mails everyone who was on a deleted drawing board. Unlike activity
// mail this goes to stopped agents too: the news is still true when they
// resume. A drawing board that is gone stays gone whoever could not be told, so a
// failure is only logged. Operator-authored, this quotes the id, which is held
// to [a-z0-9-], and not the title, which any agent with drawing-boards.write can set.
func tellDrawingBoardDeleted(id string, audience []string) {
	body := fmt.Sprintf("The operator deleted drawing board %s. Its contents are gone, and `tclaude agent drawing-board` commands for it now fail.", id)
	for _, agentID := range audience {
		a, err := db.GetAgent(agentID)
		if err != nil || !a.Active() || a.CurrentConvID == "" {
			continue
		}
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: "", ToConv: a.CurrentConvID, Subject: "[drawing-board " + id + "]", Body: body,
			ToRecipients: []string{a.CurrentConvID}, OperatorAuthored: true,
		}); err != nil {
			slog.Warn("drawing boards: tell agent of deleted drawing board", "drawing_board", id, "agent", agentID, "error", err)
		}
	}
}

// PUT /api/drawing-boards/{id}/snapshot?version=N — the dashboard's PNG rendering of
// drawing board version N.
func handleDashboardDrawingBoardSnapshotPut(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if err != nil || version < 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "version must be the drawing board version the snapshot shows")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, drawingBoardMaxSnapshotBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "snapshot is larger than 8 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_arg", "read snapshot: "+err.Error())
		return
	}
	if !bytes.HasPrefix(data, pngMagic) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "snapshot must be a PNG image")
		return
	}
	// The check, the file and its recorded version change together, so two
	// tabs saving at once cannot leave one's image under the other's version.
	drawingBoardSnapshotMu.Lock()
	defer drawingBoardSnapshotMu.Unlock()
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	if b.HasSnapshot && version < b.SnapshotVersion {
		writeError(w, http.StatusConflict, "stale_snapshot", fmt.Sprintf("a snapshot of version %d is already stored", b.SnapshotVersion))
		return
	}
	if version > b.Version {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("version %d is ahead of the drawing board (version %d)", version, b.Version))
		return
	}
	if err := writeDrawingBoardSnapshotFile(drawingBoardSnapshotPath(id), data); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "store snapshot: "+err.Error())
		return
	}
	if err := db.SetCollabDrawingBoardSnapshot(id, version); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "bytes": len(data)})
}

var drawingBoardSnapshotMu sync.Mutex

// deleteDrawingBoardAndSnapshot removes the drawing board and its PNG together, so a snapshot
// upload that found the drawing board cannot write its file after this removal.
func deleteDrawingBoardAndSnapshot(id string) error {
	drawingBoardSnapshotMu.Lock()
	defer drawingBoardSnapshotMu.Unlock()
	if err := db.DeleteCollabDrawingBoard(id); err != nil {
		return err
	}
	if err := os.Remove(drawingBoardSnapshotPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove snapshot: %w", err)
	}
	return nil
}

func writeDrawingBoardSnapshotFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*.png")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func handleDashboardDrawingBoardSnapshotGet(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := drawingBoardPathID(w, r); ok {
		serveDrawingBoardSnapshot(w, r, id)
	}
}

// POST /api/drawing-boards/{id}/participants {agent_id, remove?}
func handleDashboardDrawingBoardParticipants(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		AgentID string `json:"agent_id"`
		Remove  bool   `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid participant: "+err.Error())
		return
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "agent_id is required")
		return
	}
	if !req.Remove {
		a, err := db.GetAgent(agentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if !a.Active() {
			writeError(w, http.StatusNotFound, "not_found", "no active agent "+guideText(agentID, 80))
			return
		}
	}
	participants, err := db.SetCollabDrawingBoardParticipant(id, agentID, req.Remove)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	drawingBoardLive.publish(id, "")
	writeJSON(w, http.StatusOK, map[string]any{"participants": participants})
}

// drawingBoardEvents rate-limits operator drawing board activity per drawing board.
var drawingBoardEvents = &drawingBoardEventLimiter{last: map[string]time.Time{}}

// drawingBoardNow is swapped by tests.
var drawingBoardNow = time.Now

type drawingBoardEventLimiter struct {
	sync.Mutex
	last map[string]time.Time
}

func (l *drawingBoardEventLimiter) allow(id string) bool {
	l.Lock()
	defer l.Unlock()
	now := drawingBoardNow()
	if last, ok := l.last[id]; ok && now.Sub(last) < drawingBoardEventInterval {
		return false
	}
	l.last[id] = now
	return true
}

func (l *drawingBoardEventLimiter) forget(id string) {
	l.Lock()
	defer l.Unlock()
	delete(l.last, id)
}

type drawingBoardEventRecipient struct {
	AgentID   string `json:"agent_id"`
	ConvID    string `json:"conv_id"`
	MessageID int64  `json:"message_id,omitempty"`
	Queued    bool   `json:"queued"`
	Error     string `json:"error,omitempty"`
}

// POST /api/drawing-boards/{id}/events {text, image?} — tell the drawing board's online
// participants what the operator just did on it. image is an optional base64
// PNG of the changed region (see dashboard_drawing_board_regions.go).
func handleDashboardDrawingBoardEvent(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, drawingBoardMaxEventBytes+drawingBoardMaxRegionBytes*4/3+1024)
	var req struct {
		Text  string `json:"text"`
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid event: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > drawingBoardMaxEventBytes || !drawingBoardPrintable(text, true) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("text must be printable, non-empty and at most %d bytes", drawingBoardMaxEventBytes))
		return
	}
	image, err := decodeDrawingBoardRegion(req.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	// Liveness is read now, not from the dashboard poll cache, so an agent
	// that just stopped is not left durable mail (see the live announcement).
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", "list live agents: "+err.Error())
		return
	}
	if !drawingBoardEvents.allow(id) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "at most one drawing board event per drawing board every 3s")
		return
	}
	var online []*db.Agent
	for _, agentID := range drawingBoardAudience(b) {
		a, err := db.GetAgent(agentID)
		if err != nil || !a.Active() || a.CurrentConvID == "" || !isConvOnlineIn(a.CurrentConvID, alive) {
			continue
		}
		online = append(online, a)
	}
	// This mail is operator-authored, so it quotes nothing agents wrote: not
	// the title, which any agent with drawing-boards.write can set.
	body := fmt.Sprintf("%s\n\n(Drawing board %s is at version %d. Read it with `tclaude agent drawing-board show %s`, or `--since <version>` for changes only.)",
		text, b.ID, b.Version, b.ID)
	region := 0
	if len(image) > 0 && len(online) > 0 {
		if n, err := storeDrawingBoardRegion(b.ID, image); err == nil {
			region = n
			body += fmt.Sprintf("\n(See what changed: `tclaude agent drawing-board snapshot %s --region %d -o %s-region-%d.png`, then view the image.)",
				b.ID, n, b.ID, n)
		}
	}
	recipients := []drawingBoardEventRecipient{}
	for _, a := range online {
		agentID := a.AgentID
		row := drawingBoardEventRecipient{AgentID: agentID, ConvID: a.CurrentConvID}
		msgID, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: "", ToConv: a.CurrentConvID, Subject: "[drawing-board " + b.ID + "]", Body: body,
			ToRecipients: []string{a.CurrentConvID}, OperatorAuthored: true,
		})
		if err != nil {
			if full, ok := agentMessageQueueFull(err); ok {
				row.Error = queueFullHint(full.Pending, full.Limit)
			} else {
				row.Error = "failed to queue message: " + err.Error()
			}
		} else {
			row.MessageID, row.Queued = msgID, true
		}
		recipients = append(recipients, row)
	}
	setAuditTargetLabel(r, "drawing board "+b.ID)
	resp := map[string]any{"recipients": recipients}
	if region > 0 {
		resp["region"] = region
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- agent surface (/v1/drawing-boards) ----

// drawingBoardCaller resolves the calling agent's id. An implicit-human caller (no
// conversation) authors as the operator.
func drawingBoardCaller(w http.ResponseWriter, convID string) (author, participant string, ok bool) {
	if convID == "" {
		return drawingBoardAuthorOperator, "", true
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		writeError(w, http.StatusForbidden, "unknown_agent", "the calling conversation has no agent identity")
		return "", "", false
	}
	return agentID, agentID, true
}

// GET /v1/drawing-boards — every drawing board under drawing-boards.read; without it, the drawing boards an
// agent reads as a member of their group (groups.resources.read) and those a
// scoped resources.read grant reaches. An agent with none of these gets the
// shared gate's answer for drawing-boards.read.
func handleDrawingBoardList(w http.ResponseWriter, r *http.Request) {
	clearAuthorizedPermission(r)
	if conv := confirmedAgentConv(r); conv != "" && !silentAgentAllows(r, conv, PermDrawingBoardsRead, ActionContext{}) {
		readable := map[string]bool{}
		groups, err := db.ListGroupsForConv(conv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		for _, g := range groups {
			if !g.IsArchived() && agentHoldsGroupResource(r, conv, g.Name, false) {
				readable[g.Name] = true
			}
		}
		umbrella, scoped := scopedResourceUmbrella(r, conv, PermDrawingBoardsRead)
		if len(readable) > 0 || scoped {
			serveDrawingBoardList(w, func(b *db.CollabDrawingBoard) bool {
				if readable[b.GroupName] {
					return true
				}
				if !scoped {
					return false
				}
				var groups []string
				if b.GroupName != "" {
					groups = []string{b.GroupName}
				}
				ok, _ := umbrellaVerdictReaches(umbrella, conv, PermDrawingBoardsRead, groups, resourceRef(resourceScopeKindDrawingBoard, b.ID), false)
				return ok
			})
			return
		}
	} else if conv != "" {
		serveDrawingBoardList(w, nil)
		return
	}
	if _, ok := requirePermission(w, r, PermDrawingBoardsRead); !ok {
		return
	}
	serveDrawingBoardList(w, nil)
}

func handleDrawingBoardRead(w http.ResponseWriter, r *http.Request) {
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	convID, ok := requireDrawingBoardAccess(w, r, id, false)
	if !ok {
		return
	}
	// The read counts toward the agent's presence ("looking").
	if convID != "" {
		if agentID, err := db.AgentIDForConv(convID); err == nil && agentID != "" {
			if _, err := db.GetCollabDrawingBoard(id); err == nil {
				drawingBoardPresence.noteRead(id, agentID)
				drawingBoardLive.publish(id, "")
			}
		}
	}
	serveDrawingBoardRead(w, r, id)
}

func handleDrawingBoardSnapshot(w http.ResponseWriter, r *http.Request) {
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if _, ok := requireDrawingBoardAccess(w, r, id, false); ok {
		serveDrawingBoardSnapshot(w, r, id)
	}
}

// POST /v1/drawing-boards — drawing-boards.write makes any drawing board; a member makes one in its
// group under groups.resources.write.
func handleDrawingBoardCreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid drawing board: "+err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var peek struct {
		Group string `json:"group"`
	}
	_ = json.Unmarshal(body, &peek)
	group := ""
	if name := strings.TrimSpace(peek.Group); name != "" {
		if g, err := db.GetAgentGroupByName(name); err == nil && g != nil && !g.IsArchived() {
			group = g.Name
		}
	}
	convID, ok := requireGroupResourceAccess(w, r, PermDrawingBoardsWrite, group, "", true)
	if !ok {
		return
	}
	author, participant, ok := drawingBoardCaller(w, convID)
	if !ok {
		return
	}
	serveDrawingBoardCreate(w, r, author, participant)
}

func handleDrawingBoardPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	convID, ok := requireDrawingBoardAccess(w, r, id, true)
	if !ok {
		return
	}
	if refuseLockedForAgent(w, r, resourceKindDrawingBoard, id, "drawing board "+id) {
		return
	}
	author, participant, ok := drawingBoardCaller(w, convID)
	if !ok {
		return
	}
	serveDrawingBoardPatch(w, r, id, author, participant, "")
}

// drawingBoardSessionCaller gates the session routes and resolves whose session is
// meant: always the caller's own, which is what keeps an agent from closing
// another's. A session belongs to an agent, so a caller without one has none.
func drawingBoardSessionCaller(w http.ResponseWriter, r *http.Request) (id, agentID string, ok bool) {
	if id, ok = drawingBoardPathID(w, r); !ok {
		return "", "", false
	}
	convID, ok := requireDrawingBoardAccess(w, r, id, true)
	if !ok {
		return "", "", false
	}
	if _, agentID, ok = drawingBoardCaller(w, convID); !ok {
		return "", "", false
	}
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "no_agent", "a drawing board session belongs to an agent; this caller is not one")
		return "", "", false
	}
	return id, agentID, true
}

// POST /v1/drawing-boards/{id}/session — (re)start the calling agent's drawing board session.
func handleDrawingBoardSessionOpen(w http.ResponseWriter, r *http.Request) {
	id, agentID, ok := drawingBoardSessionCaller(w, r)
	if !ok {
		return
	}
	b, err := db.OpenCollabDrawingBoardSession(id, agentID)
	if errors.Is(err, db.ErrCollabDrawingBoardAgentRetired) {
		writeError(w, http.StatusConflict, "retired", "a retired agent cannot hold a drawing board session")
		return
	}
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	// Opening may have made the agent a participant, which the editor shows.
	drawingBoardLive.publish(id, "")
	writeJSON(w, http.StatusOK, drawingBoardToJSON(b, nil))
}

// DELETE /v1/drawing-boards/{id}/session — end the calling agent's drawing board session.
func handleDrawingBoardSessionClose(w http.ResponseWriter, r *http.Request) {
	id, agentID, ok := drawingBoardSessionCaller(w, r)
	if !ok {
		return
	}
	err := db.CloseCollabDrawingBoardSession(id, agentID)
	if errors.Is(err, db.ErrCollabDrawingBoardNoSession) {
		writeError(w, http.StatusNotFound, "no_session", "you have no session on this drawing board")
		return
	}
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
