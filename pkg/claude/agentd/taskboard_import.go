package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Ref import (tcl-mjlf51, docs/taskboard.md): `taskboard add --from-awb /
// --from-linear / --from-github` and `--lines-file`. One request carries a
// batch of refs or lines; each item becomes one task, or is refused on its
// own without failing the rest.
//
// A ref's title is looked up through the tracker proxies AS THE CALLER: the
// lookup is a sub-request to the proxy's own read route (awb issue show,
// Linear issue view, GitHub issue or pr view), carrying the caller's identity
// through the same audit and permission middleware a direct proxy call takes.
// Every gate those routes enforce (proxy.*.read, the operator's workspace /
// team / remote allow-lists, scoped grants) applies unchanged and lands in the
// audit log as the caller's own proxy read. When the lookup is refused or
// fails, the id becomes the title and the item's note says why.

// maxTaskboardImportItems bounds one batch. The board is meant to hold about
// ten tasks; fifty leaves room for a burst without inviting a backlog dump.
const maxTaskboardImportItems = 50

// taskboardImportLookupBudget bounds the time all of one import's title
// lookups may take together. The CLI waits longer than this.
const taskboardImportLookupBudget = 60 * time.Second

// taskboardImportPeerLookupBudget is the budget for a linked node's import,
// which must answer within the peer exchange's own timeout.
const taskboardImportPeerLookupBudget = 8 * time.Second

// taskboardImportMu serialises the duplicate check and the creates of an
// import, so two concurrent imports cannot both add the same ref. Lookups run
// before it is taken.
var taskboardImportMu sync.Mutex

// taskboardLookupHandler serves the proxy read routes a ref lookup calls,
// behind the audit middleware, so each lookup records an audit row exactly as
// the same read made through `tclaude proxy` would.
var taskboardLookupHandler = sync.OnceValue(func() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/awb/issue/show", handleAWBProxyIssueShow)
	mux.HandleFunc("POST /v1/linear/issue/view", handleLinearProxyIssueView)
	mux.HandleFunc("POST /v1/github/issue/view", handleGHProxyIssueView)
	mux.HandleFunc("POST /v1/github/pr/view", handleGHProxyPRView)
	return auditRequests(countSudoUses(mux))
})

// taskboardImportItem is one prepared item of a batch.
type taskboardImportItem struct {
	input    string
	title    string // a plain line's title; empty for a ref
	kind     string
	refURL   string
	refLabel string
}

// taskboardImportResult is one item's outcome on the wire.
type taskboardImportResult struct {
	Input string             `json:"input"`
	Task  *taskboardTaskView `json:"task,omitempty"`
	// Resolved is set for a ref whose title came from its tracker.
	Resolved bool `json:"resolved,omitempty"`
	// Note says why a ref's id became its title.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// prepareTaskboardImport turns refs and lines into items. A ref that does not
// parse, and a line that is neither a title nor a ref, are refused per item.
func prepareTaskboardImport(refs, lines []string) ([]taskboardImportItem, []taskboardImportResult) {
	items := []taskboardImportItem{}
	results := []taskboardImportResult{}
	add := func(input string, item taskboardImportItem, err error) {
		if err != nil {
			results = append(results, taskboardImportResult{Input: guideText(input, 80), Error: err.Error(), Code: "invalid_arg"})
			items = append(items, taskboardImportItem{input: input})
			return
		}
		item.input = input
		items = append(items, item)
		results = append(results, taskboardImportResult{Input: input})
	}
	for _, raw := range refs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		kind, refURL, label, err := parseTaskRef(raw, "")
		add(raw, taskboardImportItem{kind: kind, refURL: refURL, refLabel: label}, err)
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A line that is a single token and parses as a ref becomes that ref;
		// anything else is a title.
		if !strings.ContainsAny(line, " \t") {
			if kind, refURL, label, err := parseTaskRef(line, ""); err == nil {
				add(line, taskboardImportItem{kind: kind, refURL: refURL, refLabel: label}, nil)
				continue
			}
		}
		title, err := validateTaskTitle(line)
		add(line, taskboardImportItem{title: title}, err)
	}
	return items, results
}

