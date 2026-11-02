package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Dashboard marks (tcl-s97jr5, tcl-dp9eqk, tcl-gzie3v): what an agent, the
// Steward by default, draws on the operator's view to get their attention. A
// mark is a durable row, active until it times out, a permitted agent clears or
// replaces it, or the operator dismisses or acknowledges it, and the row keeps
// who ended it and how. Open tabs collect the active marks with their presence ping and
// draw them; a mark takes no clicks, only its tip does.

const PermDashboardMark = "dashboard.mark"

const (
	// MarkMaxActive is how many marks can be up at once, across agents.
	MarkMaxActive = 10
	// MarkDefaultTimeout applies when a mark asks for none.
	MarkDefaultTimeout = 60 * time.Second
	// MarkMaxTimeout is the longest a mark can stay up: every mark ends itself.
	MarkMaxTimeout = 10 * time.Minute
	// MarkMaxTip is the longest tip, in characters.
	MarkMaxTip = 280
	// MarkMaxReply is the longest answer the operator can give at a mark.
	MarkMaxReply = 2000
	// MarkMaxPoints bounds a freehand stroke.
	MarkMaxPoints = 400
	// MarkKeepEnded is how many ended marks stay as history.
	MarkKeepEnded = 1000
	// MarkPageDefault and MarkPageMax size a page of the listing.
	MarkPageDefault = 50
	MarkPageMax     = 200

	// CaptureKeepCount and CaptureKeepBytes bound saved captures. The oldest go
	// first; the newest is always kept.
	CaptureKeepCount = 50
	CaptureKeepBytes = 256 << 20

	markMaxBody = 32 << 10
)

// markKinds are what a mark can draw. A label is only its tip.
var markKinds = map[string]bool{
	"highlight": true, "circle": true, "underline": true, "box": true,
	"arrow": true, "freehand": true, "label": true,
}

