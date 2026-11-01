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

// Collab boards (tcl-4f86kp): a shared drawing board for the operator and
// agents. The operator draws in the dashboard board editor (/api/boards);
// agents read and write the same board through `tclaude agent board`
// (/v1/boards, boards.read / boards.write). Elements are a bounded subset of
// Excalidraw's element JSON, so a board exports as `.excalidraw`.
//
//   - Unknown element keys are dropped; the rest are validated and bounded.
//   - Each element merges independently with Excalidraw's reconciliation
//     rule (higher version wins, a version tie goes to the lower nonce).
//     Deletion is a tombstone (isDeleted) that stays so it can sync.
//   - customData.author is always set by the server: "operator" on /api, the
//     calling agent's id on /v1.
//   - The latest PNG snapshot (rendered by the dashboard) is a file under
//     config.DataDir()/boards, private daemon state, stored with the board
//     version it shows.
//   - Operator board activity (POST /api/boards/{id}/events) becomes operator
//     mail to each online participant, at most once per board per 3s.

const (
	PermBoardsRead  = "boards.read"
	PermBoardsWrite = "boards.write"
)

const (
	boardAuthorOperator  = "operator"
	boardMaxLiveElements = 5000
	boardMaxStoredRows   = 20000 // live elements plus tombstones
	boardMaxBoards       = 500
	boardMaxVersion      = 1<<53 - 1 // stays exact in JavaScript numbers
	boardMaxRequestBody  = 4 << 20
	// A dashboard edit also carries what its undo steps put back. Keep in
	// step with BOARD_MAX_STEPS_BODY in board-island.js.
	boardMaxRequestBodyWithSteps = 12 << 20
	boardMaxSnapshotBytes        = 8 << 20
	boardMaxCoordinate           = 1e6
	boardMaxStack                = 1e15
	boardMaxPoints               = 5000
	boardMaxTextBytes            = 4000
	boardMaxTitleBytes           = 200
	boardMaxEventBytes           = 2000
	boardEventInterval           = 3 * time.Second
)

var (
	boardIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	elementIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	boardColor       = regexp.MustCompile(`^(#[0-9A-Fa-f]{6}|#[0-9A-Fa-f]{3}|transparent)$`)
	pngMagic         = []byte("\x89PNG\r\n\x1a\n")
)

var (
	boardElementTypes = map[string]bool{"rectangle": true, "ellipse": true, "diamond": true, "arrow": true, "line": true, "freedraw": true, "text": true}
	boardLinearTypes  = map[string]bool{"arrow": true, "line": true, "freedraw": true}
	boardFillStyles   = map[string]bool{"solid": true, "hachure": true, "none": true}
	boardStrokeStyles = map[string]bool{"solid": true, "dashed": true, "dotted": true}
	boardTextAligns   = map[string]bool{"left": true, "center": true, "right": true}
	boardRoutings     = map[string]bool{"straight": true, "elbow": true, "curved": true}
)

// boardElement is the stored Excalidraw subset. Decoding into it drops every
// key it does not name; pointers keep absent optional fields absent.
type boardElement struct {
	ID              string            `json:"id"`
	Type            string            `json:"type,omitempty"`
	X               *float64          `json:"x,omitempty"`
	Y               *float64          `json:"y,omitempty"`
	Width           *float64          `json:"width,omitempty"`
	Height          *float64          `json:"height,omitempty"`
	Angle           *float64          `json:"angle,omitempty"`
	StrokeColor     string            `json:"strokeColor,omitempty"`
	BackgroundColor string            `json:"backgroundColor,omitempty"`
	FillStyle       string            `json:"fillStyle,omitempty"`
	StrokeWidth     *float64          `json:"strokeWidth,omitempty"`
	StrokeStyle     string            `json:"strokeStyle,omitempty"`
	Roughness       *float64          `json:"roughness,omitempty"`
	Opacity         *float64          `json:"opacity,omitempty"`
	Points          [][]float64       `json:"points,omitempty"`
	Text            *string           `json:"text,omitempty"`
	FontSize        *float64          `json:"fontSize,omitempty"`
	FontFamily      any               `json:"fontFamily,omitempty"` // Excalidraw's: 1 hand, 2 sans, 3 mono
	FontWeight      any               `json:"fontWeight,omitempty"` // bold
	FontStyle       any               `json:"fontStyle,omitempty"`  // italic
	TextAlign       string            `json:"textAlign,omitempty"`
	ContainerID     string            `json:"containerId,omitempty"`
	StartBinding    *boardBinding     `json:"startBinding,omitempty"`
	EndBinding      *boardBinding     `json:"endBinding,omitempty"`
	Routing         string            `json:"routing,omitempty"`
	Z               *float64          `json:"z,omitempty"` // its place in the stack, see stackOrder in board-model.js
	Version         int64             `json:"version"`
	VersionNonce    int64             `json:"versionNonce"`
	IsDeleted       bool              `json:"isDeleted"`
	Updated         int64             `json:"updated,omitempty"`
	CustomData      *boardElementMeta `json:"customData,omitempty"`
}

