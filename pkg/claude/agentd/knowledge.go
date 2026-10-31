package agentd

import (
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/docs"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/knowledge"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// The knowledge base (`tclaude agent knowledge`) gives a Steward the docs and
// source of the build this daemon is running. It is served rather than mounted:
// the store stays behind the wall around ~/.tclaude/data and these routes only
// read, so a reader can change nothing, under any harness or sandbox, and no
// launch needs a new path.

const PermKnowledgeRead = "knowledge.read"

// knowledgeBase is the daemon's base, built on first use.
var knowledgeBase = sync.OnceValue(func() *knowledge.Base {
	return knowledge.New(knowledge.CurrentBuild(buildversion.AppVersion()), docs.FS, filepath.Join(config.DataDir(), "knowledge"))
})

// SetKnowledgeBaseForTest swaps the base and returns the restore.
func SetKnowledgeBaseForTest(base *knowledge.Base) func() {
	previous := knowledgeBase
	knowledgeBase = func() *knowledge.Base { return base }
	return func() { knowledgeBase = previous }
}

// knowledgeReply carries the pin on every answer, so a dirty or unknown build
// can never read as exact.
type knowledgeReply struct {
	Label  string                 `json:"label"`
	State  knowledge.BuildState   `json:"state"`
	Source knowledge.SourceStatus `json:"source"`
}

func newKnowledgeReply() knowledgeReply {
	base := knowledgeBase()
	base.Prefetch()
	status := base.Status()
	return knowledgeReply{Label: status.Label, State: status.Build.State, Source: status.Source}
}

func handleKnowledgeStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermKnowledgeRead); !ok {
		return
	}
	base := knowledgeBase()
	base.Prefetch()
	writeJSON(w, http.StatusOK, base.Status())
}

func handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermKnowledgeRead); !ok {
		return
	}
	entries, err := knowledgeBase().List(r.URL.Query().Get("path"))
	if err != nil {
		writeKnowledgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		knowledgeReply
		Entries []knowledge.Entry `json:"entries"`
	}{newKnowledgeReply(), entries})
}

func handleKnowledgeRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermKnowledgeRead); !ok {
		return
	}
	query := r.URL.Query()
	from, to, ok := knowledgeLines(query.Get("lines"))
	if !ok {
		writeError(w, http.StatusBadRequest, "lines", "lines must be N or N-M, counted from 1")
		return
	}
	page, err := knowledgeBase().Read(knowledge.ReadRequest{Path: query.Get("path"), From: from, To: to})
	if err != nil {
		writeKnowledgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		knowledgeReply
		*knowledge.Page
	}{newKnowledgeReply(), page})
}

// knowledgeLines reads "N" (from there on) or "N-M". Empty means the whole file.
func knowledgeLines(lines string) (from, to int, ok bool) {
	if lines == "" {
		return 0, 0, true
	}
	first, last, ranged := strings.Cut(lines, "-")
	from, err := strconv.Atoi(first)
	if err != nil || from < 1 {
		return 0, 0, false
	}
	if !ranged {
		return from, 0, true
	}
	to, err = strconv.Atoi(last)
	if err != nil || to < from {
		return 0, 0, false
	}
	return from, to, true
}

func handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermKnowledgeRead); !ok {
		return
	}
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "limit", "limit must be a positive number")
			return
		}
		limit = parsed
	}
	result, err := knowledgeBase().Search(r.Context(), knowledge.SearchRequest{
		Pattern: query.Get("q"), Path: query.Get("path"), IgnoreCase: query.Get("i") == "1", Limit: limit,
	})
	if err != nil {
		writeKnowledgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		knowledgeReply
		*knowledge.SearchResult
	}{newKnowledgeReply(), result})
}

func writeKnowledgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, knowledge.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	}
}
