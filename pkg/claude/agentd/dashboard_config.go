package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// dashboard_config.go backs the dashboard's "Config" tab — the visual
// editor for ~/.tclaude/config.json. It is the cookie-authed,
// human-only twin of hand-editing the file: GET reads the current
// config, POST validates and (with dry_run) previews or persists it.
//
// The save flow is deliberately two-phase so the browser can show a
// diff-confirmation before anything touches disk:
//
//   1. POST /api/config?dry_run=1 — validate the submitted config and,
//      if it is clean, return its canonical marshalled form WITHOUT
//      writing. The dashboard diffs this against the GET baseline and
//      pops a confirmation modal.
//   2. POST /api/config — the human confirmed; validate again and
//      persist via config.Save.
//
// Both the GET baseline and the dry-run preview are produced by the
// same json.MarshalIndent of a config.Config, so the diff the human
// sees is exactly the byte-level change config.Save will write — no
// JS-side / Go-side canonicalisation can drift apart.

// handleDashboardConfigAPI dispatches /api/config by method. Mounted on
// the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardConfigAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleDashboardConfigGet(w, r)
	case http.MethodPost:
		handleDashboardConfigPost(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleDashboardConfigGet returns the current effective config as
// canonical (2-space-indented) JSON plus the file path. config.Load
// applies defaults for missing sections, so a never-configured machine
// still gets a fully-populated form to edit.
//
// If the file on disk is malformed, Load falls back to defaults and
// reports an error — we surface that as a "warning" so the editor can
// tell the human their broken file is being shown as defaults (and a
// save will replace it with a clean one) rather than masking it.
func handleDashboardConfigGet(w http.ResponseWriter, _ *http.Request) {
	cfg, loadErr := config.Load()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		http.Error(w, "marshal config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"raw":  string(raw),
		"path": config.ConfigPath(),
	}
	if loadErr != nil {
		resp["warning"] = "the config file could not be parsed (" + loadErr.Error() +
			") — showing defaults; saving will replace the file with a clean config"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboardConfigPost validates the submitted config and either
// previews it (dry_run=1) or persists it. Validation errors come back
// as 400 with an {"errors":[...]} body so the editor can list every
// problem; a clean config comes back as 200 with the canonical "raw"
// the editor diffs / re-syncs against.
func handleDashboardConfigPost(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "1"

	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "decode config: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the raw submission first — Validate rejects an
	// out-of-range value so the human is told, where Normalize would
	// silently clamp it.
	if errs := config.Validate(&cfg); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": errs})
		return
	}

	// Normalize to the same canonical shape Load produces, so the
	// dry-run preview, the bytes written to disk, and the next GET all
	// match exactly — config.Save persists this normalized form.
	config.Normalize(&cfg)
	raw, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		http.Error(w, "marshal config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{"raw": string(raw)})
		return
	}

	if err := config.Save(&cfg); err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"raw":  string(raw),
		"path": config.ConfigPath(),
	})
}
