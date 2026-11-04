package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Renaming and moving a group's resources from its Resources rows
// (tcl-kostyh): the operator renames a page or a board, and moves a page to
// another group. A board moves through POST /api/boards/{id}/group.
//
// A page's title is the agent's to set too: an agent showing the page again
// with --title, or without one (which names it after its name), replaces the
// operator's. A page moves only to a group its agent belongs to, as its show
// would put it there, and like a re-show into another group it drops the
// marks sent before. A group's own blank page moves only while closed: open,
// it is staged under its group's key.

// pageTitleOK reports whether an operator's title may be a page's.
func pageTitleOK(title string) bool {
	return title != "" && len(title) <= pageMaxTitleBytes && utf8.ValidString(title) && !strings.ContainsFunc(title, unicode.IsControl)
}

// tellPageOwner tells the agent behind a page what the operator did to it.
func tellPageOwner(owner, name, subject, body string) {
	if isGroupPageOwner(owner) {
		return
	}
	if convID := humanActionLiveConv(owner); convID != "" {
		_, _, _ = queueRegularAgentMessage(&db.AgentMessage{
			ToConv: convID, Subject: "[page " + name + "] " + subject, Body: body,
			ToRecipients: []string{convID}, OperatorAuthored: true,
		})
	}
}

// POST /api/pages/{id}/title {title} — the operator renames a page, open or
// saved closed.
func handleDashboardPageTitle(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if !pageTitleOK(title) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the title must be printable and at most %d bytes", pageMaxTitleBytes))
		return
	}
	id := r.PathValue("id")
	pageSaveMu.Lock()
	livePages.mu.Lock()
	p := livePages.byID[id]
	if p != nil {
		livePages.rev++
		p.title, p.rev, p.updated = title, livePages.rev, time.Now().UTC()
		saved := savedPageRow(p)
		owner, name := p.owner, p.name
		livePages.mu.Unlock()
		if saved.GroupID != 0 {
			savePageOpen(saved)
		}
		pageSaveMu.Unlock()
		setAuditTargetLabel(r, "page "+name)
		tellPageOwner(owner, name, "renamed", fmt.Sprintf("The operator renamed your live page %q to %q. Showing it again sets the title again: --title, or its name without one.", name, title))
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "title": title})
		return
	}
	livePages.mu.Unlock()
	defer pageSaveMu.Unlock()
	row, err := db.GetGroupPage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	if err := db.SetGroupPageTitle(id, title); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	setAuditTargetLabel(r, "saved page "+row.Name)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "title": title})
}

// POST /api/pages/{id}/group {group} — the operator moves a page shared in a
// group to another group.
func handleDashboardPageGroup(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Group) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"group": "..."}`)
		return
	}
	groupID, ok := resolveBoardGroup(w, req.Group, "")
	if !ok {
		return
	}
	group := strings.TrimSpace(req.Group)
	id := r.PathValue("id")
	pageSaveMu.Lock()

	livePages.mu.Lock()
	if p := livePages.byID[id]; p != nil {
		owner, name, from := p.owner, p.name, p.groupID
		livePages.mu.Unlock()
		setAuditTargetLabel(r, "page "+name)
		refuse := func(status int, kind, msg string) {
			pageSaveMu.Unlock()
			writeError(w, status, kind, msg)
		}
		switch {
		case from == groupID:
			pageSaveMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "group": group})
			return
		case from == 0:
			refuse(http.StatusConflict, "private", "a private page is not shared in a group; its agent shows it into one")
			return
		case isGroupPageOwner(owner):
			refuse(http.StatusConflict, "close_first", "a group's blank page moves while closed: close it, then move it")
			return
		}
		if fail := pageMoveFailure(id, owner, name, groupID, group); fail != nil {
			refuse(fail.Status, fail.Kind, fail.Msg)
			return
		}
		livePages.mu.Lock()
		p = livePages.byID[id]
		if p == nil || p.groupID != from {
			livePages.mu.Unlock()
			refuse(http.StatusConflict, "changed", "the page changed meanwhile; try again")
			return
		}
		// Like a re-show into another group: the marks were sent to the
		// old group's audience, so they go before the new one can read it.
		pageCollab.mu.Lock()
		forgetPageMarksLocked(id)
		pageCollab.mu.Unlock()
		livePages.rev++
		p.groupID, p.rev, p.updated = groupID, livePages.rev, time.Now().UTC()
		saved := savedPageRow(p)
		livePages.mu.Unlock()
		savePageOpen(saved)
		pageSaveMu.Unlock()
		tellPageOwner(owner, name, "moved", fmt.Sprintf("The operator moved your live page %q to group %s. The marks sent before are dropped.", name, group))
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "group": group})
		return
	}
	livePages.mu.Unlock()
	defer pageSaveMu.Unlock()

	row, err := db.GetGroupPage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	setAuditTargetLabel(r, "saved page "+row.Name)
	if row.GroupID == groupID {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "group": group})
		return
	}
	owner := row.Owner
	if isGroupPageOwner(owner) {
		owner = groupPageOwner(groupID)
	} else if fail := pageMoveFailure(id, owner, row.Name, groupID, group); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if _, err := db.MoveGroupPage(id, groupID, owner); err != nil {
		if errors.Is(err, db.ErrGroupPageNameTaken) {
			writeError(w, http.StatusConflict, "exists", "group "+group+" already has a page named "+row.Name)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	pageCollab.mu.Lock()
	clearSavedMarksLocked(id)
	pageCollab.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "group": group})
}

// pageMoveFailure says why an agent's page cannot move to groupID, nil when
// it can: its agent must be a member there, and the group must not keep
// another page of the agent's under that name.
func pageMoveFailure(id, owner, name string, groupID int64, group string) *spawnFailure {
	if !pageOwnerInGroup(owner, groupID) {
		return &spawnFailure{http.StatusConflict, "not_member", "the agent that showed this page is not a member of " + group}
	}
	other, err := db.FindGroupPage(groupID, owner, name)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if other != nil && other.ID != id {
		return &spawnFailure{http.StatusConflict, "exists", "group " + group + " already has a page named " + name + " from that agent"}
	}
	return nil
}

// POST /api/boards/{id}/title {title} — the operator renames a board.
func handleDashboardBoardTitle(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > boardMaxTitleBytes || !boardPrintable(title, false) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("title must be printable text of at most %d bytes", boardMaxTitleBytes))
		return
	}
	if err := db.SetCollabBoardTitle(id, title); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	boardLive.publish(id, "")
	setAuditTargetLabel(r, "board "+id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "title": title})
}
