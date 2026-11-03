package agentd

// Live pages, the agent-facing core (epic tcl-qo90av, ticket tcl-fcb93a):
// `tclaude agent show <target>` and `tclaude agent page ls|close`, the
// daemon state behind them, and the operator's close from the dashboard.
//
//   - A page belongs to the agent that showed it, by agent ID (so it survives
//     reincarnation and ends with retirement). Every /v1/pages route resolves
//     the page by name inside the caller's own pages, so another agent's page
//     is not addressable at all: an agent can act only on pages it showed.
//   - The state is in memory only. A page is a live stage origin whose token
//     admits the operator's frame; keeping tokens off disk means a daemon
//     restart (or a page close) ends every origin for good.
//   - The target is parsed and validated here, before anything is dialled:
//     its own port (":5173/path", "http://localhost:5173/path"), or a static
//     directory or HTML file inside its startup directory or worktree. A port
//     source is proved (owned by the agent, or inside its sandbox network)
//     once at show time, so a dev server that is not up, or listens only on
//     ::1, is refused with a reason instead of showing a broken tab.
//   - Showing again with the same name updates the page in place when the
//     source kind is unchanged (same origin, new upstream, reload). A change
//     of kind (port <-> static) mints a new origin: page code that read the
//     static files must not keep an origin that now talks to the agent's port.
//   - Page tokens reach only the local dashboard (the snapshot). They never go
//     to a peer's snapshot or to any agent, the owner included: the stage
//     admits Origin: null, so the token is the gate, and an agent holding it
//     could fetch its page itself, reading files through the daemon's view of
//     the file system rather than its sandbox's (tcl-kxiqqd). Agents see the
//     origin with the token masked, and act on pages by name.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	pageKindPort   = "port"
	pageKindStatic = "static"

	pageStateOpen = "open"

	pageMaxPerAgent   = 8
	pageMaxTotal      = 64
	pageMaxTitleBytes = 120
	pageMaxTarget     = 1024
	pageMaxBody       = 8 << 10
	pageProbeTimeout  = 5 * time.Second
)

var pageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// pageIPv4Hint is the remedy for a dev server the stage cannot find on
// 127.0.0.1: the ownership proof and the dial are IPv4-only by design.
const pageIPv4Hint = "The stage connects only to 127.0.0.1. If your dev server listens on ::1 only " +
	"(some `localhost` setups), start it with `--host 127.0.0.1` or the equivalent"

// livePage is one page an agent showed.
type livePage struct {
	id       string // public page id, not a secret (dashboard routes)
	owner    string // agent ID
	name     string
	title    string
	target   string // as declared, normalised
	kind     string
	port     int
	path     string // the frame's start path (with query)
	worktree string // the root a static target lives in; the agent's worktree for a port
	state    string
	rev      int64 // bumped on every show of this page: the dashboard's "shown/updated" event
	shownAt  time.Time
	updated  time.Time
	stage    *stagePage
	// groupID is the agent group the page is shared in, 0 for none
	// (pages_share.go). Guarded by livePages.mu.
	groupID int64
	// agentsLocked is the operator's lock (group_resource_locks.go): no
	// agent, not even the shower, patches, evals, closes or re-shows the
	// page while it is set. Guarded by livePages.mu.
	agentsLocked bool
}

type livePageRegistry struct {
	mu      sync.Mutex
	byOwner map[string]map[string]*livePage
	byID    map[string]*livePage
	rev     int64
}

var livePages = &livePageRegistry{byOwner: map[string]map[string]*livePage{}, byID: map[string]*livePage{}}

func newPageID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("pages: crypto/rand: " + err.Error())
	}
	return "pg_" + hex.EncodeToString(b)
}

func (r *livePageRegistry) count() int {
	n := 0
	for _, m := range r.byOwner {
		n += len(m)
	}
	return n
}

// remove forgets a page and closes its stage origin. Caller holds r.mu.
func (r *livePageRegistry) removeLocked(p *livePage) {
	r.detachLocked(p)
	forgetPageCollab(p.id)
}

