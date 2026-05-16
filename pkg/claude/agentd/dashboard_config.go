package agentd

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"

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
//
// Schema scope: the editor round-trips the fields of config.Config.
// A JSON key absent from that struct is dropped by the typed decode —
// this is pre-existing config.Load behaviour (every Load+Save caller
// drops them), not introduced here. The GET handler surfaces any such
// keys as "unknown_keys" so the human is warned before a save removes
// them, rather than the loss being silent.

// configPostBody is the POST wire shape. Config carries the edited
// config; Base is the canonical baseline the dashboard diffed against,
// used for the drift guard (see handleDashboardConfigPost).
type configPostBody struct {
	Config *config.Config `json:"config"`
	Base   string         `json:"base"`
}

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

// configErrors writes the {"errors":[...]} body the editor expects for
// a 400 — one shape for every rejected save, whether the failure is a
// malformed body or a validation problem.
func configErrors(w http.ResponseWriter, msgs ...string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"errors": msgs})
}

// handleDashboardConfigGet returns the current effective config as
// canonical (2-space-indented) JSON plus the file path. config.Load
// applies defaults for missing sections, so a never-configured machine
// still gets a fully-populated form to edit.
//
// "warning" appears when the file on disk is malformed (Load fell back
// to defaults). "unknown_keys" lists top-level keys present in the file
// but absent from the config.Config schema — a save would drop them, so
// the human is told up front.
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
	if unknown := unknownConfigKeys(); len(unknown) > 0 {
		resp["unknown_keys"] = unknown
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboardConfigPost validates the submitted config and either
// previews it (dry_run=1) or persists it.
//
// Drift guard: the dashboard sends, in Base, the canonical config it
// diffed against at load time. If the file changed since then (another
// dashboard action, a hand-edit), the human's approved diff is stale —
// the request is refused with 409 so a blind overwrite with a stale
// baseline cannot happen. Base is empty only for callers that opted
// out; the dashboard always sends it.
func handleDashboardConfigPost(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "1"

	var body configPostBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		configErrors(w, "request body is not valid JSON: "+err.Error())
		return
	}
	if body.Config == nil {
		configErrors(w, `request body is missing the "config" object`)
		return
	}
	cfg := body.Config

	if body.Base != "" {
		current, _ := config.Load()
		if curRaw, err := json.MarshalIndent(current, "", "  "); err == nil && string(curRaw) != body.Base {
			writeError(w, http.StatusConflict, "config_drift",
				"config.json changed on disk since the Config tab loaded it — reload the tab and re-apply your edits")
			return
		}
	}

	// Validate the raw submission first — Validate rejects an
	// out-of-range value so the human is told, where Normalize would
	// silently clamp it.
	if errs := config.Validate(cfg); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": errs})
		return
	}

	// Normalize to the same canonical shape Load produces, so the
	// dry-run preview, the bytes written to disk, and the next GET all
	// match exactly — config.Save persists this normalized form.
	config.Normalize(cfg)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		http.Error(w, "marshal config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{"raw": string(raw)})
		return
	}

	if err := config.Save(cfg); err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"raw":  string(raw),
		"path": config.ConfigPath(),
	})
}

// unknownConfigKeys returns the top-level keys present in the config
// file on disk but absent from the config.Config schema — keys a save
// through the typed decode would drop. Empty when the file is missing,
// unparseable, or fully schema-conformant.
func unknownConfigKeys() []string {
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		return nil
	}
	var fileMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &fileMap); err != nil {
		return nil
	}
	known := configSchemaKeys()
	var unknown []string
	for k := range fileMap {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// configSchemaKeys is the set of top-level JSON keys config.Config
// models, derived by reflection so it never drifts from the struct.
func configSchemaKeys() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(config.Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}