// taskboardRefKey identifies a ref for the duplicate check. A GitHub issue or
// pull request is keyed by repository and number, so the owner/repo#n
// shorthand and its URL collide. An awb or Linear ref is keyed by its tracker
// id: read from its URL when it has one (its label may be custom), else its
// label, which is the id unless set by hand. Anything else, a plain URL
// included, is keyed by its whole URL: a URL's derived label is only its host.
func taskboardRefKey(kind, refURL, label string) string {
	switch kind {
	case "":
		return ""
	case "github":
		if owner, repo, n, _, ok := githubRefParts(refURL); ok {
			return fmt.Sprintf("github\x00%s/%s#%d", strings.ToLower(owner), strings.ToLower(repo), n)
		}
	case "awb", "linear":
		id := label
		if refURL != "" {
			if derived := deriveTaskLabel(refURL); derived != "" {
				id = derived
			}
		}
		if id != "" {
			return kind + "\x00" + strings.ToLower(id)
		}
	}
	return kind + "\x00" + strings.TrimRight(strings.ToLower(refURL), "/")
}

// handleTaskboardImport creates one task per ref or line. The answer is 200
// with a result per item in input order; created counts the tasks added.
func handleTaskboardImport(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, refs, lines []string, stage string) {
	items, results := prepareTaskboardImport(refs, lines)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "nothing to import: give refs or non-empty lines")
		return
	}
	if len(items) > maxTaskboardImportItems {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("an import holds at most %d items; the board is for the next few tasks", maxTaskboardImportItems))
		return
	}

	// Resolve titles first, outside the lock: each lookup may reach a tracker.
	// The lookups share one time budget, so a batch answers in bounded time;
	// a ref past it keeps its id as the title.
	budget := taskboardImportLookupBudget
	if taskboardPeerAuthor(r) != "" {
		// A linked node waits fedPeerViewTimeout for the whole exchange.
		budget = taskboardImportPeerLookupBudget
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	lookupReq := r.WithContext(ctx)
	for i := range items {
		it := &items[i]
		if results[i].Error != "" || it.title != "" {
			continue
		}
		if ctx.Err() != nil {
			it.title = taskboardRefFallbackTitle(it.kind, it.refURL, it.refLabel)
			results[i].Note = "not looked up: the import's lookup time ran out"
			continue
		}
		title, refURL, note := lookupTaskboardRef(lookupReq, it.kind, it.refURL, it.refLabel)
		if note == "" {
			results[i].Resolved = true
		} else {
			results[i].Note = note
		}
		if it.refURL == "" {
			it.refURL = refURL
		}
		it.title = title
	}

	taskboardImportMu.Lock()
	defer taskboardImportMu.Unlock()
	live, err := db.ListGroupTasks(g.ID, db.GroupTaskListOpts{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	seen := map[string]string{}
	for _, t := range live {
		if k := taskboardRefKey(t.RefKind, t.RefURL, t.RefLabel); k != "" {
			seen[k] = t.ID
		}
	}
	actor := taskboardActor(r)
	created := 0
	for i, it := range items {
		if results[i].Error != "" {
			continue
		}
		if k := taskboardRefKey(it.kind, it.refURL, it.refLabel); k != "" {
			if id, dup := seen[k]; dup {
				results[i].Error = fmt.Sprintf("%s is already on the board as %s", it.refLabel, id)
				results[i].Code = "duplicate"
				results[i].Note = ""
				results[i].Resolved = false
				continue
			}
		}
		t, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: it.title, Stage: stage,
			RefKind: it.kind, RefURL: it.refURL, RefLabel: it.refLabel, Actor: actor, Imported: it.kind != ""})
		if err != nil {
			results[i].Error, results[i].Code = taskboardErrorText(err)
			results[i].Note, results[i].Resolved = "", false
			continue
		}
		if k := taskboardRefKey(it.kind, it.refURL, it.refLabel); k != "" {
			seen[k] = t.ID
		}
		if views, err := taskboardViews(g.Name, []*db.GroupTask{t}); err == nil {
			results[i].Task = &views[0]
		} else {
			// Created all the same: report the task without its derived fields.
			results[i].Task = &taskboardTaskView{ID: t.ID, Group: g.Name, Title: t.Title, Stage: t.Stage,
				Status: t.Status, DependsOn: []string{}}
		}
		created++
	}
	setAuditDetail(r, fmt.Sprintf("imported %d of %d into %s", created, len(items), g.Name))
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "created": created, "results": results})
}

