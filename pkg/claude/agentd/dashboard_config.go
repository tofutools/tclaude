package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/stewardtools"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
// A JSON key absent from that struct is invisible to the form, but
// config.Save carries it over from the file it replaces, so a save keeps
// it. The exception is a key inside an array element, which has no stable
// identity to carry it by: the GET handler surfaces those as
// "unknown_keys" so the human is warned before a save removes them.

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
// to defaults). "unknown_keys" lists keys present in the file but
// absent from the config.Config schema that a save would drop (those
// inside array elements), as dotted paths, so the human is told up front.
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
		resp["malformed"] = true
		resp["warning"] = "the config file could not be parsed (" + loadErr.Error() +
			") — the form shows defaults, NOT your current settings; saving will replace the corrupt file entirely"
	}
	if unknown := unknownConfigKeys(); len(unknown) > 0 {
		resp["unknown_keys"] = unknown
	}
	addExternalStewardInfo(resp, cfg)
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
//
// Malformed-target guard: if the file on disk is corrupt, the editor is
// showing defaults (not the file's unparseable real contents), so a
// save would silently discard whatever the corrupt file held. A real
// write is refused with 409 unless replace_malformed=1 explicitly
// acknowledges the wipe — the dashboard asks the human first.
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

	current, loadErr := config.Load()
	if loadErr != nil {
		// The file on disk is corrupt. A dry-run only previews, so let it
		// through to validate the form; a real write must be acknowledged.
		if !dryRun && r.URL.Query().Get("replace_malformed") != "1" {
			writeError(w, http.StatusConflict, "malformed_target",
				"config.json on disk is corrupt and cannot be parsed — the editor is showing defaults, not your current settings. Saving will replace the corrupt file entirely; re-send with replace_malformed=1 to do that deliberately.")
			return
		}
	} else if body.Base != "" {
		// Drift guard only applies to a parseable baseline.
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

	if name, named := config.AskSandboxProfileName(cfg.AskSandboxSetting()); named {
		if _, err := db.ResolveNamedSandboxSnapshot(name); err != nil {
			configErrors(w, fmt.Sprintf("ask.sandbox profile %q is unavailable: %v; choose an existing library profile, auto, global, or off", name, err))
			return
		}
	}

	// Normalize to the same canonical shape Load produces, so the
	// dry-run preview, the bytes written to disk, and the next GET all
	// match exactly — config.Save persists this normalized form.
	config.Normalize(cfg)
	// The form shows every per-type checkbox, so an unchecked one is a
	// choice `tclaude setup` must not undo.
	cfg.Notifications.MarkNotifyTypesKnown()
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

	// Apply the Claude Code transcript-retention override (claude_cleanup_period_days
	// → ~/.claude/settings.json cleanupPeriodDays) right now, so a value set in the
	// Config tab takes effect immediately rather than only on the next session
	// start. No-op when the override is unset; a failure here is logged inside and
	// must not fail the save the human already confirmed.
	_ = session.SyncClaudeCleanupPeriod(cfg)

	if loadErr == nil {
		tellStewardsNewName(current, cfg)
	}
	if len(stewardConvs()) > 0 {
		autoFetchKnowledgeSource(cfg)
	}

	resp := map[string]any{
		"raw":  string(raw),
		"path": config.ConfigPath(),
	}
	addExternalStewardInfo(resp, cfg)
	writeJSON(w, http.StatusOK, resp)
}

// addExternalStewardInfo is what the Config page shows of external stewards, in
// the load and in the save, which replaces it: the tool list has to be there
// before the feature can be turned on.
func addExternalStewardInfo(resp map[string]any, cfg *config.Config) {
	if !cfg.CollabEnabled() {
		return
	}
	resp["external_steward"] = map[string]any{
		"enabled": cfg.ExternalStewardsEnabled(),
		"tools":   stewardtools.List(),
	}
}

// unknownConfigKeys returns the dotted paths of the on-disk config's keys
// that the config.Config schema does not model AND a save would drop —
// config.Save carries every other unmodeled key over (see
// config.UnknownConfigKeys). Empty when the file is missing, unparseable, or
// has nothing a save would lose.
func unknownConfigKeys() []string {
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		return nil
	}
	_, dropped := config.UnknownConfigKeys(data)
	return dropped
}