// boardBinding attaches one end of a line to an element. FixedPoint is the
// snap point as Excalidraw stores it, [x, y] fractions of the element's box;
// without one the end meets the element wherever the line reaches it.
type boardBinding struct {
	ElementID  string    `json:"elementId"`
	FixedPoint []float64 `json:"fixedPoint,omitempty"`
}

type boardElementMeta struct {
	Author string `json:"author"`
}

// normalizeBoardElement decodes one element, drops unknown keys and checks
// every bound. author replaces whatever customData the client sent.
func normalizeBoardElement(raw json.RawMessage, author string) (db.CollabBoardElement, error) {
	var e boardElement
	if err := json.Unmarshal(raw, &e); err != nil {
		return db.CollabBoardElement{}, fmt.Errorf("invalid element: %v", err)
	}
	if !elementIDPattern.MatchString(e.ID) {
		return db.CollabBoardElement{}, errors.New("element id must be 1-64 characters of [A-Za-z0-9_-]")
	}
	fail := func(format string, args ...any) (db.CollabBoardElement, error) {
		return db.CollabBoardElement{}, fmt.Errorf("element %s: "+format, append([]any{e.ID}, args...)...)
	}
	if e.Version < 1 || e.Version > boardMaxVersion {
		return fail("version must be an integer from 1 to 2^53-1")
	}
	if e.VersionNonce < math.MinInt32 || e.VersionNonce > math.MaxInt32 {
		return fail("versionNonce must be a 32-bit integer")
	}
	if e.Type == "" && !e.IsDeleted {
		return fail("type is required")
	}
	if e.Type != "" && !boardElementTypes[e.Type] {
		return fail("type must be one of rectangle, ellipse, diamond, arrow, line, freedraw, text")
	}
	for name, v := range map[string]*float64{"x": e.X, "y": e.Y, "width": e.Width, "height": e.Height, "angle": e.Angle} {
		if v != nil && !boardFinite(*v, boardMaxCoordinate) {
			return fail("%s must be a finite number with |v| <= 1e6", name)
		}
	}
	for name, c := range map[string]string{"strokeColor": e.StrokeColor, "backgroundColor": e.BackgroundColor} {
		if c != "" && (len(c) > 32 || !boardColor.MatchString(c)) {
			return fail("%s must be #rrggbb, #rgb or transparent", name)
		}
	}
	if e.FillStyle != "" && !boardFillStyles[e.FillStyle] {
		return fail("fillStyle must be solid, hachure or none")
	}
	if e.StrokeStyle != "" && !boardStrokeStyles[e.StrokeStyle] {
		return fail("strokeStyle must be solid, dashed or dotted")
	}
	if e.TextAlign != "" && !boardTextAligns[e.TextAlign] {
		return fail("textAlign must be left, center or right")
	}
	if !boardInRange(e.StrokeWidth, 0.5, 20) {
		return fail("strokeWidth must be 0.5-20")
	}
	if !boardInRange(e.Roughness, 0, 2) {
		return fail("roughness must be 0-2")
	}
	if !boardInRange(e.Opacity, 0, 100) {
		return fail("opacity must be 0-100")
	}
	if !boardInRange(e.FontSize, 4, 200) {
		return fail("fontSize must be 4-200")
	}
	// What this board cannot draw goes the way of any key it does not know,
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
	if !boardLinearTypes[e.Type] {
		e.Points = nil
	}
	if len(e.Points) > boardMaxPoints {
		return fail("at most %d points", boardMaxPoints)
	}
	for _, p := range e.Points {
		if len(p) != 2 || !boardFinite(p[0], boardMaxCoordinate) || !boardFinite(p[1], boardMaxCoordinate) {
			return fail("points must be [x, y] pairs of finite numbers with |v| <= 1e6")
		}
	}
	if boardLinearTypes[e.Type] && !e.IsDeleted && len(e.Points) < 2 {
		return fail("%s needs at least 2 points", e.Type)
	}
	if e.Text != nil {
		if len(*e.Text) > boardMaxTextBytes {
			return fail("text is longer than %d bytes", boardMaxTextBytes)
		}
		if !boardPrintable(*e.Text, true) {
			return fail("text must be printable (newlines allowed)")
		}
	}
	if !boardLinearTypes[e.Type] {
		e.StartBinding, e.EndBinding = nil, nil
	}
	for _, b := range []*boardBinding{e.StartBinding, e.EndBinding} {
		if b == nil {
			continue
		}
		if !elementIDPattern.MatchString(b.ElementID) {
			return fail("binding elementId must be 1-64 characters of [A-Za-z0-9_-]")
		}
		if b.FixedPoint == nil {
			continue
		}
		if len(b.FixedPoint) != 2 || !boardFinite(b.FixedPoint[0], boardMaxCoordinate) || !boardFinite(b.FixedPoint[1], boardMaxCoordinate) {
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
	if e.Routing != "" && !boardRoutings[e.Routing] {
		return fail("routing must be straight, elbow or curved")
	}
	if e.Type != "text" {
		e.ContainerID = ""
	}
	if e.Z != nil && !boardFinite(*e.Z, boardMaxStack) {
		return fail("z must be a finite number")
	}
	if e.ContainerID != "" && !elementIDPattern.MatchString(e.ContainerID) {
		return fail("containerId must be 1-64 characters of [A-Za-z0-9_-]")
	}
	if e.Updated < 0 || e.Updated > boardMaxVersion {
		return fail("updated must be a millisecond epoch")
	}
	if e.IsDeleted {
		// A tombstone only needs what the merge and sync read.
		e = boardElement{ID: e.ID, Type: e.Type, Version: e.Version, VersionNonce: e.VersionNonce, IsDeleted: true, Updated: e.Updated}
	}
	e.CustomData = &boardElementMeta{Author: author}
	body, err := json.Marshal(e)
	if err != nil {
		return db.CollabBoardElement{}, err
	}
	return db.CollabBoardElement{ID: e.ID, Version: e.Version, Nonce: e.VersionNonce, Deleted: e.IsDeleted, Body: body}, nil
}

func boardFinite(v, bound float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= bound
}

func boardInRange(v *float64, lo, hi float64) bool {
	return v == nil || (!math.IsNaN(*v) && *v >= lo && *v <= hi)
}

// boardPrintable accepts printable text; newlines (and tabs) only when
// multiline.
func boardPrintable(s string, multiline bool) bool {
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

// boardIDFromTitle derives a board id from a title: lowercase, runs of other
// characters become one dash, clipped to the id length.
func boardIDFromTitle(title string) string {
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
		return "board"
	}
	return id
}

// ---- JSON shapes ----

type boardSummaryJSON struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Version      int64     `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
	ElementCount int       `json:"element_count"`
	Participants []string  `json:"participants"`
	Group        string    `json:"group,omitempty"`
	Agents       int       `json:"agents"`           // on it, invited or through its group
	Active       bool      `json:"active,omitempty"` // an agent holds a session on it
}

type boardJSON struct {
	ID              string             `json:"id"`
	Title           string             `json:"title"`
	Version         int64              `json:"version"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	UpdatedBy       string             `json:"updated_by"`
	Participants    []string           `json:"participants"`
	SnapshotVersion *int64             `json:"snapshot_version,omitempty"`
	Elements        []json.RawMessage  `json:"elements"`
	Presence        *boardPresenceJSON `json:"presence,omitempty"`
	// Group is the group the board belongs to; its active members are on the
	// board (GroupMembers) without an invitation (tcl-4yhow7).
	Group        string   `json:"group,omitempty"`
	GroupMembers []string `json:"group_members,omitempty"`
}

// boardElementJSON stamps the stored body with its server_version.
func boardElementJSON(e db.CollabBoardElement) json.RawMessage {
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

func boardToJSON(b *db.CollabBoard, elems []db.CollabBoardElement) boardJSON {
	out := boardJSON{
		ID: b.ID, Title: b.Title, Version: b.Version, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		UpdatedBy: b.UpdatedBy, Participants: b.Participants, Elements: make([]json.RawMessage, 0, len(elems)),
		Group: b.GroupName,
	}
	if members, err := db.CollabBoardGroupMembers(b); err == nil {
		out.GroupMembers = members
	}
	if b.HasSnapshot {
		v := b.SnapshotVersion
		out.SnapshotVersion = &v
	}
	for _, e := range elems {
		out.Elements = append(out.Elements, boardElementJSON(e))
	}
	return out
}

func writeBoardStoreError(w http.ResponseWriter, err error) {
	var tooMany *db.CollabBoardTooManyElementsError
	switch {
	case errors.Is(err, db.ErrCollabBoardNotFound):
		writeError(w, http.StatusNotFound, "not_found", "board not found")
	case errors.Is(err, db.ErrCollabBoardExists):
		writeError(w, http.StatusConflict, "exists", "a board with that id already exists")
	case errors.Is(err, db.ErrCollabBoardLimit):
		writeError(w, http.StatusConflict, "too_many_boards", fmt.Sprintf("at most %d boards; delete one first", boardMaxBoards))
	case errors.As(err, &tooMany):
		writeError(w, http.StatusBadRequest, "too_many_elements", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	}
}

// boardPathID reads and checks the {id} path value.
func boardPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !boardIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "board not found")
		return "", false
	}
	return id, true
}

