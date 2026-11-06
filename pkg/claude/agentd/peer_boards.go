package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Collab boards under peer grants. A board belongs to the group it is linked
// to, so a restricted peer reaches it through that group, by the group
// siblings of the global board slugs, as a local agent does:
// groups.resources.read lists and reads the boards of the groups it is held
// in, and groups.resources.write (which implies read there) edits their elements and title, creates a
// board in such a group, and moves one between two groups it holds the grant
// in. A board in no group is reached only through the global boards.* slugs,
// which no peer holds: it is no peer's.
// What the peer writes is authored as its operator (operator@<instance>),
// unrestricted trust's too, and never enters this node's undo history. A
// restricted peer's read carries neither the board's presence (who is on it,
// the local operator's view) nor its invited agents, and the group's members
// only with groups.roster.read there; a peer's new board takes its id from its
// title and leaves peerBoardReserve boards to this node. Unrestricted trust
// otherwise does all this as this node's operator does. Deleting a board, its
// live socket and presence, the operator's activity mail (events), undo, its
// rendered snapshot, inviting agents and ending their sessions stay with this
// node's own dashboard. A board this node's operator locked refuses every
// peer's change, as it does an agent's, until it is unlocked.

// peerBoardReserve is how many of the node's boards a peer's creates leave to
// this node's operator and agents.
const peerBoardReserve = 100

func addPeerBoardRules(rules map[string]peerViewRule) {
	read := peerViewRule{feature: "boards", requires: PermGroupsResourcesRead, orGrants: []string{PermGroupsResourcesWrite}, group: true}
	write := peerViewRule{feature: "boards.write", requires: PermGroupsResourcesWrite, group: true}
	list, one := read, read
	list.serve, one.serve = servePeerBoardList, servePeerBoard
	rules["GET /api/boards"] = list
	rules["GET /api/boards/{id}"] = one
	create, edit := write, write
	create.write, edit.write = servePeerBoardCreate, servePeerBoard
	rules["POST /api/boards"] = create
	for _, pattern := range []string{"PATCH /api/boards/{id}", "POST /api/boards/{id}/title", "POST /api/boards/{id}/group"} {
		rules[pattern] = edit
	}
}

// peerBoardAuthor is how a peer's edits are recorded on this node's boards.
func peerBoardAuthor(v *peerView) string { return "operator@" + v.peer.InstanceID }

// peerBoardReads reports whether the peer reads the board: either group slug
// in its group, as write implies read there (groupResourceSlugs).
func peerBoardReads(peer string, b *db.CollabBoard) bool {
	return peerBoardGroupAllows(peer, b, PermGroupsResourcesRead) || peerBoardGroupAllows(peer, b, PermGroupsResourcesWrite)
}

// peerBoardGroupAllows reports whether the peer holds slug in the board's
// group: an active group, as a board in no group is no peer's.
func peerBoardGroupAllows(peer string, b *db.CollabBoard, slug string) bool {
	if b == nil || b.GroupID == 0 {
		return false
	}
	g, err := db.GetAgentGroupByID(b.GroupID)
	if err != nil || g == nil || g.IsArchived() {
		return false
	}
	return fedPeerAllows(peer, g.ID, slug)
}