// detachLocked is removeLocked keeping the page's marks: a takeover's new
// page takes the same id (pages_group.go). Caller holds r.mu.
func (r *livePageRegistry) detachLocked(p *livePage) {
	if m := r.byOwner[p.owner]; m != nil && m[p.name] == p {
		delete(m, p.name)
		if len(m) == 0 {
			delete(r.byOwner, p.owner)
		}
	}
	delete(r.byID, p.id)
	stagePages.close(p.stage.token)
}

func (r *livePageRegistry) list(owner string) []*livePage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*livePage
	if owner == "" {
		for _, m := range r.byOwner {
			for _, p := range m {
				out = append(out, p)
			}
		}
	} else {
		for _, p := range r.byOwner[owner] {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b *livePage) int {
		if c := a.shownAt.Compare(b.shownAt); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

// closeByName closes one of owner's pages. It reports whether one existed.
func (r *livePageRegistry) closeByName(owner, name string) (*livePage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.byOwner[owner][name]
	if p == nil {
		return nil, false
	}
	r.removeLocked(p)
	return p, true
}

func (r *livePageRegistry) closeByID(id string) (*livePage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.byID[id]
	if p == nil {
		return nil, false
	}
	r.removeLocked(p)
	return p, true
}

// closeOwner closes every page an agent showed (its retirement).
func (r *livePageRegistry) closeOwner(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pages := r.byOwner[owner]
	n := 0
	for _, p := range pages {
		r.removeLocked(p)
		n++
	}
	return n
}

// closeLivePagesForConv closes the pages of the agent behind convID. It is
// called when the agent retires.
func closeLivePagesForConv(convID string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return
	}
	livePages.closeOwner(agentID)
}

// ---- targets ----

// pageTarget is a parsed, validated show target. Nothing has been dialled or
// opened yet.
type pageTarget struct {
	kind     string
	port     int
	path     string // port: the start path; static: the file system path
	declared string
}

// parsePageTarget validates what an agent may show. Port targets name only a
// port: no host name or IP is ever taken from the agent (localhost and
// 127.0.0.1 are spellings of "my own port"). Anything else is a path.
func parsePageTarget(raw string) (pageTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return pageTarget{}, errors.New("a target is required: :PORT[/path], http://localhost:PORT/..., a directory or an HTML file")
	}
	if len(raw) > pageMaxTarget || !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsControl) {
		return pageTarget{}, fmt.Errorf("the target must be printable and at most %d bytes", pageMaxTarget)
	}
	if strings.HasPrefix(raw, ":") {
		portPart, rest, _ := strings.Cut(raw[1:], "/")
		start := "/"
		if strings.Contains(raw[1:], "/") {
			start = "/" + rest
		}
		return portTarget(portPart, start)
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return pageTarget{}, fmt.Errorf("the target is not a valid URL: %v", err)
		}
		if u.Scheme != "http" {
			return pageTarget{}, errors.New("a URL target must be http://localhost:PORT/path")
		}
		if u.User != nil {
			return pageTarget{}, errors.New("a URL target cannot carry credentials")
		}
		host := strings.ToLower(u.Hostname())
		switch host {
		case "localhost", "127.0.0.1":
		case "::1":
			return pageTarget{}, errors.New("the stage does not connect to ::1. " + pageIPv4Hint)
		default:
			return pageTarget{}, errors.New("a page shows only your own port or files, never a host name or IP: use :PORT or http://localhost:PORT")
		}
		if u.Port() == "" {
			return pageTarget{}, errors.New("a URL target needs an explicit port: http://localhost:PORT/path")
		}
		start := u.EscapedPath()
		if start == "" {
			start = "/"
		}
		if u.RawQuery != "" {
			start += "?" + u.RawQuery
		}
		return portTarget(u.Port(), start)
	}
	return pageTarget{kind: pageKindStatic, path: raw, declared: raw}, nil
}