// ---- shared handlers (both surfaces) ----

func serveBoardList(w http.ResponseWriter) {
	boards, err := db.ListCollabBoards()
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	sessions, err := db.ListCollabBoardSessions()
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	active := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		active[s.BoardID] = true
	}
	out := make([]boardSummaryJSON, 0, len(boards))
	for _, b := range boards {
		out = append(out, boardSummaryJSON{
			ID: b.ID, Title: b.Title, Version: b.Version, UpdatedAt: b.UpdatedAt,
			ElementCount: b.ElementCount, Participants: b.Participants, Group: b.GroupName,
			Agents: len(boardAudience(b)), Active: active[b.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"boards": out})
}

// boardAudience is everyone on the board: its group's members are on it as well
// as those invited.
func boardAudience(b *db.CollabBoard) []string {
	audience := slices.Clone(b.Participants)
	if members, err := db.CollabBoardGroupMembers(b); err == nil {
		for _, m := range members {
			if !slices.Contains(audience, m) {
				audience = append(audience, m)
			}
		}
	}
	return audience
}

// resolveBoardGroup turns a group name into its id for linking a board; ""
// is no group. An agent (agentID set) may only link a board to a group it is
// in: the link mails the board's activity to every member.
func resolveBoardGroup(w http.ResponseWriter, name, agentID string) (int64, bool) {
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
			writeError(w, http.StatusForbidden, "not_member", "you can only link a board to a group you are a member of")
			return 0, false
		}
	}
	return g.ID, true
}

