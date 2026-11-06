package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An operator-created page (tcl-20ihu3).
//
// The operator creates a page in a group's Collab space from the dashboard
// (POST /api/pages), as they create a board. No agent shows it, so the group
// owns it: it is keyed under groupPageOwner(groupID) instead of an agent id,
// and members address it as <group>/<name> (`tclaude agent page marks
// crew/sketch`) under the same groups.pages.* slugs as a page another member
// shared.
//
// What it serves until an agent takes it over is a fixed scratch page (kind
// scratch): a blank sheet the operator draws on and sends, saying what to
// build. A member takes it over with `tclaude agent show <target> --adopt
// <group>/<name>` (groups.pages.patch in the group): the page becomes that
// agent's, still shared in the group, with the same id, and the marks drawn
// on it are kept for the adopter to read. Its source changes kind, so it gets
// a fresh origin, as any page re-shown with another kind of source does;
// open views follow the snapshot to it. The operator's lock refuses a
// takeover as it refuses a re-show.

// pageKindScratch is an operator-created page no agent has taken over.
const pageKindScratch = "scratch"

const groupPageOwnerPrefix = "group:"

// groupPageOwner is the owner key of a group's own pages.
func groupPageOwner(groupID int64) string {
	return groupPageOwnerPrefix + strconv.FormatInt(groupID, 10)
}

// isGroupPageOwner reports whether owner is a group's key, not an agent's.
func isGroupPageOwner(owner string) bool {
	return strings.HasPrefix(owner, groupPageOwnerPrefix)
}

// pageOwnerInGroup reports whether a page's owner belongs to groupID now: an
// agent that is a current member, or the group itself.
func pageOwnerInGroup(owner string, groupID int64) bool {
	if groupID == 0 {
		return false
	}
	if isGroupPageOwner(owner) {
		g, err := db.GetAgentGroupByID(groupID)
		return owner == groupPageOwner(groupID) && err == nil && g != nil && g.ArchivedAt.IsZero()
	}
	return pageGroupOf(owner, groupID) != nil
}

// groupForCaller is the active group named name that caller is a member of.
func groupForCaller(caller, name string) *db.AgentGroup {
	groups, err := pageActiveGroups(caller)
	if err != nil {
		return nil
	}
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}

