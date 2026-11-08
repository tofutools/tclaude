package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A task's refs (tk_01ad0884): a task may link to several things at once,
// such as its tracker issue and the pull request that does it. The first
// ref is "the" ref every reader of one ref knows (ref, ref_label,
// --ref); add_refs and rm_refs change the list.

// taskRefOf is a stored ref as a parsed one.
func taskRefOf(kind, refURL, label string) db.GroupTaskRef {
	return db.GroupTaskRef{Kind: kind, URL: refURL, Label: label}
}

// sameTaskRef reports whether a and b name the same thing: the same link,
// or the same tracker issue or pull request however it was written.
func sameTaskRef(a, b db.GroupTaskRef) bool {
	if ka, kb := taskboardRefKey(a.Kind, a.URL, a.Label), taskboardRefKey(b.Kind, b.URL, b.Label); ka != "" && ka == kb {
		return true
	}
	if a.URL != "" && a.URL == b.URL {
		return true
	}
	return a.URL == "" && b.URL == "" && a.Kind == b.Kind && strings.EqualFold(a.Label, b.Label) && a.Label != ""
}

// parseTaskRefs reads the refs to add, each as --ref reads one (resolve:
// a bare awb or Linear id is linked through its tracker, as resolve_ref
// does).
func parseTaskRefs(r *http.Request, raws []string, resolve bool) ([]db.GroupTaskRef, error) {
	out := make([]db.GroupTaskRef, 0, len(raws))
	for _, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		kind, refURL, label, err := parseTaskRef(raw, "")
		if err != nil {
			return nil, err
		}
		if resolve {
			refURL = resolveTaskboardRefLink(r, kind, refURL, label)
		}
		out = append(out, taskRefOf(kind, refURL, label))
	}
	return out, nil
}

// addTaskRefs appends refs, leaving out one the list already has, and
// refuses a list past db.MaxGroupTaskRefs.
func addTaskRefs(list, add []db.GroupTaskRef) ([]db.GroupTaskRef, error) {
	out := append([]db.GroupTaskRef(nil), list...)
	for _, ref := range add {
		dup := false
		for _, have := range out {
			if sameTaskRef(have, ref) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, ref)
		}
	}
	if len(out) > db.MaxGroupTaskRefs {
		return nil, fmt.Errorf("a task has at most %d refs", db.MaxGroupTaskRefs)
	}
	return out, nil
}

// removeTaskRefs takes refs off the list, each named by its position (#2,
// counting from 1), its link, its label, or as --ref would read it. A name
// that matches none is refused: the caller meant something that is not there.
func removeTaskRefs(list []db.GroupTaskRef, names []string) ([]db.GroupTaskRef, error) {
	drop := make([]bool, len(list))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		hit := -1
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "#")); err == nil && strings.HasPrefix(name, "#") {
			if n >= 1 && n <= len(list) {
				hit = n - 1
			}
		} else {
			parsed, perr := db.GroupTaskRef{}, error(nil)
			if kind, refURL, label, err := parseTaskRef(name, ""); err == nil {
				parsed = taskRefOf(kind, refURL, label)
			} else {
				perr = err
			}
			for i, ref := range list {
				if ref.URL == name || strings.EqualFold(ref.Label, name) || (perr == nil && sameTaskRef(ref, parsed)) {
					hit = i
					break
				}
			}
		}
		if hit < 0 {
			return nil, fmt.Errorf("the task has no ref %q (name one by #position, link or label)", name)
		}
		drop[hit] = true
	}
	out := make([]db.GroupTaskRef, 0, len(list))
	for i, ref := range list {
		if !drop[i] {
			out = append(out, ref)
		}
	}
	return out, nil
}

// setTaskRefs makes edit write list as the task's refs: the first in the
// ref columns, the rest as more_refs. EditGroupTask writes only what
// differs from the task.
func setTaskRefs(edit *db.GroupTaskEdit, list []db.GroupTaskRef) {
	var first db.GroupTaskRef
	var more []db.GroupTaskRef
	if len(list) > 0 {
		first, more = list[0], list[1:]
	}
	edit.RefKind, edit.RefURL, edit.RefLabel = &first.Kind, &first.URL, &first.Label
	if more == nil {
		more = []db.GroupTaskRef{}
	}
	edit.MoreRefs = &more
}

// editRefsOf is the task's refs as an edit has made them so far: its own,
// with the first replaced by what the edit sets on it.
func editRefsOf(t *db.GroupTask, edit db.GroupTaskEdit) []db.GroupTaskRef {
	first := taskRefOf(t.RefKind, t.RefURL, t.RefLabel)
	if edit.RefKind != nil {
		first.Kind = *edit.RefKind
	}
	if edit.RefURL != nil {
		first.URL = *edit.RefURL
	}
	if edit.RefLabel != nil {
		first.Label = *edit.RefLabel
	}
	var list []db.GroupTaskRef
	if first.Kind != "" || first.URL != "" || first.Label != "" {
		list = append(list, first)
	}
	return append(list, t.MoreRefs...)
}