func portTarget(portPart, start string) (pageTarget, error) {
	port, err := strconv.Atoi(portPart)
	if err != nil || port <= 0 || port > 65535 || strconv.Itoa(port) != portPart {
		return pageTarget{}, fmt.Errorf("%q is not a port", portPart)
	}
	if isStageDaemonPort(port) {
		return pageTarget{}, fmt.Errorf("port %d is the daemon's own", port)
	}
	if !pageStartPathOK(start) {
		return pageTarget{}, errors.New("the path must be a plain URL path, such as /app?x=1")
	}
	declared := ":" + portPart
	if start != "/" {
		declared += start
	}
	return pageTarget{kind: pageKindPort, port: port, path: start, declared: declared}, nil
}

// pageStartPathOK accepts an absolute URL path (and query) that stays a path
// when appended to the page's origin.
func pageStartPathOK(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\#") {
		return false
	}
	for _, c := range p {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	// No dot segments, spelled any way: a browser resolves them before it
	// asks, so /p/<token>/../../api would leave the page's path.
	pathPart, _, _ := strings.Cut(p, "?")
	decoded, err := url.PathUnescape(pathPart)
	return err == nil && !stageHasDotSegment(decoded)
}

// pageRoots are the directories a static target may live in: the agent's
// startup directory, which the operator's launch chose. Never the edit
// directory the agent's hooks report: an agent steers that with any file path
// it touches, even one its sandbox refused.
var pageRoots = func(convID string) []string {
	loc := agent.ResolveLocation(convID)
	var roots []string
	for _, d := range []string{loc.StartupDir} {
		if d = strings.TrimSpace(d); d != "" && filepath.IsAbs(d) && !slices.Contains(roots, filepath.Clean(d)) {
			roots = append(roots, filepath.Clean(d))
		}
	}
	return roots
}

// pageWithin reports whether target is root or under it, compared lexically
// and through root's canonical spelling. The static source re-checks the walk
// itself without following any link.
func pageWithin(root, target string) bool {
	in := func(r string) bool {
		rel, err := filepath.Rel(r, target)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if in(root) {
		return true
	}
	if canon, err := filepath.EvalSymlinks(root); err == nil && canon != root {
		return in(canon)
	}
	return false
}

// pageStaticRoot resolves a static target against the agent's roots and
// returns the root it lives in and its absolute path.
func pageStaticRoot(roots []string, target string) (root, abs string, err error) {
	if len(roots) == 0 {
		return "", "", errors.New("you have no recorded worktree to serve files from")
	}
	abs = target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(roots[0], abs)
	}
	abs = filepath.Clean(abs)
	best := ""
	for _, r := range roots {
		if pageWithin(r, abs) && len(r) > len(best) {
			best = r
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("%s is outside your worktree (%s)", abs, strings.Join(roots, " or "))
	}
	return best, abs, nil
}

func pageHTMLFile(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".html" || ext == ".htm"
}

// ---- show ----

type pageShowRequest struct {
	Target string `json:"target"`
	Title  string `json:"title"`
	Name   string `json:"name"`
	// Group names the group to share the page in; "" is the shower's only
	// group. Private shares it with no one.
	Group   string `json:"group,omitempty"`
	Private bool   `json:"private,omitempty"`
	// Adopt takes over a page its group owns, as <group>/<name>
	// (pages_group.go). The page keeps its name and group.
	Adopt string `json:"adopt,omitempty"`
}

// pageView is a page as the dashboard sees it (its origin carries the token)
// or, through agentView, as its owner sees it (token masked).
type pageView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Title    string    `json:"title"`
	Owner    string    `json:"owner"`
	Target   string    `json:"target"`
	Kind     string    `json:"kind"`
	Worktree string    `json:"worktree,omitempty"`
	State    string    `json:"state"`
	Origin   string    `json:"origin"`
	URL      string    `json:"url"`
	Rev      int64     `json:"rev"`
	ShownAt  time.Time `json:"shown_at"`
	Updated  time.Time `json:"updated_at"`
	// RemoteBase is, for a dev server page, the --base it needs to be
	// served to a dashboard on another machine (remote_stage.go).
	RemoteBase string `json:"remote_base,omitempty"`
	// GroupID and Group are the agent group the page is shared in, and its
	// current name (pages_share.go).
	GroupID int64  `json:"group_id,omitempty"`
	Group   string `json:"group,omitempty"`
	// GroupOwned marks a page the operator created in a group that no agent
	// has taken over: Owner is empty, and members address it as
	// <group>/<name> (pages_group.go).
	GroupOwned bool `json:"group_owned,omitempty"`
	// AgentsLocked is the operator's lock on the page (group_resource_locks.go).
	AgentsLocked bool `json:"agents_locked,omitempty"`
	// ShareNote says, in show's answer, why a page is not shared.
	ShareNote string `json:"share_note,omitempty"`
}

