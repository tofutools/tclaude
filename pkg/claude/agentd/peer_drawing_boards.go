package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Collab drawing boards under peer grants. A drawing board belongs to the group it is linked
// to, so a restricted peer reaches it through that group, by the group
// siblings of the global drawing board slugs, as a local agent does:
// groups.resources.read lists and reads the drawing boards of the groups it is held
// in, and groups.resources.write (which implies read there) edits their elements and title, creates a
// drawing board in such a group, and moves one between two groups it holds the grant
// in. A drawing board in no group is reached only through the global drawing boards.* slugs,
// which no peer holds: it is no peer's.
// What the peer writes is authored as its operator (operator@<instance>),
// unrestricted trust's too, and never enters this node's undo history. A
// restricted peer's read carries neither the drawing board's presence (who is on it,
// the local operator's view) nor its invited agents, and the group's members
// only with groups.roster.read there; a peer's new drawing board takes its id from its
// title and leaves peerDrawingBoardReserve drawing boards to this node. Unrestricted trust
// otherwise does all this as this node's operator does. Deleting a drawing board, its
// live socket and presence, the operator's activity mail (events), undo, its
// rendered snapshot, inviting agents and ending their sessions stay with this
// node's own dashboard. A drawing board this node's operator locked refuses every
// peer's change, as it does an agent's, until it is unlocked.

// peerDrawingBoardReserve is how many of the node's drawing boards a peer's creates leave to
// this node's operator and agents.
const peerDrawingBoardReserve = 100

func addPeerDrawingBoardRules(rules map[string]peerViewRule) {
	read := peerViewRule{feature: "drawing-boards", requires: PermGroupsResourcesRead, orGrants: []string{PermGroupsResourcesWrite}, group: true}
	write := peerViewRule{feature: "drawing-boards.write", requires: PermGroupsResourcesWrite, group: true}
	list, one := read, read
	list.serve, one.serve = servePeerDrawingBoardList, servePeerDrawingBoard
	rules["GET /api/drawing-boards"] = list
	rules["GET /api/drawing-boards/{id}"] = one
	// Export reads the drawing board, though it is a POST: its body says what
	// to draw.
	export := read
	export.write = servePeerDrawingBoardExport
	rules["POST /api/drawing-boards/{id}/export"] = export
	create, edit := write, write
	create.write, edit.write = servePeerDrawingBoardCreate, servePeerDrawingBoard
	rules["POST /api/drawing-boards"] = create
	for _, pattern := range []string{"PATCH /api/drawing-boards/{id}", "POST /api/drawing-boards/{id}/title", "POST /api/drawing-boards/{id}/group"} {
		rules[pattern] = edit
	}
}

// peerDrawingBoardExportDeadline keeps a peer's export within the peer
// view's exchange (fedPeerViewTimeout), with time to carry it.
const peerDrawingBoardExportDeadline = 10 * time.Second

// peerDrawingBoardExportMaxBytes is the biggest image a peer's export
// carries: base64 in its JSON reply, under fedPeerViewResponseBodyLimit.
const peerDrawingBoardExportMaxBytes = (fedPeerViewResponseBodyLimit - 64<<10) / 4 * 3

// servePeerDrawingBoardExport renders a drawing board the peer reads
// (drawing-board export --node, the editor's File › Export on a peer's
// page). A peer view carries JSON only, so it always answers ?format=json.
func servePeerDrawingBoardExport(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if !v.unrestricted() {
		b, err := db.GetCollabDrawingBoard(id)
		if err != nil || !peerDrawingBoardReads(v.peer.InstanceID, b) {
			writeError(w, http.StatusNotFound, "not_found", "drawing board not found")
			return
		}
		v.groupName = b.GroupName
	}
	v.auditDetail = "drawing board=" + id
	ctx, cancel := context.WithTimeout(r.Context(), peerDrawingBoardExportDeadline)
	defer cancel()
	serveDrawingBoardExportWith(w, r.WithContext(ctx), id, drawingBoardExportServe{asJSON: true, maxBytes: peerDrawingBoardExportMaxBytes})
}

