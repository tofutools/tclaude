package agentd

import (
	"encoding/json"
	"fmt"
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
// to defaults). "unknown_keys" lists keys present in the file but
// absent from the config.Config schema — at any nesting depth, as
// dotted paths — that a save would drop, so the human is told up front.
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

// unknownConfigKeys walks the on-disk config file and returns the
// dotted paths of every key absent from the config.Config schema, at
// any nesting depth — keys a save through the typed decode would drop.
// Empty when the file is missing, unparseable, or fully conformant.
func unknownConfigKeys() []string {
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		return nil
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil
	}
	var unknown []string
	collectUnknownKeys("", tree, reflect.TypeOf(config.Config{}), &unknown)
	sort.Strings(unknown)
	return unknown
}

// collectUnknownKeys walks one JSON object against the struct type that
// models it: every key with no matching struct field is appended (as a
// dotted path), and the value of every known key is descended into.
func collectUnknownKeys(prefix string, node map[string]any, t reflect.Type, out *[]string) {
	fieldType := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fieldType[name] = t.Field(i).Type
		}
	}
	for key, val := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		ft, ok := fieldType[key]
		if !ok {
			*out = append(*out, path)
			continue
		}
		descendUnknownKeys(path, val, ft, out)
	}
}

// descendUnknownKeys recurses into a known field's value: into a nested
// struct's keys, into each element of a slice, into each value of a map
// (whose own keys are arbitrary by design, e.g. agent.sudo.overrides).
// Scalars and type mismatches bottom out.
func descendUnknownKeys(path string, val any, ft reflect.Type, out *[]string) {
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	switch ft.Kind() {
	case reflect.Struct:
		if obj, ok := val.(map[string]any); ok {
			collectUnknownKeys(path, obj, ft, out)
		}
	case reflect.Map:
		if obj, ok := val.(map[string]any); ok {
			for mk, mv := range obj {
				descendUnknownKeys(path+"."+mk, mv, ft.Elem(), out)
			}
		}
	case reflect.Slice, reflect.Array:
		if arr, ok := val.([]any); ok {
			for i, item := range arr {
				descendUnknownKeys(fmt.Sprintf("%s[%d]", path, i), item, ft.Elem(), out)
			}
		}
	}
}
