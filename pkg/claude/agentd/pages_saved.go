package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Pages saved with their group (tcl-1lth8e). A page shared in a group has a
// group_pages row that outlives its stage: closing the page (the operator's
// tab ×, the agent's `page close`, the owner retiring, the daemon stopping)
// keeps the row as closed, and the group can reopen it. Only an explicit
// delete removes it. A page shown privately has no group to keep it in and
// ends when it closes, as before.
//
// The stage token is never stored, so a reopened page gets a fresh origin; it
// keeps its id. A port page reopens only while its agent's dev server
// answers, and a static page only while its files are still inside the
// agent's directories: the source is proved again as a show proves it.
// Its marks and live patches are saved with it (pages_collab_saved.go).

const (
	pageClosedByOperator = "closed by the operator"
	pageClosedByAgent    = "closed by its agent"
	pageClosedRetired    = "its agent retired"
	pageClosedRestart    = "the daemon restarted"
	pageClosedArchived   = "the group was archived"
)

// pageLifetimeRule is the show rule that says what closing does to the page.
func pageLifetimeRule(group string) string {
	if group == "" {
		return "A private page ends when it closes or the daemon restarts; show again to bring it back."
	}
	return "The page is saved with group " + group + ": closed (by you, the operator or a daemon restart), it stays in the group, " +
		"and the operator's Reopen or your show of the same name reopens it with a new origin, keeping its marks and live patches."
}

// pageSaveMu orders every change to the live registry that a saved row
// follows with the row's write, so the two cannot interleave (a close landing
// between a show's registry change and its save). Taken before
// livePages.mu, never while holding it.
var pageSaveMu sync.Mutex

// savedPageRow is the row a live page is saved as. Caller holds livePages.mu
// or owns p.
func savedPageRow(p *livePage) db.GroupPage {
	return db.GroupPage{ID: p.id, GroupID: p.groupID, Owner: p.owner, Name: p.name, Title: p.title, Kind: p.kind,
		Target: p.target, Path: p.path, Worktree: p.worktree, Port: p.port, AgentsLocked: p.agentsLocked}
}

// savePageOpen records a page as open in its group, or forgets a saved row
// when the page is no longer shared in one. Best-effort: the live page works
// either way, and a failure is logged.
func savePageOpen(row db.GroupPage) {
	var err error
	if row.GroupID == 0 {
		_, err = db.DeleteGroupPage(row.ID)
	} else {
		err = db.SaveOpenGroupPage(row)
	}
	if err != nil {
		slog.Warn("pages: could not save page with its group", "page", row.ID, "error", err)
	}
}

// savePageClosed records that a saved page closed, and why.
func savePageClosed(id string, groupID int64, reason string) {
	if groupID == 0 {
		return
	}
	if err := db.CloseGroupPage(id, reason); err != nil {
		slog.Warn("pages: could not record a closed page", "page", id, "error", err)
	}
}

// closeSavedPagesAtStart marks the pages a previous daemon left open as
// closed: no stage serves them now, and their sources may be gone, so they
// reopen only when someone asks.
func closeSavedPagesAtStart() {
	if n, err := db.CloseOpenGroupPages(pageClosedRestart); err != nil {
		slog.Warn("pages: could not close the saved pages left open", "error", err)
	} else if n > 0 {
		slog.Info("pages: saved pages from the last run are closed and can be reopened", "pages", n)
	}
}

// savedPageFor is the page owner saved in groupID under name, if any: a show
// of that name reopens it (keeping its id), and an operator lock on it
// refuses a show that would replace it. Zero when there is none.
func savedPageFor(groupID int64, owner, name string) (id string, locked bool) {
	if groupID == 0 {
		return "", false
	}
	row, err := db.FindGroupPage(groupID, owner, name)
	if err != nil || row == nil {
		return "", false
	}
	return row.ID, row.AgentsLocked
}