func serveBoardCreate(w http.ResponseWriter, r *http.Request, by, agentID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid board: "+err.Error())
		return
	}
	id, title := strings.TrimSpace(req.ID), strings.TrimSpace(req.Title)
	if title == "" {
		title = id
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a board needs a title or an id")
		return
	}
	if len(title) > boardMaxTitleBytes || !boardPrintable(title, false) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("title must be printable text of at most %d bytes", boardMaxTitleBytes))
		return
	}
	if id == "" {
		id = boardIDFromTitle(title)
	}
	if !boardIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "board id must match [a-z0-9][a-z0-9-]{0,63}")
		return
	}
	groupID, ok := resolveBoardGroup(w, req.Group, agentID)
	if !ok {
		return
	}
	b, err := db.CreateCollabBoard(id, title, by, boardMaxBoards)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	if groupID > 0 {
		if err := db.SetCollabBoardGroup(id, groupID); err != nil {
			// Leave no half-made board for the retry to collide with.
			if delErr := deleteBoardAndSnapshot(id); delErr != nil {
				slog.Warn("boards: remove board after failed group link", "board", id, "error", delErr)
			}
			writeBoardStoreError(w, err)
			return
		}
		if b, err = db.GetCollabBoard(id); err != nil {
			writeBoardStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, boardToJSON(b, nil))
}

