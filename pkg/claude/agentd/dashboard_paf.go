package agentd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// Dashboard twins of the PAF routes (export, import, redact, publish,
// snapshots). The browser
// cannot reach the daemon socket, so each /api route checks the dashboard
// cookie and hands the shared /v1 handler a synthetic human caller; the
// handlers keep their own permission checks. None is served to peer view.
func registerDashboardPAFRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent-bundle/export", dashboardPAFRoute(handleAgentBundleExport))
	mux.HandleFunc("POST /api/agent-bundle/import", dashboardPAFRoute(handleAgentBundleImport))
	mux.HandleFunc("POST /api/import/reveal", dashboardPAFRoute(handleImportReveal))
	mux.HandleFunc("POST /api/paf/redact", dashboardPAFRoute(handlePAFRedact))
	mux.HandleFunc("POST /api/paf/publish", dashboardPAFRoute(handlePAFPublish))

	// The snapshot library (4d) and the resilience policy status (5a,
	// read-only here: setting a policy stays on the CLI for now).
	mux.HandleFunc("GET /api/snapshots", dashboardPAFRoute(handleSnapshotList))
	mux.HandleFunc("POST /api/snapshots", dashboardPAFRoute(handleSnapshotCreate))
	mux.HandleFunc("POST /api/snapshots/prune", dashboardPAFRoute(handleSnapshotPrune))
	mux.HandleFunc("GET /api/snapshots/{name}", dashboardPAFRoute(handleSnapshotShow))
	mux.HandleFunc("DELETE /api/snapshots/{name}", dashboardPAFRoute(handleSnapshotRemove))
	mux.HandleFunc("GET /api/snapshots/{name}/export", dashboardPAFRoute(handleSnapshotExport))
	mux.HandleFunc("POST /api/snapshots/{name}/spawn", dashboardPAFRoute(handleSnapshotSpawn))
	mux.HandleFunc("GET /api/resilience/policies", dashboardPAFRoute(handleResiliencePolicies))
}

func dashboardPAFRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, asDashboardHumanPeer(r))
	}
}

// handlePAFRedact previews, or writes, a redacted copy of an uploaded PAF.
//
//	POST /api/paf/redact[?preview=true][&mask_flagged=true][&stub_tool_results=true&keep_turns=N][&drop_media=true][&drop_reasoning=true][&paths_to_placeholders=true]
//
// It is `tclaude paf redact` for the dashboard: with preview=true it
// returns the redaction report (counts and kinds, never content) and the
// scan of the redacted output; otherwise it returns the redacted PAF,
// signed by this node when it has a federation identity. Human-only.
func handlePAFRedact(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "redact a PAF") {
		return
	}
	q := r.URL.Query()
	opts := privacy.RedactOptions{
		MaskFlagged: q.Get("mask_flagged") == "true", StubToolResults: q.Get("stub_tool_results") == "true",
		KeepTurns: 3, DropMedia: q.Get("drop_media") == "true", DropReasoning: q.Get("drop_reasoning") == "true",
		PathsToPlaceholders: q.Get("paths_to_placeholders") == "true", Native: decodeNativeHistory,
	}
	if v := q.Get("keep_turns"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, 400, "keep_turns", "keep_turns must be a non-negative number")
			return
		}
		opts.KeepTurns = n
	}
	if !opts.MaskFlagged && !opts.StubToolResults && !opts.DropMedia && !opts.DropReasoning && !opts.PathsToPlaceholders {
		writeError(w, 400, "redact", "choose at least one redaction")
		return
	}
	limits := paf.DefaultLimits()
	in, err := os.CreateTemp("", ".paf-redact-in-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = in.Close(); _ = os.Remove(in.Name()) }()
	n, err := io.Copy(in, http.MaxBytesReader(w, r.Body, limits.MaxArchiveBytes+1))
	if err != nil || n > limits.MaxArchiveBytes {
		writeError(w, 400, "archive", fmt.Sprintf("archive exceeds or cannot be read within %d bytes", limits.MaxArchiveBytes))
		return
	}
	// Rewriting needs every required feature, as import does.
	a, err := paf.Open(in, n, paf.Options{Purpose: paf.PurposeImport, Limits: limits})
	if err != nil {
		writeError(w, 400, "archive", err.Error())
		return
	}
	producer := paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}
	if q.Get("preview") == "true" {
		rep, err := privacy.Redact(a, io.Discard, opts, producer)
		if err != nil {
			writeError(w, 422, "redact", err.Error())
			return
		}
		writeJSON(w, 200, rep)
		return
	}
	var keys []paf.SigningKey
	// Signed only when this node already has a federation identity;
	// redacting never creates one.
	if id, err := proto.LoadIdentity(filepath.Join(config.DataDir(), "federation", "instance.key")); err == nil {
		keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	out, err := os.CreateTemp("", ".paf-redact-out-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	if _, err := privacy.Redact(a, out, opts, producer, keys...); err != nil {
		writeError(w, 422, "redact", err.Error())
		return
	}
	size, err := out.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = out.Seek(0, io.SeekStart)
	}
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	w.Header().Set("Content-Type", paf.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, out)
}
