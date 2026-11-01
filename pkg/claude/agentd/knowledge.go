package agentd

import (
	"encoding/json"
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
//
// The source is a download only the operator can agree to. The routes that do
// it are on the dashboard, with no agent path, and steward.source_download in
// Config holds the answer.

const PermKnowledgeRead = "knowledge.read"

// knowledgeBase is the daemon's base, built on first use.
var knowledgeBase = sync.OnceValue(func() *knowledge.Base {
	return knowledge.New(knowledge.CurrentBuild(buildversion.AppVersion()), docs.FS, filepath.Join(config.DataDir(), "knowledge"))
})

// knowledgeStartup is the store's tidying at daemon start, which goes on behind
// it so a start never waits on the disk or a download.
var knowledgeStartup sync.WaitGroup

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
	status := knowledgeBase().Status()
	return knowledgeReply{Label: status.Label, State: status.Build.State, Source: status.Source}
}

func handleKnowledgeStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermKnowledgeRead); !ok {
		return
	}
	writeJSON(w, http.StatusOK, knowledgeBase().Status())
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

// knowledgeSourceView is what the operator's question, progress bar and Config
// row are drawn from.
type knowledgeSourceView struct {
	knowledge.SourceStatus
	// Ask says to put the question to the operator: a Steward could use the
	// source, this version has none, and their choice leaves it open.
	Ask           bool   `json:"ask"`
	Policy        string `json:"policy"`
	Version       string `json:"version"`
	Origin        string `json:"origin"`
	DownloadBytes int64  `json:"download_bytes"`
	StoredBytes   int64  `json:"stored_bytes"`
}

func newKnowledgeSourceView(cfg *config.Config) knowledgeSourceView {
	status := knowledgeBase().Status()
	policy := cfg.StewardSourceDownload()
	return knowledgeSourceView{
		SourceStatus:  status.Source,
		Ask:           policy == config.StewardSourceAsk && status.Source.Detail == knowledge.SourceNotDownloaded && len(stewardConvs()) > 0,
		Policy:        policy,
		Version:       status.Build.Version,
		Origin:        knowledge.SourceOrigin,
		DownloadBytes: knowledge.ApproxDownloadBytes,
		StoredBytes:   knowledge.ApproxStoredBytes,
	}
}

// autoFetchKnowledgeSource starts a download the operator has already agreed to
// for every version. It never blocks: progress and failure show in the dashboard.
func autoFetchKnowledgeSource(cfg *config.Config) {
	if cfg.CollabEnabled() && cfg.StewardSourceDownload() == config.StewardSourceEachVersion {
		knowledgeBase().StartFetch()
	}
}

// GET /api/knowledge/source: the live progress the question's card polls.
func handleDashboardKnowledgeSourceGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, newKnowledgeSourceView(cfg))
}

// POST /api/knowledge/source: the operator's answer. "once" is also how a failed
// download is tried again. "remove" drops the copy and stops downloads, so the
// question doesn't come straight back.
func handleDashboardKnowledgeSourcePost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "request body is not valid JSON")
		return
	}
	policy := ""
	switch body.Action {
	case "once":
	case "each_version":
		policy = config.StewardSourceEachVersion
	case "never":
		policy = config.StewardSourceNever
	case "remove":
		policy = config.StewardSourceNever
	default:
		writeError(w, http.StatusBadRequest, "action", "action must be once, each_version, never or remove")
		return
	}
	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if policy != "" {
			if cfg.Steward == nil {
				cfg.Steward = &config.StewardConfig{}
			}
			cfg.Steward.SourceDownload = policy
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	switch body.Action {
	case "once", "each_version":
		knowledgeBase().StartFetch()
	case "remove":
		// After the save: a snapshot in between would see no source and a choice
		// still open, and put the question back.
		if err := knowledgeBase().Remove(); err != nil {
			writeKnowledgeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, newKnowledgeSourceView(cfg))
}

func writeKnowledgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, knowledge.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, knowledge.ErrFetching):
		writeError(w, http.StatusConflict, "fetching", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	}
}