// serveBoardRead returns the board. Without ?since it carries the live
// elements; with ?since=N every element (tombstones included) whose
// server_version is above N.
func serveBoardRead(w http.ResponseWriter, r *http.Request, id string) {
	since := int64(-1)
	if s := r.URL.Query().Get("since"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since must be a board version >= 0")
			return
		}
		since = n
	}
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	elems, err := db.ListCollabBoardElements(id, since)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	out := boardToJSON(b, elems)
	out.Presence = boardPresence.snapshot(id)
	writeJSON(w, http.StatusOK, out)
}

// serveBoardPatch merges elements authored by author; participant (an agent
// id, or empty) joins the board in the same transaction.
//
// historyActor, when set, takes the request's undo steps into that actor's
// history. A step's before values are stored as sent and only normalized when
// an undo uses them, so a bad step costs that undo and never the edit.
func serveBoardPatch(w http.ResponseWriter, r *http.Request, id, author, participant, historyActor string) {
	limit := int64(boardMaxRequestBody)
	if historyActor != "" {
		limit = boardMaxRequestBodyWithSteps
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var req struct {
		Elements []json.RawMessage             `json:"elements"`
		Steps    [][]db.CollabBoardStepElement `json:"steps"`
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
	elems := make([]db.CollabBoardElement, 0, len(req.Elements))
	for i, raw := range req.Elements {
		e, err := normalizeBoardElement(raw, author)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_element", fmt.Sprintf("elements[%d]: %v", i, err))
			return
		}
		elems = append(elems, e)
	}
	var history *db.CollabBoardHistory
	if historyActor != "" {
		history = &db.CollabBoardHistory{Actor: historyActor, Steps: boardStepsWithAfter(validBoardSteps(req.Steps), elems)}
	}
	version, accepted, err := db.ApplyCollabBoardEdit(id, author, participant, elems, history, boardMaxLiveElements, boardMaxStoredRows)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	if accepted > 0 {
		boardPresence.noteEdit(id, participant)
	}
	if accepted > 0 || participant != "" {
		boardLive.publish(id, boardClient(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "accepted": accepted})
}

func boardSnapshotPath(id string) string {
	return filepath.Join(config.DataDir(), "boards", id+".png")
}

func serveBoardSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	if region := r.URL.Query().Get("region"); region != "" {
		serveBoardRegion(w, id, region)
		return
	}
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	if !b.HasSnapshot {
		writeError(w, http.StatusNotFound, "no_snapshot", "the board has no snapshot yet")
		return
	}
	data, err := os.ReadFile(boardSnapshotPath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_snapshot", "the board has no snapshot yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read snapshot: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Board-Snapshot-Version", strconv.FormatInt(b.SnapshotVersion, 10))
	w.Header().Set("X-Board-Version", strconv.FormatInt(b.Version, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- dashboard surface (/api/boards) ----

func registerDashboardBoardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/boards", collabRoute(handleDashboardBoardList))
	mux.HandleFunc("POST /api/boards", collabRoute(handleDashboardBoardCreate))
	mux.HandleFunc("GET /api/boards/{id}", collabRoute(handleDashboardBoardRead))
	mux.HandleFunc("PATCH /api/boards/{id}", collabRoute(handleDashboardBoardPatch))
	mux.HandleFunc("DELETE /api/boards/{id}", collabRoute(handleDashboardBoardDelete))
	mux.HandleFunc("PUT /api/boards/{id}/snapshot", collabRoute(handleDashboardBoardSnapshotPut))
	mux.HandleFunc("GET /api/boards/{id}/snapshot", collabRoute(handleDashboardBoardSnapshotGet))
	mux.HandleFunc("POST /api/boards/{id}/participants", collabRoute(handleDashboardBoardParticipants))
	mux.HandleFunc("POST /api/boards/{id}/events", collabRoute(handleDashboardBoardEvent))
	mux.HandleFunc("POST /api/boards/{id}/view", collabRoute(handleDashboardBoardView))
	mux.HandleFunc("POST /api/boards/{id}/group", collabRoute(handleDashboardBoardGroup))
	mux.HandleFunc("GET /api/boards/{id}/live", collabRoute(handleDashboardBoardLive))
	mux.HandleFunc("POST /api/boards/{id}/undo", collabRoute(handleDashboardBoardUndo))
	mux.HandleFunc("DELETE /api/boards/{id}/sessions", collabRoute(handleDashboardBoardSessionsEnd))
}

// DELETE /api/boards/{id}/sessions — the operator ends every agent's session
// on the board, its row in the Background terminals menu. The board stays.
func handleDashboardBoardSessionsEnd(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	if _, err := db.GetCollabBoard(id); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	ended, err := db.EndCollabBoardSessions(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	setAuditTargetLabel(r, "board "+id)
	writeJSON(w, http.StatusOK, map[string]any{"ended": ended})
}

// POST /api/boards/{id}/group {group} — link the board to a group ("" unlinks).
func handleDashboardBoardGroup(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
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
	groupID, ok := resolveBoardGroup(w, req.Group, "")
	if !ok {
		return
	}
	if err := db.SetCollabBoardGroup(id, groupID); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	boardLive.publish(id, "")
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	setAuditTargetLabel(r, "board "+b.ID)
	out := boardToJSON(b, nil)
	writeJSON(w, http.StatusOK, map[string]any{"group": out.Group, "group_members": out.GroupMembers})
}

func handleDashboardBoardList(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveBoardList(w)
}

func handleDashboardBoardCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveBoardCreate(w, r, boardAuthorOperator, "")
}

func handleDashboardBoardRead(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := boardPathID(w, r); ok {
		serveBoardRead(w, r, id)
	}
}

func handleDashboardBoardPatch(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := boardPathID(w, r); ok {
		serveBoardPatch(w, r, id, boardAuthorOperator, "", boardAuthorOperator)
	}
}

func handleDashboardBoardDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	// Who was on it can only be read before it goes.
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	audience := boardAudience(b)
	if err := deleteBoardAndSnapshot(id); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	boardEvents.forget(id)
	boardPresence.forget(id)
	boardLive.publish(id, "")
	tellBoardDeleted(id, audience)
	setAuditTargetLabel(r, "board "+id)
	if err := removeBoardRegions(id); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "remove event images: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tellBoardDeleted mails everyone who was on a deleted board. Unlike activity
// mail this goes to stopped agents too: the news is still true when they
// resume. A board that is gone stays gone whoever could not be told, so a
// failure is only logged. Operator-authored, this quotes the id, which is held
// to [a-z0-9-], and not the title, which any agent with boards.write can set.
func tellBoardDeleted(id string, audience []string) {
	body := fmt.Sprintf("The operator deleted board %s. Its contents are gone, and `tclaude agent board` commands for it now fail.", id)
	for _, agentID := range audience {
		a, err := db.GetAgent(agentID)
		if err != nil || !a.Active() || a.CurrentConvID == "" {
			continue
		}
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: "", ToConv: a.CurrentConvID, Subject: "[board " + id + "]", Body: body,
			ToRecipients: []string{a.CurrentConvID}, OperatorAuthored: true,
		}); err != nil {
			slog.Warn("boards: tell agent of deleted board", "board", id, "agent", agentID, "error", err)
		}
	}
}

// PUT /api/boards/{id}/snapshot?version=N — the dashboard's PNG rendering of
// board version N.
func handleDashboardBoardSnapshotPut(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if err != nil || version < 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "version must be the board version the snapshot shows")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, boardMaxSnapshotBytes))
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
	boardSnapshotMu.Lock()
	defer boardSnapshotMu.Unlock()
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	if b.HasSnapshot && version < b.SnapshotVersion {
		writeError(w, http.StatusConflict, "stale_snapshot", fmt.Sprintf("a snapshot of version %d is already stored", b.SnapshotVersion))
		return
	}
	if version > b.Version {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("version %d is ahead of the board (version %d)", version, b.Version))
		return
	}
	if err := writeBoardSnapshotFile(boardSnapshotPath(id), data); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "store snapshot: "+err.Error())
		return
	}
	if err := db.SetCollabBoardSnapshot(id, version); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "bytes": len(data)})
}