type markRegion struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// markSpec says where a mark goes and how it looks, in viewport pixels at the
// moment it was drawn for a region or a stroke. Exactly one anchor is set.
type markSpec struct {
	// Target is the visible text of a control; Ref is a CSS selector, steadier
	// when a page has one to offer (a capture reports a control's ref).
	Target string `json:"target,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// From is where an arrow starts, by visible text.
	From   string       `json:"from,omitempty"`
	Region *markRegion  `json:"region,omitempty"`
	Points [][2]float64 `json:"points,omitempty"`
	// Path keeps the mark to one dashboard page.
	Path  string `json:"path,omitempty"`
	Blink bool   `json:"blink,omitempty"`
	Color string `json:"color,omitempty"`
}

// markView is the wire shape of a mark, to tabs and agents alike.
type markView struct {
	ID        int64      `json:"id"`
	AgentID   string     `json:"agent_id,omitempty"`
	AgentName string     `json:"agent_name,omitempty"`
	Kind      string     `json:"kind"`
	Spec      markSpec   `json:"spec"`
	Tip       string     `json:"tip,omitempty"`
	State     string     `json:"state"`
	EndedBy   string     `json:"ended_by,omitempty"`
	Reply     string     `json:"reply,omitempty"`
	RepliedAt *time.Time `json:"replied_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

func viewMark(m *db.DashboardMark) markView {
	view := markView{
		ID: m.ID, AgentID: m.AgentID, AgentName: m.AgentName, Kind: m.Kind, Tip: m.Tip, State: m.State,
		EndedBy: m.EndedBy, Reply: m.Reply, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
	}
	_ = json.Unmarshal([]byte(m.Spec), &view.Spec)
	if !m.RepliedAt.IsZero() {
		view.RepliedAt = &m.RepliedAt
	}
	if !m.EndedAt.IsZero() {
		view.EndedAt = &m.EndedAt
	}
	return view
}

func activeMarkViews() ([]markView, error) {
	rows, err := db.ActiveDashboardMarks(guideNow())
	if err != nil {
		return nil, err
	}
	out := make([]markView, 0, len(rows))
	for _, m := range rows {
		out = append(out, viewMark(m))
	}
	return out, nil
}

// markColors are the colours a mark can take, from the annotation palette.
var markColors = map[string]bool{"": true, "red": true, "yellow": true, "blue": true, "green": true, "purple": true}

// validateMarkRequest cleans a new mark and says what is wrong with it.
func validateMarkRequest(kind *string, spec *markSpec, tip *string) string {
	*kind = strings.ToLower(strings.TrimSpace(*kind))
	if !markKinds[*kind] {
		return "unknown kind " + guideText(*kind, 32) + "; known: highlight, circle, underline, box, arrow, freehand, label"
	}
	if len([]rune(*tip)) > MarkMaxTip {
		return fmt.Sprintf("a tip is at most %d characters", MarkMaxTip)
	}
	*tip = guideText(*tip, MarkMaxTip*4)
	spec.Target = guideText(spec.Target, guideMaxTarget)
	spec.From = guideText(spec.From, guideMaxTarget)
	spec.Ref = guideText(spec.Ref, guideMaxTarget)
	if path, ok := guideViewPath(spec.Path); ok {
		spec.Path = path
	} else {
		return "path must be a dashboard path"
	}
	if !markColors[strings.ToLower(spec.Color)] {
		return "color is red, yellow, blue, green or purple"
	}
	spec.Color = strings.ToLower(spec.Color)
	if len(spec.Points) > MarkMaxPoints {
		return fmt.Sprintf("a stroke has at most %d points", MarkMaxPoints)
	}
	anchors := 0
	for _, set := range []bool{spec.Target != "", spec.Ref != "", spec.Region != nil, len(spec.Points) > 0} {
		if set {
			anchors++
		}
	}
	switch {
	case *kind == "freehand" && len(spec.Points) < 2:
		return "a freehand mark needs at least two points"
	case *kind != "freehand" && len(spec.Points) > 0:
		return "only a freehand mark takes points"
	case *kind == "arrow" && (spec.From == "" || (spec.Target == "" && spec.Ref == "")):
		return "an arrow needs from and a target"
	case *kind != "arrow" && spec.From != "":
		return "only an arrow takes from"
	case *kind == "label" && *tip == "":
		return "a label needs a tip"
	case anchors == 0 && *kind != "label":
		return "a mark needs a target, a ref, a region or points"
	case anchors > 1:
		return "a mark takes one of target, ref, region or points"
	case spec.Region != nil && (spec.Region.Width <= 0 || spec.Region.Height <= 0):
		return "a region needs a width and a height"
	}
	return ""
}

func callerIdentity(convID string) (agentID, name string) {
	if convID == "" {
		return "", ""
	}
	if db.IsExternalStewardConv(convID) {
		return externalStewardIdentity(convID)
	}
	if res, _, err := agent.ResolveSelector(convID); err == nil {
		agentID = res.AgentID
		if res.Row != nil {
			name = guideText(res.Row.CustomTitle, 80)
		}
	}
	return agentID, name
}

// POST /v1/dashboard/marks — draw a mark on the operator's view.
func handleDashboardMarkCreate(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardMark)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, markMaxBody)
	var req struct {
		Kind           string   `json:"kind"`
		Spec           markSpec `json:"spec"`
		Tip            string   `json:"tip"`
		TimeoutSeconds float64  `json:"timeout_seconds"`
		Replaces       int64    `json:"replaces"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid mark: "+err.Error())
		return
	}
	if problem := validateMarkRequest(&req.Kind, &req.Spec, &req.Tip); problem != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", problem)
		return
	}
	timeout := MarkDefaultTimeout
	if req.TimeoutSeconds != 0 {
		timeout = time.Duration(req.TimeoutSeconds * float64(time.Second))
	}
	if timeout <= 0 || timeout > MarkMaxTimeout {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("a mark's timeout is at most %s", MarkMaxTimeout))
		return
	}
	spec, err := json.Marshal(req.Spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	now := guideNow()
	mark := &db.DashboardMark{Kind: req.Kind, Spec: string(spec), Tip: req.Tip, ExpiresAt: now.Add(timeout)}
	mark.AgentID, mark.AgentName = callerIdentity(convID)
	err = db.CreateDashboardMark(mark, req.Replaces, MarkMaxActive, MarkKeepEnded, now)
	switch {
	case errors.Is(err, db.ErrDashboardMarkLimit):
		writeError(w, http.StatusConflict, "too_many_marks", fmt.Sprintf("%d marks are already up; clear or replace one first", MarkMaxActive))
	case errors.Is(err, db.ErrDashboardMarkNotFound), errors.Is(err, db.ErrDashboardMarkEnded):
		writeError(w, http.StatusConflict, "mark_ended", fmt.Sprintf("mark %d is not active, so it cannot be replaced", req.Replaces))
	case err != nil:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	default:
		writeJSON(w, http.StatusOK, viewMark(mark))
	}
}

// pageParams reads the ?before= and ?limit= of a listing.
func pageParams(r *http.Request) (before int64, limit int, ok bool) {
	limit = MarkPageDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		limit = min(n, MarkPageMax)
	}
	if raw := r.URL.Query().Get("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		before = n
	}
	return before, limit, true
}

var markStates = map[string]bool{
	"": true, db.MarkStateActive: true, db.MarkStateExpired: true, db.MarkStateDismissed: true,
	db.MarkStateAcknowledged: true, db.MarkStateCleared: true, db.MarkStateReplaced: true,
}

// GET /v1/dashboard/marks?state=&before=&limit= — marks, newest first. A page
// that may not be the last carries next_before.
func handleDashboardMarkList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermDashboardMark); !ok {
		return
	}
	state := r.URL.Query().Get("state")
	if !markStates[state] {
		writeError(w, http.StatusBadRequest, "invalid_arg", "state is active, expired, dismissed, acknowledged, cleared or replaced")
		return
	}
	before, limit, ok := pageParams(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_arg", "before and limit must be positive integers")
		return
	}
	rows, err := db.ListDashboardMarks(state, before, limit, guideNow())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	marks := make([]markView, 0, len(rows))
	for _, m := range rows {
		marks = append(marks, viewMark(m))
	}
	resp := map[string]any{"marks": marks}
	if len(rows) == limit {
		resp["next_before"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /v1/dashboard/marks/{id} — take one mark down.
func handleDashboardMarkClear(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardMark)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a mark id is a number")
		return
	}
	agentID, _ := callerIdentity(convID)
	writeMarkEnd(w, db.EndDashboardMark(id, db.MarkStateCleared, markEndedBy(agentID), guideNow()))
}

// DELETE /v1/dashboard/marks — take down the caller's own marks.
func handleDashboardMarkClearAll(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardMark)
	if !ok {
		return
	}
	agentID, _ := callerIdentity(convID)
	// The operator's own CLI has no marks of its own and clears everyone's. An
	// agent that cannot be named must not fall into that.
	if convID != "" && agentID == "" {
		writeError(w, http.StatusConflict, "no_agent", "this conversation has no agent identity, so it owns no marks")
		return
	}
	n, err := db.EndActiveDashboardMarks(agentID, db.MarkStateCleared, markEndedBy(agentID), guideNow())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n})
}

func markEndedBy(agentID string) string {
	if agentID == "" {
		return db.MarkEndedByOperator
	}
	return agentID
}

func writeMarkEnd(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrDashboardMarkNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such mark")
	case errors.Is(err, db.ErrDashboardMarkEnded):
		writeError(w, http.StatusConflict, "mark_ended", "the mark has already ended")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// POST /api/companion/marks/{id}/dismiss and /acknowledge — the operator ends a
// mark. Dismiss says "not now", acknowledge says "seen".
func handleDashboardMarkEnd(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "a mark id is a number")
			return
		}
		writeMarkEnd(w, db.EndDashboardMark(id, state, db.MarkEndedByOperator, guideNow()))
	}
}

// POST /api/companion/marks/dismiss — the operator's one action for all of them.
func handleDashboardMarksDismissAll(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if _, err := db.EndActiveDashboardMarks("", db.MarkStateDismissed, db.MarkEndedByOperator, guideNow()); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// markReplyBody is what the agent receives: enough to know which mark, without
// the operator having to say.
func markReplyBody(m *db.DashboardMark, reply string) string {
	body := fmt.Sprintf("[mark %d", m.ID)
	if m.Tip != "" {
		body += fmt.Sprintf(": %q", m.Tip)
	}
	return body + "] " + reply
}

// POST /api/companion/marks/{id}/reply — the operator answers at the mark. The
// answer goes to the agent that drew it as operator mail, which needs no route
// to the agent, and stays on the mark.
func handleDashboardMarkReply(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a mark id is a number")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, markMaxBody)
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid reply: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len([]rune(text)) > MarkMaxReply {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("a reply is 1 to %d characters", MarkMaxReply))
		return
	}
	mark, err := db.ActiveDashboardMark(id, guideNow())
	if err != nil {
		writeMarkEnd(w, err)
		return
	}
	if mark.AgentID == "" {
		writeError(w, http.StatusConflict, "no_agent", "this mark was drawn from the CLI, so nobody is there to answer")
		return
	}
	res, _, err := agent.ResolveSelector(mark.AgentID)
	if err != nil || res == nil {
		writeError(w, http.StatusNotFound, "not_found", "the agent that drew this mark is gone")
		return
	}
	messageID, _, err := queueRegularAgentMessage(&db.AgentMessage{
		ToConv: res.ConvID, Subject: fmt.Sprintf("Reply to mark %d", mark.ID), Body: markReplyBody(mark, text),
		ToRecipients: []string{res.ConvID}, OperatorAuthored: true,
	})
	if full, ok := agentMessageQueueFull(err); ok {
		writeQueueFull(w, res.ConvID, full)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Recorded once it is on its way, so the history never shows an answer the
	// agent did not get.
	if err := db.RecordDashboardMarkReply(mark.ID, text, guideNow()); err != nil {
		slog.Warn("dashboard mark: record reply", "mark", mark.ID, "error", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message_id": messageID})
}

// -- saved captures -----------------------------------------------------------

func captureFilePath(id int64) string {
	return filepath.Join(config.DataDir(), "captures", strconv.FormatInt(id, 10)+".png")
}

// saveCapture keeps a capture for later and applies the retention limit.
func saveCapture(convID, label string, image []byte, ctx *captureContext) (*db.DashboardCapture, error) {
	saved := &db.DashboardCapture{Label: label, Size: int64(len(image))}
	saved.AgentID, saved.AgentName = callerIdentity(convID)
	if ctx != nil {
		if raw, err := json.Marshal(ctx); err == nil {
			saved.Context = string(raw)
		}
	}
	if err := db.SaveDashboardCapture(saved, guideNow()); err != nil {
		return nil, err
	}
	path := captureFilePath(saved.ID)
	if err := writeCaptureFile(path, image); err != nil {
		_ = os.Remove(path)
		_ = db.DeleteDashboardCapture(saved.ID)
		return nil, err
	}
	stale, err := db.PruneDashboardCaptures(CaptureKeepCount, CaptureKeepBytes)
	if err != nil {
		return nil, err
	}
	for _, id := range stale {
		_ = os.Remove(captureFilePath(id))
	}
	return saved, nil
}

func writeCaptureFile(path string, image []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, image, 0o600)
}

type savedCaptureView struct {
	ID        int64           `json:"id"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentName string          `json:"agent_name,omitempty"`
	Label     string          `json:"label,omitempty"`
	Size      int64           `json:"size"`
	CreatedAt time.Time       `json:"created_at"`
	Context   json.RawMessage `json:"context,omitempty"`
}