type pageShowResponse struct {
	pageView
	// Created is true for a new page, false for one updated in place.
	Created bool `json:"created"`
	// NewOrigin is true when an existing name was re-shown with another
	// source kind and so got a fresh origin.
	NewOrigin bool     `json:"new_origin,omitempty"`
	Rules     []string `json:"rules"`
}

// pageOrigin is the page's stage origin on this daemon's dashboard port, or
// "" when the dashboard is not listening.
func pageOrigin(token string) string {
	u, err := url.Parse(popupBaseURL)
	if err != nil || u.Port() == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(stageHost(token), u.Port())
}

func (p *livePage) view() pageView {
	origin := pageOrigin(p.stage.token)
	u := ""
	if origin != "" {
		u = origin + p.path
	}
	owner, groupOwned := p.owner, isGroupPageOwner(p.owner)
	if groupOwned {
		owner = ""
	}
	return pageView{
		ID: p.id, Name: p.name, Title: p.title, Owner: owner, GroupOwned: groupOwned, Target: p.target, Kind: p.kind,
		Worktree: p.worktree, State: p.state, Origin: origin, URL: u, Rev: p.rev,
		ShownAt: p.shownAt, Updated: p.updated, GroupID: p.groupID,
		AgentsLocked: p.agentsLocked,
	}
}

// pageMaskedHost stands in for the token in what an agent is shown.
const pageMaskedHost = stageHostPrefix + "<token>." + stageHostSuffix

// agentView is the page as its owning agent sees it: everything but the key.
func (p *livePage) agentView() pageView {
	v := p.view()
	if u, err := url.Parse(v.Origin); err == nil && v.Origin != "" {
		v.Origin = "http://" + net.JoinHostPort(pageMaskedHost, u.Port())
	}
	v.URL = ""
	if p.kind == pageKindPort && p.stage != nil {
		v.RemoteBase = remoteStagePrefix + p.stage.remoteToken + "/"
	}
	return v
}

// pageRules are printed with every show, so the agent knows what its page can
// and cannot do and what the operator's typing exposes.
func pageRules(kind, group string) []string {
	typed := "Anything the operator types into the page can be read by you, the agent that showed it."
	if group != "" {
		typed = "The page is shared in group " + group + ": anything the operator types into it can be read by you and by every member of that group, " +
			"who can also snapshot it, read its DOM and live-patch it (show with --private to keep it to yourself)."
	}
	rules := []string{
		"The page runs in a sandboxed frame on its own origin: no dashboard cookies, storage or API reach it, and it can fetch only its own origin.",
		"Cookies, IndexedDB and service workers do not work in the frame; an app behind a login needs a dev bypass or a fixture user.",
		typed,
		"The page's token is its only key and goes only to the operator's dashboard; you act on the page by name.",
		"Pages end when the daemon restarts; show again to bring one back.",
		"Show again with the same --name to update this page in place; `tclaude agent page close <name>` closes it.",
	}
	if kind == pageKindStatic {
		rules = append(rules, "Files are served read-only with no symbolic links or dot-names; a saved file reloads the page.")
	} else {
		rules = append(rules, "The dev server sees itself addressed as http://localhost:PORT; its HMR websocket passes through.")
	}
	return rules
}

func pageDefaultName(t pageTarget) string {
	var base string
	if t.kind == pageKindPort {
		base = "port-" + strconv.Itoa(t.port)
	} else {
		base = strings.TrimSuffix(filepath.Base(filepath.Clean(t.path)), filepath.Ext(t.path))
		if base == "index" {
			base = filepath.Base(filepath.Dir(filepath.Clean(t.path)))
		}
	}
	var b strings.Builder
	for _, c := range strings.ToLower(base) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > 48 {
		name = strings.Trim(name[:48], "-_")
	}
	if !pageNamePattern.MatchString(name) {
		return "page"
	}
	return name
}