var boardSnapshotMu sync.Mutex

// deleteBoardAndSnapshot removes the board and its PNG together, so a snapshot
// upload that found the board cannot write its file after this removal.
func deleteBoardAndSnapshot(id string) error {
	boardSnapshotMu.Lock()
	defer boardSnapshotMu.Unlock()
	if err := db.DeleteCollabBoard(id); err != nil {
		return err
	}
	if err := os.Remove(boardSnapshotPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove snapshot: %w", err)
	}
	return nil
}

func writeBoardSnapshotFile(path string, data []byte) error {
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

func handleDashboardBoardSnapshotGet(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := boardPathID(w, r); ok {
		serveBoardSnapshot(w, r, id)
	}
}

// POST /api/boards/{id}/participants {agent_id, remove?}
func handleDashboardBoardParticipants(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
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
	participants, err := db.SetCollabBoardParticipant(id, agentID, req.Remove)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	boardLive.publish(id, "")
	writeJSON(w, http.StatusOK, map[string]any{"participants": participants})
}

// boardEvents rate-limits operator board activity per board.
var boardEvents = &boardEventLimiter{last: map[string]time.Time{}}

// boardNow is swapped by tests.
var boardNow = time.Now

type boardEventLimiter struct {
	sync.Mutex
	last map[string]time.Time
}

func (l *boardEventLimiter) allow(id string) bool {
	l.Lock()
	defer l.Unlock()
	now := boardNow()
	if last, ok := l.last[id]; ok && now.Sub(last) < boardEventInterval {
		return false
	}
	l.last[id] = now
	return true
}

func (l *boardEventLimiter) forget(id string) {
	l.Lock()
	defer l.Unlock()
	delete(l.last, id)
}

type boardEventRecipient struct {
	AgentID   string `json:"agent_id"`
	ConvID    string `json:"conv_id"`
	MessageID int64  `json:"message_id,omitempty"`
	Queued    bool   `json:"queued"`
	Error     string `json:"error,omitempty"`
}

// POST /api/boards/{id}/events {text, image?} — tell the board's online
// participants what the operator just did on it. image is an optional base64
// PNG of the changed region (see dashboard_board_regions.go).
func handleDashboardBoardEvent(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, boardMaxEventBytes+boardMaxRegionBytes*4/3+1024)
	var req struct {
		Text  string `json:"text"`
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid event: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > boardMaxEventBytes || !boardPrintable(text, true) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("text must be printable, non-empty and at most %d bytes", boardMaxEventBytes))
		return
	}
	image, err := decodeBoardRegion(req.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	b, err := db.GetCollabBoard(id)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	// Liveness is read now, not from the dashboard poll cache, so an agent
	// that just stopped is not left durable mail (see the live announcement).
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", "list live agents: "+err.Error())
		return
	}
	if !boardEvents.allow(id) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "at most one board event per board every 3s")
		return
	}
	var online []*db.Agent
	for _, agentID := range boardAudience(b) {
		a, err := db.GetAgent(agentID)
		if err != nil || !a.Active() || a.CurrentConvID == "" || !isConvOnlineIn(a.CurrentConvID, alive) {
			continue
		}
		online = append(online, a)
	}
	// This mail is operator-authored, so it quotes nothing agents wrote: not
	// the title, which any agent with boards.write can set.
	body := fmt.Sprintf("%s\n\n(Board %s is at version %d. Read it with `tclaude agent board show %s`, or `--since <version>` for changes only.)",
		text, b.ID, b.Version, b.ID)
	region := 0
	if len(image) > 0 && len(online) > 0 {
		if n, err := storeBoardRegion(b.ID, image); err == nil {
			region = n
			body += fmt.Sprintf("\n(See what changed: `tclaude agent board snapshot %s --region %d -o %s-region-%d.png`, then view the image.)",
				b.ID, n, b.ID, n)
		}
	}
	recipients := []boardEventRecipient{}
	for _, a := range online {
		agentID := a.AgentID
		row := boardEventRecipient{AgentID: agentID, ConvID: a.CurrentConvID}
		msgID, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: "", ToConv: a.CurrentConvID, Subject: "[board " + b.ID + "]", Body: body,
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
	setAuditTargetLabel(r, "board "+b.ID)
	resp := map[string]any{"recipients": recipients}
	if region > 0 {
		resp["region"] = region
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- agent surface (/v1/boards) ----

// boardCaller resolves the calling agent's id. An implicit-human caller (no
// conversation) authors as the operator.
func boardCaller(w http.ResponseWriter, convID string) (author, participant string, ok bool) {
	if convID == "" {
		return boardAuthorOperator, "", true
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		writeError(w, http.StatusForbidden, "unknown_agent", "the calling conversation has no agent identity")
		return "", "", false
	}
	return agentID, agentID, true
}

func handleBoardList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermBoardsRead); !ok {
		return
	}
	serveBoardList(w)
}

