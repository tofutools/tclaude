package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
// What the peer writes is authored as its operator (operator@<instance>).
// Unrestricted trust does all this as this node's operator does. Deleting a
// board, its live socket and presence, the operator's activity mail
// (events), undo, its rendered snapshot, inviting agents and ending their
// sessions stay with this node's own dashboard.

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
	serveBoardList(w, func(b *db.CollabBoard) bool { return peerBoardReads(peer, b) })
}

// servePeerBoard serves one board's read, edit, title and group move.
func servePeerBoard(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
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
		serveBoardRead(w, r, id)
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
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	var req struct {
		Group string `json:"group"`
	}
	if err != nil || json.Unmarshal(raw, &req) != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid board")
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