// pageCaller resolves the calling agent. A page belongs to an agent, so a
// human caller (no conversation) is refused.
func pageCaller(w http.ResponseWriter, r *http.Request) (convID, agentID string, ok bool) {
	convID, ok = requirePermission(w, r, PermSelfPage)
	if !ok {
		return "", "", false
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "no_agent", "a live page belongs to an agent; this caller is not one")
		return "", "", false
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		writeError(w, http.StatusForbidden, "unknown_agent", "the calling conversation has no agent identity")
		return "", "", false
	}
	// Only the agent's current generation: a predecessor still running after
	// a reincarnation must not read the pages' origins or change them.
	if cur, err := db.CurrentConvForAgent(agentID); err != nil || cur != convID {
		writeError(w, http.StatusForbidden, "not_current", "only the agent's current conversation can act on its live pages")
		return "", "", false
	}
	return convID, agentID, true
}

// pageSource builds the upstream for a target, proving a port source once
// before anything is shown. onChange is the reload signal of the page that
// will hold it.
func pageSource(ctx context.Context, convID, agentID string, t pageTarget, onChange func()) (up http.RoundTripper, worktree, start string, err error) {
	switch t.kind {
	case pageKindPort:
		src, err := newStageAgentPort(agentID, t.port)
		if err != nil {
			return nil, "", "", err
		}
		pctx, cancel := context.WithTimeout(ctx, pageProbeTimeout)
		defer cancel()
		conn, err := src.dialTarget(pctx, agentID)
		if err != nil {
			_ = src.Close()
			msg := stageSourceMessage(err)
			if msg == "" {
				msg = fmt.Sprintf("cannot reach :%d", t.port)
			}
			return nil, "", "", fmt.Errorf("%s. %s", msg, pageIPv4Hint)
		}
		_ = conn.Close()
		if roots := pageRoots(convID); len(roots) > 0 {
			worktree = roots[0]
		}
		return src, worktree, t.path, nil
	case pageKindStatic:
		root, abs, err := pageStaticRoot(pageRoots(convID), t.path)
		if err != nil {
			return nil, "", "", err
		}
		start = "/"
		if st, err := os.Lstat(abs); err == nil && st.Mode().IsRegular() {
			if !pageHTMLFile(abs) {
				return nil, "", "", errors.New("a single-file page must be an .html file; to serve other files, show their directory")
			}
			start = "/" + path.Base(filepath.ToSlash(abs))
		}
		viewer := newPageFSViewer(agentID, convID, root)
		viewer.onChange = onChange
		src, err := newStageStatic(root, abs, onChange, viewer)
		if err != nil {
			return nil, "", "", err
		}
		return src, root, start, nil
	}
	return nil, "", "", errors.New("unknown target kind")
}