// withAddedTaskRefs is a new task's refs: its ref (kind, refURL, label;
// none when kind is empty) and the refs added after it, the first of them
// the task's ref when it gives none.
func withAddedTaskRefs(r *http.Request, kind, refURL, label string, addRefs []string, resolve bool) (string, string, string, []db.GroupTaskRef, error) {
	var list []db.GroupTaskRef
	if kind != "" {
		list = append(list, taskRefOf(kind, refURL, label))
	}
	add, err := parseTaskRefs(r, addRefs, resolve)
	if err == nil {
		list, err = addTaskRefs(list, add)
	}
	if err != nil || len(list) == 0 {
		return kind, refURL, label, nil, err
	}
	return list[0].Kind, list[0].URL, list[0].Label, list[1:], nil
}

// taskboardRefPR is a pull request ref's status, the agent rows' PR shape
// (presentedPRView): its number, state and title, its CI summary and its
// reviews. Each is what the cache holds, and absent when it holds none.
type taskboardRefPR struct {
	Number  int              `json:"number,omitempty"`
	State   string           `json:"state,omitempty"`
	Title   string           `json:"title,omitempty"`
	Checks  *prChecksSummary `json:"checks,omitempty"`
	Reviews *prReviewSummary `json:"reviews,omitempty"`
}

// taskRefPRNumber is the number of a GitHub pull request or GitLab merge
// request URL; ok is false for any other link.
func taskRefPRNumber(rawURL string) (int, bool) {
	if ref, ok := githubPRRefFromURL(rawURL); ok {
		return ref.number, true
	}
	if ref, ok := gitlabMRRefFromURL(rawURL); ok {
		return ref.number, true
	}
	return 0, false
}

// stampTaskRefStatus gives every ref in views what tclaude last read about
// it. A pull request's status comes from the caches the agent rows read
// (prStateKey identity, one batch read each), so a task's PR shows what an
// agent's shows; a tracker issue's title and status come from its last
// lookup. It reads only: the forge reads the agent rows, presented PRs and
// the hover cards already make, and the lookups an add, an import or the
// dashboard's ref lookup make, fill those caches. A board read adds none.
func stampTaskRefStatus(views []taskboardTaskView) {
	stampTaskRefTrackers(views)
	var urls []string
	for i := range views {
		for _, ref := range views[i].Refs {
			if _, ok := taskRefPRNumber(ref.URL); ok {
				urls = append(urls, ref.URL)
			}
		}
	}
	if len(urls) == 0 {
		return
	}
	states, titles := cachedPresentedPRMetadata(urls)
	checks := prChecksIndexFor(urls)
	reviews := prReviewsIndexFor(urls)
	for i := range views {
		for _, ref := range views[i].Refs {
			n, ok := taskRefPRNumber(ref.URL)
			if !ok {
				continue
			}
			key := prStateKey(ref.URL)
			ref.PR = &taskboardRefPR{Number: n, State: states[key].state, Title: titles[key],
				Checks: checks[key], Reviews: reviews[key]}
		}
	}
}

// taskboardRefTracker is what a tracker said about a ref's issue at its
// last lookup: its title and status, and when.
type taskboardRefTracker struct {
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

// taskRefTrackerCacheKey is the git cache key of a ref's issue, "" for a
// ref that names no tracker issue (a plain link).
func taskRefTrackerCacheKey(kind, refURL, label string) string {
	k := taskboardRefKey(kind, refURL, label)
	if k == "" {
		return ""
	}
	h := sha256.Sum256([]byte("task-ref\x00" + k))
	return "tref_" + hex.EncodeToString(h[:8])
}

// saveTaskRefTracker keeps what a lookup found about a ref's issue.
func saveTaskRefTracker(kind, refURL, label, title, status string) {
	key := taskRefTrackerCacheKey(kind, refURL, label)
	if key == "" || (title == "" && status == "") {
		return
	}
	now := time.Now()
	data, err := json.Marshal(taskboardRefTracker{Title: title, Status: status, FetchedAt: rfc3339(now)})
	if err != nil {
		return
	}
	if err := db.SaveGitCache(key, data, now); err != nil {
		slog.Warn("taskboard: failed to keep a ref's lookup", "error", err, "kind", kind, "module", "agentd")
	}
}

// stampTaskRefTrackers gives every tracker ref in views (not a pull request)
// its issue's title and status from its last lookup, in one batch read.
func stampTaskRefTrackers(views []taskboardTaskView) {
	keyOf := map[*taskboardRef]string{}
	var keys []string
	for i := range views {
		for _, ref := range views[i].Refs {
			if _, pr := taskRefPRNumber(ref.URL); pr {
				continue
			}
			label := ref.Label
			if ref.ID != "" {
				label = ref.ID
			}
			if key := taskRefTrackerCacheKey(ref.Kind, ref.URL, label); key != "" {
				keyOf[ref] = key
				keys = append(keys, key)
			}
		}
	}
	if len(keys) == 0 {
		return
	}
	rows, err := db.LoadGitCacheBatch(keys)
	if err != nil {
		return
	}
	for ref, key := range keyOf {
		row := rows[key]
		if row == nil {
			continue
		}
		var info taskboardRefTracker
		if json.Unmarshal(row.Data, &info) == nil {
			ref.Tracker = &info
		}
	}
}