func handleBoardRead(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermBoardsRead)
	if !ok {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	// The read counts toward the agent's presence ("looking").
	if convID != "" {
		if agentID, err := db.AgentIDForConv(convID); err == nil && agentID != "" {
			if _, err := db.GetCollabBoard(id); err == nil {
				boardPresence.noteRead(id, agentID)
				boardLive.publish(id, "")
			}
		}
	}
	serveBoardRead(w, r, id)
}

func handleBoardSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermBoardsRead); !ok {
		return
	}
	if id, ok := boardPathID(w, r); ok {
		serveBoardSnapshot(w, r, id)
	}
}

func handleBoardCreate(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermBoardsWrite)
	if !ok {
		return
	}
	author, participant, ok := boardCaller(w, convID)
	if !ok {
		return
	}
	serveBoardCreate(w, r, author, participant)
}

func handleBoardPatch(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermBoardsWrite)
	if !ok {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	author, participant, ok := boardCaller(w, convID)
	if !ok {
		return
	}
	serveBoardPatch(w, r, id, author, participant, "")
}

// boardSessionCaller gates the session routes and resolves whose session is
// meant: always the caller's own, which is what keeps an agent from closing
// another's. A session belongs to an agent, so a caller without one has none.
func boardSessionCaller(w http.ResponseWriter, r *http.Request) (id, agentID string, ok bool) {
	convID, ok := requirePermission(w, r, PermBoardsWrite)
	if !ok {
		return "", "", false
	}
	if id, ok = boardPathID(w, r); !ok {
		return "", "", false
	}
	if _, agentID, ok = boardCaller(w, convID); !ok {
		return "", "", false
	}
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "no_agent", "a board session belongs to an agent; this caller is not one")
		return "", "", false
	}
	return id, agentID, true
}

// POST /v1/boards/{id}/session — (re)start the calling agent's board session.
func handleBoardSessionOpen(w http.ResponseWriter, r *http.Request) {
	id, agentID, ok := boardSessionCaller(w, r)
	if !ok {
		return
	}
	b, err := db.OpenCollabBoardSession(id, agentID)
	if errors.Is(err, db.ErrCollabBoardAgentRetired) {
		writeError(w, http.StatusConflict, "retired", "a retired agent cannot hold a board session")
		return
	}
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	// Opening may have made the agent a participant, which the editor shows.
	boardLive.publish(id, "")
	writeJSON(w, http.StatusOK, boardToJSON(b, nil))
}

// DELETE /v1/boards/{id}/session — end the calling agent's board session.
func handleBoardSessionClose(w http.ResponseWriter, r *http.Request) {
	id, agentID, ok := boardSessionCaller(w, r)
	if !ok {
		return
	}
	err := db.CloseCollabBoardSession(id, agentID)
	if errors.Is(err, db.ErrCollabBoardNoSession) {
		writeError(w, http.StatusNotFound, "no_session", "you have no session on this board")
		return
	}
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