func servePeerBoardList(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if !peerOperatorHoldsInSomeGroup(v, PermGroupsResourcesRead) && !peerOperatorHoldsInSomeGroup(v, PermGroupsResourcesWrite) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	peer := v.peer.InstanceID
	rec := httptest.NewRecorder()
	serveBoardList(rec, func(b *db.CollabBoard) bool { return peerBoardReads(peer, b) })
	if rec.Code != http.StatusOK {
		copyRecorded(w, rec)
		return
	}
	var out struct {
		Boards []boardSummaryJSON `json:"boards"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		writeError(w, http.StatusInternalServerError, "encode", "read the board list")
		return
	}
	for i := range out.Boards {
		// Invited agents may be any group's.
		out.Boards[i].Participants = []string{}
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

// servePeerBoardRead is serveBoardRead for a restricted peer: no presence, no
// invited agents, and the group's members only with its roster grant.
func servePeerBoardRead(w http.ResponseWriter, r *http.Request, peer string, b *db.CollabBoard) {
	since := int64(-1)
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since must be a board version >= 0")
			return
		}
		since = n
	}
	elems, err := db.ListCollabBoardElements(b.ID, since)
	if err != nil {
		writeBoardStoreError(w, err)
		return
	}
	out := boardToJSON(b, elems)
	out.Presence, out.Participants = nil, []string{}
	if !peerBoardGroupAllows(peer, b, PermGroupsRosterRead) {
		out.GroupMembers = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// servePeerBoard serves one board's read, edit, title and group move.
func servePeerBoard(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	unrestricted := v.unrestricted()
	// A board this node's operator locked is not a remote operator's to
	// change, whatever its trust: it unlocks it first (groups.resources.lock).
	if r.Method != http.MethodGet && db.GroupResourceLocked(resourceKindBoard, r.PathValue("id")) {
		if !unrestricted {
			if b, err := db.GetCollabBoard(r.PathValue("id")); err != nil || !peerBoardReads(v.peer.InstanceID, b) {
				writeError(w, http.StatusNotFound, "not_found", "board not found")
				return
			}
		}
		writeResourceLocked(w, "board "+r.PathValue("id"))
		return
	}
	if unrestricted && r.Method != http.MethodPatch {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	if unrestricted {
		serveBoardPatch(w, r, id, peerBoardAuthor(v), "", "")
		return
	}
	b, err := db.GetCollabBoard(id)
	peer := v.peer.InstanceID
	if err != nil || !peerBoardReads(peer, b) {
		// Whether a board exists outside the peer's groups is not its to know.
		writeError(w, http.StatusNotFound, "not_found", "board not found")
		return
	}
	v.groupName = b.GroupName
	if r.Method == http.MethodGet {
		servePeerBoardRead(w, r, peer, b)
		return
	}
	if !peerBoardGroupAllows(peer, b, PermGroupsResourcesWrite) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsResourcesWrite+" in the board's group")
		return
	}
	v.auditDetail = "board=" + id
	switch {
	case r.Method == http.MethodPatch:
		serveBoardPatch(w, r, id, peerBoardAuthor(v), "", "")
	case strings.HasSuffix(r.URL.Path, "/group"):
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
		var req struct {
			Group string `json:"group"`
		}
		if err != nil || json.Unmarshal(raw, &req) != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid group")
			return
		}
		// Unlinked, the board would be no group's, so no peer's to have moved.
		to, err := db.GetAgentGroupByName(strings.TrimSpace(req.Group))
		if err != nil || to == nil || to.IsArchived() || !fedPeerAllows(peer, to.ID, PermGroupsResourcesWrite) {
			writeError(w, http.StatusForbidden, "permission", "a peer moves a board only to another group it holds "+PermGroupsResourcesWrite+" in")
			return
		}
		peerServeBoardLocally(w, r, raw)
	default:
		peerServeBoardLocally(w, r, nil)
	}
}

// peerServeBoardLocally hands an authorized request to this node's own board
// handler, with the body already read when raw is set.
func peerServeBoardLocally(w http.ResponseWriter, r *http.Request, raw []byte) {
	req := peerAsLocalRequest(r)
	if raw != nil {
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	}
	peerDashboardMux().ServeHTTP(w, req)
}

func servePeerBoardCreate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	var req struct {
		Group string `json:"group"`
	}
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &req) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid board")
		return
	}
	if boards, err := db.ListCollabBoards(); err != nil || len(boards) >= boardMaxBoards-peerBoardReserve {
		writeError(w, http.StatusConflict, "too_many_boards", "this node keeps its last board slots for its own operator and agents")
		return
	}
	if v.unrestricted() {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		serveBoardCreate(w, r, peerBoardAuthor(v), "")
		return
	}
	// The id comes from the title: one the peer names could say whether a
	// board of that id exists in a group it cannot see.
	delete(fields, "id")
	if raw, err = json.Marshal(fields); err != nil {
		writeError(w, http.StatusInternalServerError, "encode", err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(strings.TrimSpace(req.Group))
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsResourcesWrite) {
		writeError(w, http.StatusForbidden, "permission", "a peer creates a board in a group it holds "+PermGroupsResourcesWrite+" in")
		return
	}
	v.groupName = g.Name
	r.Body = io.NopCloser(bytes.NewReader(raw))
	serveBoardCreate(w, r, peerBoardAuthor(v), "")
}