// taskboardErrorText is writeTaskboardError's code and message for one item.
func taskboardErrorText(err error) (msg, code string) {
	rec := httptest.NewRecorder()
	writeTaskboardError(rec, err)
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &body) == nil && body.Error != "" {
		return body.Error, body.Code
	}
	return err.Error(), "db"
}

// taskboardRefFallbackTitle is the title of a ref whose lookup failed: the
// tracker id, owner/repo#n for GitHub, or the whole URL.
func taskboardRefFallbackTitle(kind, refURL, label string) string {
	switch kind {
	case "awb", "linear":
		if label != "" {
			return sanitizeTaskTitle(label)
		}
	case "github":
		if owner, repo, n, _, ok := githubRefParts(refURL); ok {
			return sanitizeTaskTitle(fmt.Sprintf("%s/%s#%d", owner, repo, n))
		}
	}
	return sanitizeTaskTitle(firstNonEmpty(refURL, label))
}

// lookupTaskboardRef finds a ref's title through its tracker's proxy. On any
// refusal or failure it returns the fallback title and a note.
func lookupTaskboardRef(r *http.Request, kind, refURL, label string) (title, resolvedURL, note string) {
	fallback := taskboardRefFallbackTitle(kind, refURL, label)
	cfg, _ := config.Load()
	var (
		path string
		body any
		repo string // github: the owner/repo the ref names
	)
	switch kind {
	case "awb":
		if !cfg.AWBProxyEnabled() {
			return fallback, "", "no awb proxy is configured"
		}
		path, body = "/v1/awb/issue/show", map[string]any{"id": label}
		if cfg.Agent != nil && cfg.Agent.AWBProxy != nil {
			if base, fault := validateAWBBaseURL(cfg.Agent.AWBProxy.URL); fault == nil {
				resolvedURL = base + "/#/issues/" + strings.ToLower(label)
			}
		}
	case "linear":
		if !cfg.LinearProxyConfigured() && strings.TrimSpace(os.Getenv("LINEAR_API_KEY")) == "" {
			return fallback, "", "no Linear proxy is configured"
		}
		path, body = "/v1/linear/issue/view", map[string]any{"identifier": label}
	case "github":
		owner, name, n, pull, ok := githubRefParts(refURL)
		if !ok {
			return fallback, "", "not a GitHub issue or pull request URL"
		}
		if !cfg.GitProxyEnabled() {
			return fallback, "", "no GitHub proxy is configured"
		}
		repo = owner + "/" + name
		path, body = "/v1/github/issue/view", map[string]any{"number": n}
		if pull {
			path = "/v1/github/pr/view"
		}
		t, u, note := readTaskboardRefTitle(r, kind, path, body, repo, fallback, resolvedURL)
		// owner/repo#n names an issue or a pull request alike; try the pull
		// request when no issue has the number.
		if note != "" && !pull && strings.Contains(note, "no issue") {
			return readTaskboardRefTitle(r, kind, "/v1/github/pr/view", body, repo, fallback, resolvedURL)
		}
		return t, u, note
	default:
		return fallback, "", "a plain URL is not looked up"
	}
	return readTaskboardRefTitle(r, kind, path, body, repo, fallback, resolvedURL)
}