func viewCapture(c *db.DashboardCapture) savedCaptureView {
	view := savedCaptureView{ID: c.ID, AgentID: c.AgentID, AgentName: c.AgentName, Label: c.Label, Size: c.Size, CreatedAt: c.CreatedAt}
	if c.Context != "" {
		view.Context = json.RawMessage(c.Context)
	}
	return view
}

// GET /v1/dashboard/captures?before=&limit= — kept captures, newest first.
func handleDashboardCaptureList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermDashboardCapture); !ok {
		return
	}
	before, limit, ok := pageParams(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_arg", "before and limit must be positive integers")
		return
	}
	rows, err := db.ListDashboardCaptures(before, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	captures := make([]savedCaptureView, 0, len(rows))
	for _, c := range rows {
		captures = append(captures, viewCapture(c))
	}
	resp := map[string]any{"captures": captures}
	if len(rows) == limit {
		resp["next_before"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/dashboard/captures/{id} — one kept capture's record, and with
// ?image=1 its PNG.
func handleDashboardCaptureRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermDashboardCapture); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a capture id is a number")
		return
	}
	saved, err := db.GetDashboardCapture(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if saved == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such capture; the oldest are removed past the retention limit")
		return
	}
	if !isTruthy(r.URL.Query().Get("image")) {
		writeJSON(w, http.StatusOK, viewCapture(saved))
		return
	}
	image, err := os.ReadFile(captureFilePath(id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read capture: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(image)
}