// deletePagesForConv closes the pages of the agent behind convID and removes
// its saved ones: the agent is being deleted. Call it while the agent ID
// still resolves.
func deletePagesForConv(convID string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return
	}
	pageSaveMu.Lock()
	defer pageSaveMu.Unlock()
	livePages.closeOwner(agentID)
	if err := db.DeleteGroupPagesOwnedBy(agentID); err != nil {
		slog.Warn("pages: could not remove a deleted agent's saved pages", "agent", agentID, "error", err)
	}
}

// POST /api/pages/{id}/reopen — the operator reopens a closed saved page.
func handleDashboardPageReopen(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	livePages.mu.Lock()
	live := livePages.byID[id]
	var v pageView
	if live != nil {
		v = live.view()
	}
	livePages.mu.Unlock()
	if live != nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	row, err := db.GetGroupPage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such saved page")
		return
	}
	g, err := db.GetAgentGroupByID(row.GroupID)
	if err != nil || g == nil || !g.ArchivedAt.IsZero() {
		writeError(w, http.StatusConflict, "group_gone", "the page's group is archived or gone")
		return
	}
	setAuditTargetLabel(r, "page "+g.Name+"/"+row.Name)
	if !isGroupPageOwner(row.Owner) && !pageOwnerInGroup(row.Owner, row.GroupID) {
		writeError(w, http.StatusConflict, "not_member", "the agent that showed this page is no longer a member of "+g.Name+"; it cannot be reopened there")
		return
	}
	p, status, kind, msg := reopenSavedPage(r, row)
	if p == nil {
		writeError(w, status, kind, msg)
		return
	}
	v = p.view()
	saved := savedPageRow(p)
	livePages.mu.Unlock()
	savePageOpen(saved)
	pageSaveMu.Unlock()
	v.Group = g.Name
	if !isGroupPageOwner(row.Owner) {
		if convID := humanActionLiveConv(row.Owner); convID != "" {
			body := fmt.Sprintf("The operator reopened your live page %q from group %s. It has a new origin, and keeps its marks and live patches.",
				row.Name, g.Name)
			_, _, _ = queueRegularAgentMessage(&db.AgentMessage{
				ToConv: convID, Subject: "[page " + row.Name + "] reopened", Body: body,
				ToRecipients: []string{convID}, OperatorAuthored: true,
			})
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// reopenSavedPage builds a closed saved page's source again and opens it in
// the registry under its id. nil with why when it cannot. On success it
// returns holding pageSaveMu and livePages.mu, for the caller to save the
// row before releasing them.
func reopenSavedPage(r *http.Request, row *db.GroupPage) (*livePage, int, string, string) {
	owner := row.Owner
	var convID string
	var t pageTarget
	relay := &pageReloadRelay{}
	var up http.RoundTripper = stageScratch{}
	worktree, start := row.Worktree, row.Path
	if !isGroupPageOwner(owner) {
		a, err := db.GetAgent(owner)
		if err != nil || a == nil || !a.Active() || a.CurrentConvID == "" {
			return nil, http.StatusConflict, "owner_gone", "the agent that showed this page is retired or gone; it cannot be reopened"
		}
		convID = a.CurrentConvID
		if t, err = parsePageTarget(row.Target); err != nil {
			return nil, http.StatusConflict, "invalid_target", "the saved target is no longer valid: " + err.Error()
		}
		if up, worktree, start, err = pageSource(r.Context(), convID, owner, t, relay.fire); err != nil {
			return nil, http.StatusConflict, "target_unavailable", "the page's source is not available: " + err.Error()
		}
	}
	kept := loadSavedCollab(row.ID)
	now := time.Now().UTC()
	reg := livePages
	pageSaveMu.Lock()
	reg.mu.Lock()
	fail := func(status int, kind, msg string) (*livePage, int, string, string) {
		reg.mu.Unlock()
		pageSaveMu.Unlock()
		closeStageUpstream(up)
		return nil, status, kind, msg
	}
	if reg.byID[row.ID] != nil {
		return fail(http.StatusConflict, "exists", "the page is already open")
	}
	if reg.byOwner[owner][row.Name] != nil {
		return fail(http.StatusConflict, "exists", "another page named "+row.Name+" is open; close it first")
	}
	if len(reg.byOwner[owner]) >= pageMaxPerAgent || reg.count() >= pageMaxTotal {
		return fail(http.StatusConflict, "too_many_pages", "too many live pages are open; close one first")
	}
	reg.rev++
	kind := row.Kind
	if !isGroupPageOwner(owner) {
		kind = t.kind
	}
	p := &livePage{id: row.ID, owner: owner, name: row.Name, title: row.Title, target: row.Target, kind: kind,
		port: row.Port, path: start, worktree: worktree, state: pageStateOpen, rev: reg.rev, shownAt: now, updated: now,
		groupID: row.GroupID, agentsLocked: row.AgentsLocked}
	p.stage = stagePages.open(owner, row.Name, up)
	p.stage.static.Store(kind == pageKindStatic)
	relay.set(p.stage)
	if reg.byOwner[owner] == nil {
		reg.byOwner[owner] = map[string]*livePage{}
	}
	reg.byOwner[owner][row.Name] = p
	reg.byID[p.id] = p
	restoreSavedCollab(p.id, kept)
	return p, http.StatusOK, "", ""
}

// POST /api/pages/{id}/delete — the operator deletes a saved page: closed if
// open, and its row removed, so it no longer shows in the group.
func handleDashboardPageDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	pageSaveMu.Lock()
	p, wasLive := livePages.closeByID(id)
	removed, err := db.DeleteGroupPage(id)
	pageSaveMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if !wasLive && !removed {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	if wasLive {
		setAuditTargetLabel(r, "page "+p.name)
		if convID := humanActionLiveConv(p.owner); convID != "" {
			body := fmt.Sprintf("The operator deleted your live page %q.", p.name)
			_, _, _ = queueRegularAgentMessage(&db.AgentMessage{
				ToConv: convID, Subject: "[page " + p.name + "] deleted", Body: body,
				ToRecipients: []string{convID}, OperatorAuthored: true,
			})
		}
	} else {
		setAuditTargetLabel(r, "saved page "+id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// savedClosedPages are the closed saved pages of each group, for the group's
// Resources list next to the live ones.
func savedClosedPages() map[int64][]processGroupPage {
	out := map[int64][]processGroupPage{}
	rows, err := db.ListGroupPages()
	if err != nil {
		slog.Warn("pages: saved pages unavailable", "error", err)
		return out
	}
	livePages.mu.Lock()
	live := map[string]bool{}
	for id := range livePages.byID {
		live[id] = true
	}
	livePages.mu.Unlock()
	for _, row := range rows {
		// A row still marked open with no stage behind it (a write that
		// failed) is closed in all but name: it lists, and reopens, as closed.
		if live[row.ID] {
			continue
		}
		v := processGroupPage{ID: row.ID, Name: row.Name, Title: row.Title, Owner: row.Owner, Kind: row.Kind,
			AgentsLocked: row.AgentsLocked, State: db.GroupPageClosed, ClosedReason: row.ClosedReason}
		if !row.ClosedAt.IsZero() {
			v.ClosedAt = row.ClosedAt.UTC().Format(time.RFC3339)
		}
		if isGroupPageOwner(row.Owner) {
			v.Owner, v.GroupOwned = "", true
		}
		out[row.GroupID] = append(out[row.GroupID], v)
	}
	return out
}

// POST /api/pages/{id}/reload — the operator reloads a live page in every
// view of it, as a saved source file does.
func handleDashboardPageReload(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	livePages.mu.Lock()
	p := livePages.byID[r.PathValue("id")]
	var name string
	if p != nil {
		name = p.name
		p.stage.notifyReload()
	}
	livePages.mu.Unlock()
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such live page")
		return
	}
	setAuditTargetLabel(r, "page "+name)
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true})
}