// resolveTaskboardRefLink is a single add's resolve_ref: a bare awb or Linear
// id (no link yet) is looked up as the caller, within an import's time budget,
// and takes the link the lookup gives. Any other ref, or a lookup that gives
// no link, keeps refURL. The title is the caller's, so the looked-up one is
// not used.
func resolveTaskboardRefLink(r *http.Request, kind, refURL, label string) string {
	if refURL != "" || (kind != "awb" && kind != "linear") {
		return refURL
	}
	budget := taskboardImportLookupBudget
	if taskboardPeerAuthor(r) != "" {
		budget = taskboardImportPeerLookupBudget
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	if _, resolved, _ := lookupTaskboardRef(r.WithContext(ctx), kind, refURL, label); resolved != "" {
		return resolved
	}
	return refURL
}

// readTaskboardRefTitle runs one proxy read and takes the title from it.
func readTaskboardRefTitle(r *http.Request, kind, path string, body any, repo, fallback, resolvedURL string) (title, outURL, note string) {
	status, payload, errText := taskboardProxyRead(r, path, body)
	if status != http.StatusOK {
		return fallback, resolvedURL, "lookup refused: " + guideText(errText, 160)
	}
	var out struct {
		JSON     json.RawMessage `json:"json"`
		Repo     string          `json:"repo"`
		ExitCode int             `json:"exit_code"`
		Stderr   string          `json:"stderr"`
	}
	if err := json.Unmarshal(payload, &out); err != nil || len(out.JSON) == 0 {
		msg := "the proxy returned no issue"
		if out.Stderr != "" {
			msg = "lookup failed: " + guideText(out.Stderr, 160)
		}
		if repo != "" && out.Repo != "" && !strings.EqualFold(out.Repo, repo) {
			msg = fmt.Sprintf("the GitHub proxy reaches %s from your repository, not %s", out.Repo, repo)
		}
		return fallback, resolvedURL, msg
	}
	if repo != "" && !strings.EqualFold(out.Repo, repo) {
		return fallback, resolvedURL, fmt.Sprintf("the GitHub proxy reaches %s from your repository, not %s", out.Repo, repo)
	}
	var issue struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if json.Unmarshal(out.JSON, &issue) != nil {
		return fallback, resolvedURL, "the proxy returned no issue"
	}
	t, err := validateTaskTitle(sanitizeTaskTitle(issue.Title))
	if err != nil {
		return fallback, resolvedURL, "the tracker's title is not usable: " + err.Error()
	}
	if resolvedURL == "" && kind == "linear" && validateTaskRefURL(issue.URL) == nil {
		resolvedURL = issue.URL
	}
	return t, resolvedURL, ""
}

// sanitizeTaskTitle makes a tracker's title fit a task's: forbidden runes
// become spaces, and it is cut to the byte bound on a rune boundary.
func sanitizeTaskTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if taskTextForbidden(r, false) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > maxTaskTitleBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}

// githubRefParts reads owner, repo and number from a GitHub issue or pull
// request URL.
func githubRefParts(raw string) (owner, repo string, n int, pull, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), "github.com") {
		return "", "", 0, false, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return "", "", 0, false, false
	}
	n, err = strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, false, false
	}
	return parts[0], parts[1], n, parts[2] == "pull", true
}

// taskboardProxyRead runs one proxy read as the caller of r and returns its
// status, body, and the error message of a refusal.
func taskboardProxyRead(r *http.Request, path string, body any) (int, []byte, string) {
	raw, err := json.Marshal(body)
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	// The caller's identity rides on the context (the socket peer) and, for a
	// dashboard caller, on its headers. The audit result of the import itself
	// must not be the one the lookup writes into: the lookup handler installs
	// its own.
	sub := r.Clone(r.Context())
	sub.Method = http.MethodPost
	sub.URL = &url.URL{Path: path}
	sub.RequestURI = ""
	sub.Body = httpNoopCloser{bytes.NewReader(raw)}
	sub.ContentLength = int64(len(raw))
	sub.Header.Set("Content-Type", "application/json")
	// One --ask-human popup per looked-up ref would be noise: the human was
	// asked about the import, and a refused lookup only costs the title.
	sub.Header.Del("X-Tclaude-Ask-Human")
	clearAuthorizedPermission(sub)
	rec := httptest.NewRecorder()
	taskboardLookupHandler().ServeHTTP(rec, sub)
	if rec.Code == http.StatusOK {
		return rec.Code, rec.Body.Bytes(), ""
	}
	var fail struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fail)
	return rec.Code, nil, firstNonEmpty(fail.Error, http.StatusText(rec.Code))
}

type httpNoopCloser struct{ *bytes.Reader }

func (httpNoopCloser) Close() error { return nil }