// POST /v1/pages {target, title?, name?} — show a page, or update the
// caller's page of that name in place.
func handlePageShow(w http.ResponseWriter, r *http.Request) {
	convID, agentID, ok := pageCaller(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pageMaxBody)
	var req pageShowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	t, err := parsePageTarget(req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	adopt, ok := pageAdoptTarget(w, r, agentID, req, &name)
	if !ok {
		return
	}
	if name == "" {
		name = pageDefaultName(t)
	}
	if !pageNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a page name is 1-48 characters of a-z, 0-9, - and _, starting with a letter or digit")
		return
	}
	groupID, shareNote, err := pageShareGroup(agentID, req.Group, req.Private)
	if adopt != nil {
		groupID, shareNote, err = adopt.ID, "", nil
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_group", err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > pageMaxTitleBytes || !utf8.ValidString(title) || strings.ContainsFunc(title, unicode.IsControl) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the title must be printable and at most %d bytes", pageMaxTitleBytes))
		return
	}

	// The source is built (and a port proved) outside the registry lock: a
	// sandboxed dial can take a moment. A static source's watcher is wired to
	// a reload relay that is pointed at the page once it is known.
	relay := &pageReloadRelay{}
	up, worktree, start, err := pageSource(r.Context(), convID, agentID, t, relay.fire)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "target_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	reg := livePages
	reg.mu.Lock()
	existing := reg.byOwner[agentID][name]
	if adopt != nil {
		var refused func()
		existing, refused = adoptGroupPageLocked(w, reg, adopt, agentID, name, existing)
		if refused != nil {
			reg.mu.Unlock()
			closeStageUpstream(up)
			refused()
			return
		}
	}
	if existing != nil && existing.agentsLocked {
		reg.mu.Unlock()
		closeStageUpstream(up)
		writeResourceLocked(w, "page "+name)
		return
	}
	if existing == nil {
		if len(reg.byOwner[agentID]) >= pageMaxPerAgent {
			reg.mu.Unlock()
			closeStageUpstream(up)
			writeError(w, http.StatusConflict, "too_many_pages", fmt.Sprintf("you have %d pages open, the most an agent may; close one with `tclaude agent page close <name>`", pageMaxPerAgent))
			return
		}
		if reg.count() >= pageMaxTotal {
			reg.mu.Unlock()
			closeStageUpstream(up)
			writeError(w, http.StatusConflict, "too_many_pages", "the daemon has too many live pages open")
			return
		}
	}
	// A page re-shown into another group (or out of one) changes who reads
	// what the operator typed into it: the marks sent before are dropped.
	regrouped := existing != nil && existing.groupID != groupID
	reg.rev++
	resp := pageShowResponse{}
	var p *livePage
	switch {
	case existing != nil && existing.kind == t.kind:
		// Same kind: same origin, new upstream, and open views reload.
		p = existing
		relay.set(p.stage)
		p.stage.setUpstream(up)
		p.stage.notifyReload()
	default:
		p = &livePage{id: newPageID(), owner: agentID, name: name, shownAt: now}
		if existing != nil {
			// Another kind of source: a fresh origin (and token), so nothing
			// the old origin's code holds can reach the new source.
			p.id, p.shownAt = existing.id, existing.shownAt
			if adopt != nil {
				// A takeover keeps what the operator drew on the page.
				reg.detachLocked(existing)
			} else {
				reg.removeLocked(existing)
			}
			resp.NewOrigin = true
		}
		p.stage = stagePages.open(agentID, name, up)
		p.stage.static.Store(t.kind == pageKindStatic)
		relay.set(p.stage)
		if reg.byOwner[agentID] == nil {
			reg.byOwner[agentID] = map[string]*livePage{}
		}
		reg.byOwner[agentID][name] = p
		reg.byID[p.id] = p
		resp.Created = existing == nil
	}
	if title == "" {
		title = name
	}
	p.title, p.target, p.kind, p.port, p.worktree = title, t.declared, t.kind, t.port, worktree
	p.path, p.groupID = start, groupID
	p.state, p.rev, p.updated = pageStateOpen, reg.rev, now
	resp.pageView = p.agentView()
	reg.mu.Unlock()
	resp.Group = pageGroupName(resp.GroupID)
	resp.ShareNote = shareNote
	if regrouped {
		forgetPageMarks(p.id)
	}

	resp.Rules = pageRules(t.kind, resp.Group)
	setAuditTargetLabel(r, "page "+name)
	status := http.StatusOK
	if resp.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

// pageReloadRelay forwards a static source's change signal to the page that
// ends up holding it. Signals before the page is known are dropped: the page
// loads fresh anyway.
type pageReloadRelay struct {
	mu   sync.Mutex
	page *stagePage
}

func (r *pageReloadRelay) set(p *stagePage) {
	r.mu.Lock()
	r.page = p
	r.mu.Unlock()
}

func (r *pageReloadRelay) fire() {
	r.mu.Lock()
	p := r.page
	r.mu.Unlock()
	if p != nil {
		p.notifyReload()
	}
}

