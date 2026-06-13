package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboard_prefs.go backs the browser dashboard's "sticky" view/config
// preferences — group expand/collapse, per-tab filters and toggles, the
// sort state, the spawn-modal auto-focus checkbox, and the per-model
// spawn-effort memory.
//
// These lived in the browser's localStorage, but the dashboard is served
// on a RANDOM loopback port each daemon start and localStorage is
// partitioned by origin (scheme+host+port) — so every such setting
// silently reset on restart. They now persist server-side in SQLite
// (dashboard_prefs, migration v56), reached via this endpoint, the same
// way the slop volume sliders moved to config.json. Dashboard-only,
// like /api/slop/volumes: there is no /v1 twin — these are the human's
// own browser-view config, no business of an agent.
//
// The values are opaque strings the dashboard owns ('1'/'0', filter
// text, a JSON blob); the daemon stores and returns them verbatim.

// maxPrefValueLen caps a single stored value. The real prefs are a
// handful of bytes; this is a sanity ceiling against a buggy/abusive
// client wedging a megabyte into a "sticky checkbox".
const maxPrefValueLen = 64 * 1024

// maxPrefKeyLen caps a key. The dashboard's keys are short namespaced
// strings ("tclaude.dash.group.<name>"); group names are bounded well
// under this.
const maxPrefKeyLen = 512

// prefsBody is the POST wire shape. Value is a pointer so the client can
// distinguish "store this string" (incl. "") from "delete this key":
//   - {"key":"k","value":"v"} → upsert (mirrors localStorage.setItem)
//   - {"key":"k","value":null} or {"key":"k"} → delete (removeItem)
type prefsBody struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

// handleDashboardPrefsAPI dispatches /api/dashboard/prefs by method.
// Mounted on the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardPrefsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		prefs, err := db.ListDashboardPrefs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		// Always emit an object (never null) so the client can spread it
		// straight into its cache.
		if prefs == nil {
			prefs = map[string]string{}
		}
		writeJSON(w, http.StatusOK, prefs)
	case http.MethodPost:
		var body prefsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON: "+err.Error())
			return
		}
		if body.Key == "" {
			writeError(w, http.StatusBadRequest, "empty_key", "key is required")
			return
		}
		if len(body.Key) > maxPrefKeyLen {
			writeError(w, http.StatusBadRequest, "key_too_long", "key exceeds the size limit")
			return
		}
		if body.Value == nil {
			if err := db.DeleteDashboardPref(body.Key); err != nil {
				writeError(w, http.StatusInternalServerError, "db", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if len(*body.Value) > maxPrefValueLen {
			writeError(w, http.StatusBadRequest, "value_too_long", "value exceeds the size limit")
			return
		}
		if err := db.SetDashboardPref(body.Key, *body.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}