// peerDrawingBoardAuthor is how a peer's edits are recorded on this node's drawing boards.
func peerDrawingBoardAuthor(v *peerView) string { return "operator@" + v.peer.InstanceID }

// peerDrawingBoardReads reports whether the peer reads the drawing board: either group slug
// in its group, as write implies read there (groupResourceSlugs).
func peerDrawingBoardReads(peer string, b *db.CollabDrawingBoard) bool {
	return peerDrawingBoardGroupAllows(peer, b, PermGroupsResourcesRead) || peerDrawingBoardGroupAllows(peer, b, PermGroupsResourcesWrite)
}

// peerDrawingBoardGroupAllows reports whether the peer holds slug in the drawing board's
// group: an active group, as a drawing board in no group is no peer's.
func peerDrawingBoardGroupAllows(peer string, b *db.CollabDrawingBoard, slug string) bool {
	if b == nil || b.GroupID == 0 {
		return false
	}
	g, err := db.GetAgentGroupByID(b.GroupID)
	if err != nil || g == nil || g.IsArchived() {
		return false
	}
	return fedPeerAllows(peer, g.ID, slug)
}

// peerHoldsDrawingBoardsSomewhere reports whether the peer reads drawing
// boards in any active group, so the list is its at all. It is the peer's
// grant, whoever on the peer asks: its operator, or an agent whose own node
// checked its peer= grant (agentPeerViewRoutes), as the board reads are.
func peerHoldsDrawingBoardsSomewhere(peer string) bool {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && (fedPeerAllows(peer, g.ID, PermGroupsResourcesRead) || fedPeerAllows(peer, g.ID, PermGroupsResourcesWrite)) {
			return true
		}
	}
	return false
}