// pageScratchHTML is what a page serves before an agent takes it over. It is
// tclaude's own, fixed markup: no script, nothing from the operator or an
// agent.
const pageScratchHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Scratch page</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root { color-scheme: light dark; }
html, body { height: 100%; margin: 0; }
body { display: flex; align-items: center; justify-content: center; font: 15px/1.5 system-ui, sans-serif;
  color: #57606a; background: #ffffff; background-image: radial-gradient(#d0d7de 1px, transparent 1px); background-size: 20px 20px; }
@media (prefers-color-scheme: dark) { body { color: #8b949e; background-color: #0d1117; background-image: radial-gradient(#30363d 1px, transparent 1px); } }
main { max-width: 34em; padding: 24px; text-align: center; }
code { font-size: 13px; }
</style></head>
<body><main>
<p><b>A blank page for this group.</b></p>
<p>Draw on it and send, to show the group what to build here. A member takes it over with
<code>tclaude agent show &lt;target&gt; --adopt &lt;group&gt;/&lt;name&gt;</code>.</p>
</main></body></html>
`

// stageScratch serves pageScratchHTML at every path.
type stageScratch struct{}

func (stageScratch) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	status, body := http.StatusOK, pageScratchHTML
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		status, body = http.StatusMethodNotAllowed, ""
	}
	if req.Method == http.MethodHead {
		body = ""
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req,
	}, nil
}

// POST /api/pages {group, name, title?} — the operator creates a page in a
// group's Collab space. The dashboard opens it by the id it answers with.
func handleDashboardPageCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Group string `json:"group"`
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if !pageNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a page name is 1-48 characters of a-z, 0-9, - and _, starting with a letter or digit")
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > pageMaxTitleBytes || !utf8.ValidString(title) || strings.ContainsFunc(title, unicode.IsControl) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the title must be printable and at most %d bytes", pageMaxTitleBytes))
		return
	}
	if title == "" {
		title = name
	}
	g, err := db.GetAgentGroupByName(strings.TrimSpace(req.Group))
	if err != nil || g == nil || !g.ArchivedAt.IsZero() {
		writeError(w, http.StatusNotFound, "not_found", "no such active group")
		return
	}
	owner := groupPageOwner(g.ID)
	// A closed page the group saved under this name is reopened, keeping its
	// id and lock (pages_saved.go).
	savedID, savedLocked := savedPageFor(g.ID, owner, name)
	now := time.Now().UTC()
	reg := livePages
	pageSaveMu.Lock()
	defer pageSaveMu.Unlock()
	reg.mu.Lock()
	if reg.byOwner[owner][name] != nil {
		reg.mu.Unlock()
		writeError(w, http.StatusConflict, "exists", "the group already has a page named "+name)
		return
	}
	if len(reg.byOwner[owner]) >= pageMaxPerAgent || reg.count() >= pageMaxTotal {
		reg.mu.Unlock()
		writeError(w, http.StatusConflict, "too_many_pages", "too many live pages are open")
		return
	}
	reg.rev++
	p := &livePage{id: newPageID(), owner: owner, name: name, title: title, target: "scratch", kind: pageKindScratch,
		path: "/", state: pageStateOpen, rev: reg.rev, shownAt: now, updated: now, groupID: g.ID, agentsLocked: savedLocked}
	if savedID != "" && reg.byID[savedID] == nil {
		p.id = savedID
	}
	p.stage = stagePages.open(owner, name, stageScratch{})
	if reg.byOwner[owner] == nil {
		reg.byOwner[owner] = map[string]*livePage{}
	}
	reg.byOwner[owner][name] = p
	reg.byID[p.id] = p
	v := p.view()
	saved := savedPageRow(p)
	reg.mu.Unlock()
	savePageOpen(saved)
	v.Group = g.Name
	setAuditTargetLabel(r, "page "+g.Name+"/"+name)
	writeJSON(w, http.StatusCreated, v)
}

// pageAdoptTarget resolves a show's --adopt <group>/<name>: the group, which
// the caller must be a current member of and hold groups.pages.patch in. The
// page keeps its name, so a different --name is refused, as is --group or
// --private. nil without --adopt.
func pageAdoptTarget(w http.ResponseWriter, r *http.Request, agentID string, req pageShowRequest, name *string) (*db.AgentGroup, bool) {
	ref := strings.TrimSpace(req.Adopt)
	if ref == "" {
		return nil, true
	}
	if peerActionFromRequest(r) != nil {
		writeError(w, http.StatusForbidden, "local_only", "a group's page is reachable only by local members of its group")
		return nil, false
	}
	groupName, pageName, ok := strings.Cut(ref, "/")
	if !ok || !pageNamePattern.MatchString(pageName) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "--adopt takes <group>/<name>")
		return nil, false
	}
	if (*name != "" && *name != pageName) || strings.TrimSpace(req.Group) != "" || req.Private {
		writeError(w, http.StatusBadRequest, "invalid_arg", "an adopted page keeps its name and group: leave out --name, --group and --private")
		return nil, false
	}
	g := groupForCaller(agentID, groupName)
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no page "+ref+" is shared with you")
		return nil, false
	}
	if _, ok := requirePermission(w, r, PermGroupsPagesPatch, ActionContext{Group: g.Name}); !ok {
		return nil, false
	}
	*name = pageName
	return g, true
}

// adoptGroupPageLocked finds the group's page an adopting show takes over,
// or says why not (refused is then non-nil and writes the answer). The
// adopter may not already have a page by that name. held is the locked pages
// the adopter holds the lock slug for (pageLocksHeld). Caller holds reg.mu.
func adoptGroupPageLocked(w http.ResponseWriter, reg *livePageRegistry, g *db.AgentGroup, agentID, name string, mine *livePage, held map[string]bool) (*livePage, func()) {
	gp := reg.byOwner[groupPageOwner(g.ID)][name]
	switch {
	case gp == nil:
		return nil, func() {
			writeError(w, http.StatusNotFound, "not_found", "no page "+g.Name+"/"+name+" is shared with you")
		}
	case gp.agentsLocked && !held[gp.id]:
		return nil, func() { writeResourceLocked(w, "page "+g.Name+"/"+name) }
	case mine != nil:
		return nil, func() {
			writeError(w, http.StatusConflict, "exists", "you already have a page named "+name+"; close it first with `tclaude agent page close "+name+"`")
		}
	case len(reg.byOwner[agentID]) >= pageMaxPerAgent:
		return nil, func() {
			writeError(w, http.StatusConflict, "too_many_pages", fmt.Sprintf("you have %d pages open, the most an agent may", pageMaxPerAgent))
		}
	}
	return gp, nil
}

// closeGroupPages closes the pages a group owns, when it is archived or
// deleted: no member can reach them any more.
func closeGroupPages(groupID int64) {
	pageSaveMu.Lock()
	closed := livePages.closeOwner(groupPageOwner(groupID))
	for _, row := range closed {
		savePageClosed(row.ID, row.GroupID, pageClosedArchived)
	}
	pageSaveMu.Unlock()
	if len(closed) > 0 {
		slog.Info("closed a removed group's pages", "group_id", groupID, "pages", len(closed))
	}
}