// GET /v1/pages — the caller's own pages, and those other members of its
// groups showed and share with it (shared; never their remote base).
func handlePageList(w http.ResponseWriter, r *http.Request) {
	convID, agentID, ok := pageCaller(w, r)
	if !ok {
		return
	}
	shared := []pageView{}
	if peerActionFromRequest(r) == nil {
		var readable []*livePage
		allowed := map[int64]bool{}
		for _, p := range pagesSharedWith(agentID) {
			livePages.mu.Lock()
			id := p.groupID
			livePages.mu.Unlock()
			ok, seen := allowed[id]
			if !seen {
				ok, _, _ = permissionAllowsAction(r, convID, PermGroupsPagesRead, ActionContext{Group: pageGroupName(id)})
				allowed[id] = ok
			}
			if ok {
				readable = append(readable, p)
			}
		}
		shared = pageViews(readable, func(p *livePage) pageView {
			v := p.agentView()
			v.RemoteBase = ""
			return v
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pages":  pageViews(livePages.list(agentID), (*livePage).agentView),
		"shared": shared,
	})
}

func pageViews(pages []*livePage, view func(*livePage) pageView) []pageView {
	livePages.mu.Lock()
	out := make([]pageView, 0, len(pages))
	for _, p := range pages {
		out = append(out, view(p))
	}
	livePages.mu.Unlock()
	// Group names are read after the registry lock, once per group.
	names := map[int64]string{}
	for i := range out {
		if id := out[i].GroupID; id != 0 {
			if _, ok := names[id]; !ok {
				names[id] = pageGroupName(id)
			}
			out[i].Group = names[id]
		}
	}
	return out
}

// DELETE /v1/pages/{name}[?owner=<agent>] — close one of the caller's own
// pages, or one shared with it (groups.pages.manage).
func handlePageClose(w http.ResponseWriter, r *http.Request) {
	p, group, caller, ok := pageForAs(w, r, PermGroupsPagesManage)
	if !ok {
		return
	}
	if _, ok := livePages.closeByName(p.owner, p.name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "the page is already closed")
		return
	}
	if group == nil {
		setAuditTargetLabel(r, "page "+p.name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Another member closed it: its shower is told, as when the operator
	// closes a tab. The body names only the page and the closer's id.
	if to := humanActionLiveConv(p.owner); to != "" {
		from, _ := db.CurrentConvForAgent(caller)
		body := fmt.Sprintf("Agent %s, a member of group %s, closed your live page %q. Show it again with `tclaude agent show <target> --name %s` if it is still needed.",
			caller, group.Name, p.name, p.name)
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: from, ToConv: to, Subject: "[page " + p.name + "] closed", Body: body,
			ToRecipients: []string{to},
		}); err != nil {
			slog.Warn("pages: tell the shower its page was closed", "page", p.name, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- dashboard ----

// livePagesSnapshot is the dashboard's view of every open page. It carries the
// page origins, which only the local dashboard may see (peer projections deny
// the field).
func livePagesSnapshot() []pageView {
	return pageViews(livePages.list(""), (*livePage).view)
}

// livePagesSnapshotFor is livePagesSnapshot for the dashboard asking: a
// remote dashboard (remote access) gets each page's remote stage address on
// its own host, never the loopback origin and its token.
func livePagesSnapshotFor(r *http.Request) []pageView {
	if !dashboardViewerIsRemote(r) {
		return livePagesSnapshot()
	}
	return pageViews(livePages.list(""), func(p *livePage) pageView {
		v := p.view()
		v.Origin, v.URL = "", remotePageURL(p, r.Host)
		return v
	})
}

// DELETE /api/pages/{id} — the operator closed a page's tab. The page closes
// and its agent is told through its inbox.
func handleDashboardPageClose(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	p, ok := livePages.closeByID(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	setAuditTargetLabel(r, "page "+p.name)
	if convID := humanActionLiveConv(p.owner); convID != "" {
		// Operator-authored: it quotes nothing but the agent's own page name
		// (charset-checked), not the agent-written title.
		body := fmt.Sprintf("The operator closed your live page %q. Show it again with `tclaude agent show <target> --name %s` if it is still needed.",
			p.name, p.name)
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			ToConv: convID, Subject: "[page " + p.name + "] closed", Body: body,
			ToRecipients: []string{convID}, OperatorAuthored: true,
		}); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"closed": true, "told_agent": false, "error": "failed to queue message: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"closed": true, "told_agent": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": true, "told_agent": false})
}