func servePeerDrawingBoardList(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if !peerHoldsDrawingBoardsSomewhere(v.peer.InstanceID) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	peer := v.peer.InstanceID
	rec := httptest.NewRecorder()
	serveDrawingBoardList(rec, func(b *db.CollabDrawingBoard) bool { return peerDrawingBoardReads(peer, b) })
	if rec.Code != http.StatusOK {
		copyRecorded(w, rec)
		return
	}
	var out struct {
		DrawingBoards []drawingBoardSummaryJSON `json:"drawingBoards"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		writeError(w, http.StatusInternalServerError, "encode", "read the drawing board list")
		return
	}
	for i := range out.DrawingBoards {
		// Invited agents may be any group's.
		out.DrawingBoards[i].Participants = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

// copyRecorded writes a recorded response as it is.
func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, vals := range rec.Header() {
		w.Header()[k] = vals
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// servePeerDrawingBoardRead is serveDrawingBoardRead for a restricted peer: no presence, no
// invited agents, and the group's members only with its roster grant.
func servePeerDrawingBoardRead(w http.ResponseWriter, r *http.Request, peer string, b *db.CollabDrawingBoard) {
	since := int64(-1)
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since must be a drawing board version >= 0")
			return
		}
		since = n
	}
	elems, err := db.ListCollabDrawingBoardElements(b.ID, since)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	out := drawingBoardToJSON(b, elems)
	out.Presence, out.Participants = nil, []string{}
	if !peerDrawingBoardGroupAllows(peer, b, PermGroupsRosterRead) {
		out.GroupMembers = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// servePeerDrawingBoard serves one drawing board's read, edit, title and group move.
func servePeerDrawingBoard(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	unrestricted := v.unrestricted()
	// A drawing board this node's operator locked is not a remote operator's to
	// change, whatever its trust: it unlocks it first (groups.resources.lock).
	if r.Method != http.MethodGet && db.GroupResourceLocked(resourceKindDrawingBoard, r.PathValue("id")) {
		if !unrestricted {
			if b, err := db.GetCollabDrawingBoard(r.PathValue("id")); err != nil || !peerDrawingBoardReads(v.peer.InstanceID, b) {
				writeError(w, http.StatusNotFound, "not_found", "drawing board not found")
				return
			}
		}
		writeResourceLocked(w, "drawing board "+r.PathValue("id"))
		return
	}
	if unrestricted && r.Method != http.MethodPatch {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if unrestricted {
		serveDrawingBoardPatch(w, r, id, peerDrawingBoardAuthor(v), "", "")
		return
	}
	b, err := db.GetCollabDrawingBoard(id)
	peer := v.peer.InstanceID
	if err != nil || !peerDrawingBoardReads(peer, b) {
		// Whether a drawing board exists outside the peer's groups is not its to know.
		writeError(w, http.StatusNotFound, "not_found", "drawing board not found")
		return
	}
	v.groupName = b.GroupName
	if r.Method == http.MethodGet {
		servePeerDrawingBoardRead(w, r, peer, b)
		return
	}
	if !peerDrawingBoardGroupAllows(peer, b, PermGroupsResourcesWrite) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsResourcesWrite+" in the drawing board's group")
		return
	}
	v.auditDetail = "drawing board=" + id
	switch {
	case r.Method == http.MethodPatch:
		serveDrawingBoardPatch(w, r, id, peerDrawingBoardAuthor(v), "", "")
	case strings.HasSuffix(r.URL.Path, "/group"):
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
		var req struct {
			Group string `json:"group"`
		}
		if err != nil || json.Unmarshal(raw, &req) != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid group")
			return
		}
		// Unlinked, the drawing board would be no group's, so no peer's to have moved.
		to, err := db.GetAgentGroupByName(strings.TrimSpace(req.Group))
		if err != nil || to == nil || to.IsArchived() || !fedPeerAllows(peer, to.ID, PermGroupsResourcesWrite) {
			writeError(w, http.StatusForbidden, "permission", "a peer moves a drawing board only to another group it holds "+PermGroupsResourcesWrite+" in")
			return
		}
		peerServeDrawingBoardLocally(w, r, raw)
	default:
		peerServeDrawingBoardLocally(w, r, nil)
	}
}

// peerServeDrawingBoardLocally hands an authorized request to this node's own drawing board
// handler, with the body already read when raw is set.
func peerServeDrawingBoardLocally(w http.ResponseWriter, r *http.Request, raw []byte) {
	req := peerAsLocalRequest(r)
	if raw != nil {
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	}
	peerDashboardMux().ServeHTTP(w, req)
}

func servePeerDrawingBoardCreate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	var req struct {
		Group string `json:"group"`
	}
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &req) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid drawing board")
		return
	}
	if drawingBoards, err := db.ListCollabDrawingBoards(); err != nil || len(drawingBoards) >= drawingBoardMaxDrawingBoards-peerDrawingBoardReserve {
		writeError(w, http.StatusConflict, "too_many_drawing_boards", "this node keeps its last drawing board slots for its own operator and agents")
		return
	}
	if v.unrestricted() {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		serveDrawingBoardCreate(w, r, peerDrawingBoardAuthor(v), "")
		return
	}
	// The id comes from the title: one the peer names could say whether a
	// drawing board of that id exists in a group it cannot see.
	delete(fields, "id")
	if raw, err = json.Marshal(fields); err != nil {
		writeError(w, http.StatusInternalServerError, "encode", err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(strings.TrimSpace(req.Group))
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsResourcesWrite) {
		writeError(w, http.StatusForbidden, "permission", "a peer creates a drawing board in a group it holds "+PermGroupsResourcesWrite+" in")
		return
	}
	v.groupName = g.Name
	r.Body = io.NopCloser(bytes.NewReader(raw))
	serveDrawingBoardCreate(w, r, peerDrawingBoardAuthor(v), "")
}
